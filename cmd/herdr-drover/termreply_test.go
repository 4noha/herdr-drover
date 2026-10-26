package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/4noha/herdr-drover/internal/herdrapi"
)

// herdr 0.9.5 が実際に返した端末応答（2026-10-26・隔離 server で採取。
// ~/.cache/hd095/herdr095_term_replies.json と同じ値）。
var herdr095Replies = map[string]string{
	"DA1":       "\x1b[?62;22c", // ← mac-studio の素シェルに打ち込まれていたもの
	"DA2":       "\x1b[>1;0;0c",
	"DA3":       "\x1bP!|00000000\x1b\\",
	"DSR":       "\x1b[0n",
	"XTVERSION": "\x1bP>|libghostty\x1b\\",
	"OSC4":      "\x1b]4;0;rgb:1d1d/1f1f/2121\x1b\\",
	"DECRPM":    "\x1b[?2026;2$y",
	"KITTY":     "\x1b[?0u",
	"DECRQSS":   "\x1bP1$r0m\x1b\\",
}

// TestTermReplyFilterDropsRecordedReplies は採取した応答がすべて捨てられることを固定する
// （BUG-14）。⚠これが落ちると ↗窓 の向こうのシェルに `62;22c` 等が打ち込まれる。
func TestTermReplyFilterDropsRecordedReplies(t *testing.T) {
	for name, r := range herdr095Replies {
		var f termReplyFilter
		out, dropped := f.Filter([]byte(r))
		if len(out) != 0 {
			t.Errorf("%s: 応答が残った: %q", name, out)
		}
		if len(dropped) != 1 || string(dropped[0]) != r {
			t.Errorf("%s: 除去の記録が違う: %q", name, dropped)
		}
	}
}

// 打鍵・マウス・フォーカスなど「ユーザーの入力」は 1 バイトも変えずに通す。
func TestTermReplyFilterPassesUserInput(t *testing.T) {
	for name, in := range map[string]string{
		"文字":          "hello",
		"改行":          "\r",
		"単独 Esc":      "\x1b",
		"矢印 CSI":      "\x1b[A",
		"矢印 SS3":      "\x1bOA",
		"F5":          "\x1b[15~",
		"Ctrl+→":      "\x1b[1;5C",
		"修飾付き F3":     "\x1b[1;2R", // CPR 応答と同形＝通す側に倒す
		"CPR 形":       "\x1b[1;1R", // 同上（採取した CPR 応答そのもの）
		"SGR マウス":     "\x1b[<0;10;5M",
		"フォーカス in":    "\x1b[I",
		"フォーカス out":   "\x1b[O",
		"kitty キー事象":  "\x1b[97;5u",
		"Alt+]":       "\x1b]",
		"Alt+Shift+P": "\x1bP",
		"日本語":         "こんにちは",
	} {
		var f termReplyFilter
		out, dropped := f.Filter([]byte(in))
		if string(out) != in || len(dropped) != 0 {
			t.Errorf("%s: 入力が変わった: in=%q out=%q dropped=%q", name, in, out, dropped)
		}
	}
}

// 応答と打鍵が 1 回の Read に混ざっても、応答だけを抜く。
func TestTermReplyFilterMixed(t *testing.T) {
	var f termReplyFilter
	in := "a" + herdr095Replies["DA1"] + "b\x1b[A" + herdr095Replies["OSC4"] + "c"
	out, dropped := f.Filter([]byte(in))
	if string(out) != "ab\x1b[Ac" {
		t.Fatalf("out=%q", out)
	}
	if len(dropped) != 2 {
		t.Fatalf("dropped=%q", dropped)
	}
}

// 貼り付けの中身は、応答と同じ形の文字列でも手を付けない（境界が Read をまたいでも）。
func TestTermReplyFilterLeavesPasteAlone(t *testing.T) {
	var f termReplyFilter
	part1 := "\x1b[200~x" + herdr095Replies["DA1"]
	part2 := "y" + herdr095Replies["OSC4"] + "\x1b[201~"
	o1, d1 := f.Filter([]byte(part1))
	o2, d2 := f.Filter([]byte(part2))
	if string(o1)+string(o2) != part1+part2 || len(d1)+len(d2) != 0 {
		t.Fatalf("貼り付けが変わった: %q %q dropped=%q %q", o1, o2, d1, d2)
	}
	// 貼り付けが終われば、また応答を捨てる。
	o3, d3 := f.Filter([]byte(herdr095Replies["DA1"]))
	if len(o3) != 0 || len(d3) != 1 {
		t.Fatalf("貼り付け後に応答を捨てていない: out=%q", o3)
	}
}

// 途中で切れた列は保留せず、そのまま通す（保留すると単独 Esc 等が次の打鍵まで遅れる）。
func TestTermReplyFilterDoesNotHoldIncomplete(t *testing.T) {
	for _, in := range []string{"\x1b[?62;2", "\x1b]4;0;rgb:1d1d", "\x1bP>|libgh", "a\x1b["} {
		var f termReplyFilter
		out, dropped := f.Filter([]byte(in))
		if string(out) != in || len(dropped) != 0 {
			t.Errorf("途中の列を変えた／保留した: in=%q out=%q", in, out)
		}
	}
}

