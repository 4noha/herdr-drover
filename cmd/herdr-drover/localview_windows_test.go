//go:build windows

package main

// localview の Windows 実 herdr e2e（鉄則: 合成で緑にしない）。
//
// herdr の Windows pane（ConPTY）の中で runLocalView を走らせ、別 pane を映す:
//   - pane A: 素の cmd.exe（映される側）
//   - pane B: このテストバイナリ自身を helper として起動し runLocalView(A)
// pane B へ pane.send_text で打鍵する＝ConPTY が pane B のコンソール入力に
// 変換する＝実ユーザーの打鍵と同じ経路で tty_windows.go の raw mode
// （VIRTUAL_TERMINAL_INPUT）を通る。検証:
//   1. 打鍵が A へ届き、A の出力が observe フレームとして B の画面に描かれる
//      （VT 出力が文字化けせず解釈されている＝B の pane.read にマーカー）
//   2. Ctrl+C は B（ビューア）を殺さず A へ 0x03 として届く（PROCESSED_INPUT 無効）
//   3. Ctrl-B q で detach＝helper が正常終了する

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/4noha/herdr-drover/internal/herdrapi"
)

// TestLocalViewHelperProcess は pane B の中で動く helper（直接は何もしない）。
func TestLocalViewHelperProcess(t *testing.T) {
	target := os.Getenv("HD_LV_TARGET")
	if target == "" {
		t.Skip("helper process only")
	}
	api := herdrapi.New(os.Getenv("HERDR_SOCKET_PATH"))
	if err := runLocalView(api, target, os.Getenv("HD_LV_HERDR"), os.Stdin, os.Stdout, os.Stderr); err != nil {
		t.Fatal(err)
	}
}

func TestLocalViewWindowsRealHerdr(t *testing.T) {
	sock := startHerdrForTest(t)
	herdrBin, err := exec.LookPath("herdr")
	if err != nil {
		t.Skip("herdr not installed")
	}
	api := herdrapi.New(sock)
	ws, err := api.WorkspaceCreate()
	if err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	// Windows はプロセスの cwd になっている dir を消せない＝TempDir 削除より先に
	// （Cleanup は LIFO）workspace を閉じて pane 内の cmd.exe を終わらせる。
	t.Cleanup(func() {
		_ = api.WorkspaceClose(ws.Workspace.WorkspaceID)
		waitPanesGone(api, 5*time.Second)
	})
	apply := func(label string, argv []string, env map[string]string) string {
		t.Helper()
		type pane struct {
			Type    string            `json:"type"`
			Cwd     string            `json:"cwd"`
			Command []string          `json:"command"`
			Env     map[string]string `json:"env,omitempty"`
		}
		raw, err := api.Call("layout.apply", struct {
			WorkspaceID string `json:"workspace_id"`
			TabLabel    string `json:"tab_label"`
			Focus       bool   `json:"focus"`
			Root        pane   `json:"root"`
		}{ws.Workspace.WorkspaceID, label, false, pane{"pane", cwd, argv, env}})
		if err != nil {
			t.Fatalf("layout.apply %s: %v", label, err)
		}
		var out struct {
			Layout struct {
				Root struct {
					PaneID string `json:"pane_id"`
				} `json:"root"`
			} `json:"layout"`
		}
		if err := json.Unmarshal(raw, &out); err != nil || out.Layout.Root.PaneID == "" {
			t.Fatalf("layout.apply %s decode: %v %s", label, err, raw)
		}
		return out.Layout.Root.PaneID
	}
	paneA := apply("target", []string{os.Getenv("COMSPEC"), "/Q", "/D"}, nil)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	paneB := apply("viewer", []string{self, "-test.run=^TestLocalViewHelperProcess$", "-test.v"}, map[string]string{
		"HD_LV_TARGET":      paneA,
		"HD_LV_HERDR":       herdrBin,
		"HERDR_SOCKET_PATH": sock,
	})
	read := func(p string) string {
		r, err := api.PaneRead(p, "visible")
		if err != nil {
			return ""
		}
		return r.Text
	}
	waitFor := func(what string, d time.Duration, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(d)
		for !cond() {
			if time.Now().After(deadline) {
				pi, _ := api.Call("pane.process_info", map[string]string{"pane_id": paneB})
				t.Fatalf("timeout: %s\n--- A ---\n%s\n--- B ---\n%s\n--- B process ---\n%s", what, read(paneA), read(paneB), pi)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	// helper の observe が A の初回フレームを B に描くまで（cmd のプロンプト＝cwd）。
	waitFor("B に A のフレームが描かれる", 20*time.Second, func() bool {
		return strings.Contains(read(paneB), ">")
	})

	// 1. B への打鍵 → A で実行 → A の出力が B に描かれる。^ 分割で echo 行と区別。
	if err := api.PaneSendText(paneB, "echo HD_LV_^OK\r"); err != nil {
		t.Fatal(err)
	}
	waitFor("A でマーカー出力", 15*time.Second, func() bool { return strings.Contains(read(paneA), "HD_LV_OK") })
	waitFor("B の画面にマーカー（VT 出力が解釈されている）", 15*time.Second, func() bool {
		return strings.Contains(read(paneB), "HD_LV_OK")
	})
	if b := read(paneB); strings.Contains(b, "[?2026") || strings.Contains(b, "\x1b") {
		t.Fatalf("B に生の ESC シーケンスが文字として出ている（VT 出力無効）:\n%s", b)
	}

	// 2. Ctrl+C はビューアを殺さず A へ届く（cmd は ^C を表示して新プロンプト）。
	if err := api.PaneSendText(paneB, "\x03"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	if err := api.PaneSendText(paneB, "echo HD_LV_^AFTER_CTRLC\r"); err != nil {
		t.Fatal(err)
	}
	waitFor("Ctrl+C 後もビューアが生きて入力を中継", 15*time.Second, func() bool {
		return strings.Contains(read(paneB), "HD_LV_AFTER_CTRLC")
	})

	// 3. Ctrl-B q で detach → helper が終了する。終了の判定は pane B の前景
	// プロセスに自分（テストバイナリ）が居なくなること（画面は alt-screen を
	// 抜けた直後で空になり得る＝文字列では判定しない）。
	if err := api.PaneSendText(paneB, "\x02q"); err != nil {
		t.Fatal(err)
	}
	base := strings.ToLower(filepathBase(self))
	waitFor("detach で helper 終了", 15*time.Second, func() bool {
		pi, err := api.PaneProcessInfo(paneB)
		if err != nil {
			return true // pane ごと閉じた＝終了
		}
		for _, p := range pi.ForegroundProcesses {
			if strings.ToLower(p.Name) == base {
				return false
			}
		}
		return true
	})
}

// waitPanesGone は全 pane が消える（pane 内プロセスが終わる）まで待つ。
func waitPanesGone(api *herdrapi.Client, d time.Duration) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if ps, err := api.PaneList(); err != nil || len(ps) == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func filepathBase(p string) string {
	if i := strings.LastIndexAny(p, `\/`); i >= 0 {
		return p[i+1:]
	}
	return p
}