// ============ 実 herdr 0.9.5: attach の入力経路そのもの ============

// TestAttachInputHelperProcess は下の実 herdr テストが pane 内で起動する子プロセス
// （環境変数が無ければ何もしない）。stdin を raw にし、端末へ問い合わせを書いたうえで
// forwardInput（attach と同じ入力経路）に通し、転送された列を OUT に書く。
// 届いた生の列も RAW に書く（応答が本当に届いたことの物証）。
func TestAttachInputHelperProcess(t *testing.T) {
	outPath := os.Getenv("HD_ATTACH_INPUT_OUT")
	if outPath == "" {
		return
	}
	rawPath := os.Getenv("HD_ATTACH_INPUT_RAW")
	fd := int(os.Stdin.Fd())
	if _, err := enterRaw(fd); err != nil {
		os.WriteFile(outPath+".err", []byte(err.Error()), 0o644)
		os.Exit(2)
	}
	tee := &teeReader{r: os.Stdin, path: rawPath}
	var forwarded bytes.Buffer // forwardInput がリモートへ送った列（＝こちらの観測点）
	write := func(p []byte) error {
		forwarded.Write(p)
		return os.WriteFile(outPath, forwarded.Bytes(), 0o644)
	}
	// リモートの画面データに問い合わせが混ざった状況の再現（herdr が応答を返す）。
	os.Stdout.WriteString("\x1b[c\x1b[>c\x1b[=c\x1b[5n\x1b[>0q\x1b]4;0;?\x07\x1b[?2026$p\x1b[?u\x1bP$qm\x1b\\")
	os.WriteFile(outPath+".ready", nil, 0o644)
	go forwardInput(tee, write, nil)
	time.Sleep(20 * time.Second)
	os.Exit(0)
}

type teeReader struct {
	r    io.Reader
	path string
	buf  bytes.Buffer
}

func (t *teeReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 && t.path != "" {
		t.buf.Write(p[:n])
		_ = os.WriteFile(t.path, t.buf.Bytes(), 0o644)
	}
	return n, err
}

// TestAttachInputDropsHerdrTermRepliesRealHerdr は、実 herdr の pane 内で attach と同じ
// 入力経路を動かし、herdr が返した端末応答がリモートへ送られず、ユーザーの打鍵だけが
// 送られることを確かめる（BUG-14 の再現そのもの）。
func TestAttachInputDropsHerdrTermRepliesRealHerdr(t *testing.T) {
	sock := startHerdrForTest(t)
	api := herdrapi.New(sock)
	dir, err := os.MkdirTemp("/tmp", "hdin")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	outPath, rawPath := filepath.Join(dir, "out"), filepath.Join(dir, "raw")

	wsID, err := currentWorkspaceID(api)
	if err != nil {
		t.Fatalf("currentWorkspaceID: %v", err)
	}
	raw, err := api.Call("layout.apply", map[string]any{
		"workspace_id": wsID, "focus": false,
		"root": map[string]any{
			"type": "pane", "cwd": dir,
			"command": []string{os.Args[0], "-test.run=^TestAttachInputHelperProcess$"},
			"env":     map[string]string{"HD_ATTACH_INPUT_OUT": outPath, "HD_ATTACH_INPUT_RAW": rawPath},
		}})
	if err != nil {
		t.Fatalf("layout.apply: %v", err)
	}
	pane := decodeLayoutRootPane(t, raw)

	waitCond(t, 10*time.Second, "helper の準備", func() bool {
		_, e := os.Stat(outPath + ".ready")
		return e == nil
	})
	// herdr の応答が届くのを待つ（届いたことは RAW で確かめる）。
	waitCond(t, 10*time.Second, "herdr の端末応答が pane 入力に届く", func() bool {
		b, _ := os.ReadFile(rawPath)
		return bytes.Contains(b, []byte(herdr095Replies["DA1"]))
	})
	// ユーザーの打鍵（文字＋矢印）。
	if err := api.PaneSendText(pane, "hello\x1b[A"); err != nil {
		t.Fatalf("send_text: %v", err)
	}
	waitCond(t, 10*time.Second, "打鍵が転送される", func() bool {
		b, _ := os.ReadFile(outPath)
		return bytes.Contains(b, []byte("hello"))
	})
	got, _ := os.ReadFile(outPath)
	if !bytes.Contains(got, []byte("hello\x1b[A")) {
		t.Fatalf("打鍵がそのまま転送されていない: %q", got)
	}
	for name, r := range herdr095Replies {
		if bytes.Contains(got, []byte(r)) {
			t.Errorf("端末応答 %s がリモートへ転送された（リモートのシェルに打ち込まれる）: %q", name, r)
		}
	}
	if strings.Contains(string(got), "62;22c") {
		t.Errorf("mac-studio で観測された `62;22c` が転送されている: %q", got)
	}
}

func decodeLayoutRootPane(t *testing.T, raw []byte) string {
	t.Helper()
	var out struct {
		Layout struct {
			Root struct {
				PaneID string `json:"pane_id"`
			} `json:"root"`
		} `json:"layout"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Layout.Root.PaneID == "" {
		t.Fatalf("layout_apply decode: %v (%s)", err, raw)
	}
	return out.Layout.Root.PaneID
}
