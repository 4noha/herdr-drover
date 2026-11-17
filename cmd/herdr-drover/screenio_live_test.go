package main

// screenio_live_test.go — send-input の「送信されたか」を**実 Claude Code** で検証する
// ライブ試験。隔離 herdr server（短い /tmp socket・XDG_CONFIG_HOME 隔離）に実 Claude
// Code の pane を立てた状態（`scripts/live-claude-pane.py hold` が作る live_pane.json）
// を環境変数 DROVER_LIVE_PANE_INFO で受け取り、newDoSendInput を本物の pane に打つ。
// 送信の判定は画面ではなく **Claude Code 自身の transcript（~/.claude/projects/
// <cwd>/<session>.jsonl）に user メッセージが載ったか**＝ゴーストサジェスチョンや
// 画面の残像に騙されない（memory: herdr-remote-pane-ghost-suggestion）。
//
// 環境変数が無ければ Skip（CI では走らない。実 API を使い課金が発生する）。
//
// 背景（2026-11-17）: Grok Bot（MCP）からの send_input が「文字は入るが送信されない」
// 事象。Claude Code 2.1.326 の usePasteHandler は 800 文字超の入力（または貼り付け
// 進行中）を paste として束ね、**100ms 静穏まで後続入力を同じ paste に吸う**
// （PASTE_COMPLETION_TIMEOUT_MS=100）。send_text 直後の Enter はこの窓に落ちて
// 改行になり、送信されない。

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/4noha/herdr-drover/internal/herdrapi"
)

type livePaneInfo struct {
	Sock string `json:"sock"`
	Pane string `json:"pane"`
	Work string `json:"work"`
	Proj string `json:"proj"`
}

func loadLivePane(t *testing.T) (livePaneInfo, *herdrapi.Client) {
	t.Helper()
	p := os.Getenv("DROVER_LIVE_PANE_INFO")
	if p == "" {
		t.Skip("DROVER_LIVE_PANE_INFO 未設定（実 Claude Code pane を使うライブ試験）")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("live pane info: %v", err)
	}
	var info livePaneInfo
	if err := json.Unmarshal(b, &info); err != nil {
		t.Fatalf("live pane info decode: %v", err)
	}
	if info.Proj == "" {
		info.Proj = filepath.Join(os.Getenv("HOME"), ".claude", "projects", strings.ReplaceAll(info.Work, "/", "-"))
	}
	return info, herdrapi.New(info.Sock)
}

// transcriptHas は proj 配下の transcript に、nonce を含む type==kind の行があるか。
func transcriptHas(proj, kind, nonce string) bool {
	files, _ := filepath.Glob(filepath.Join(proj, "*.jsonl"))
	needle := `"type":"` + kind + `"`
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 1<<20), 64<<20)
		for sc.Scan() {
			l := sc.Text()
			if strings.Contains(l, nonce) && strings.Contains(l, needle) {
				fh.Close()
				return true
			}
		}
		fh.Close()
	}
	return false
}

func waitUntil(d time.Duration, f func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if f() {
			return true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return false
}

func liveNonce() string {
	const cs = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, 6)
	for i := range b {
		b[i] = cs[rand.Intn(len(cs))]
	}
	return "ZQ" + string(b)
}

// liveBody は「nonce だけを返せ」という指示＋サイズ合わせの行（Grok Bot の実データ
// 538B 中央値・最大 5.5KB に合わせて 45B〜8KB を作る）。
func liveBody(nonce string, size int, multiline bool) string {
	head := "Reply with exactly this token and nothing else: " + nonce
	if size <= len(head) {
		return head
	}
	var sb strings.Builder
	sb.WriteString(head)
	if multiline {
		sb.WriteString("\n\nContext (ignore, do not act on it):\n")
		for i := 0; sb.Len() < size; i++ {
			fmt.Fprintf(&sb, "line %03d %s\n", i, strings.Repeat("x", 60))
		}
	} else {
		sb.WriteString(" Context (ignore): ")
		for sb.Len() < size {
			sb.WriteString("lorem ipsum ")
		}
	}
	return sb.String()[:size]
}

// clearComposer は未送信のまま残った入力を Ctrl-C で消す（Claude Code は入力が
// あるときの Ctrl-C 1 回で入力欄を空にする。空のときの 2 回連打は終了なので、
// nonce が画面に残っている間だけ打つ）。
func clearComposer(t *testing.T, hcli *herdrapi.Client, pane, nonce string) {
	t.Helper()
	for i := 0; i < 2; i++ {
		_ = hcli.PaneSendKeys(pane, "C-c")
		time.Sleep(1200 * time.Millisecond)
		rd, err := hcli.PaneRead(pane, "visible")
		if err != nil || !strings.Contains(rd.Text, nonce) {
			return
		}
	}
	t.Logf("入力欄の掃除に失敗（nonce %s が画面に残る）", nonce)
}

// afterTrial は送信済みなら応答完了まで待ち、未送信なら入力欄を掃除する。
func afterTrial(t *testing.T, hcli *herdrapi.Client, info livePaneInfo, nonce string, submitted bool) {
	t.Helper()
	if submitted {
		if !waitUntil(150*time.Second, func() bool { return transcriptHas(info.Proj, "assistant", nonce) }) {
			t.Logf("応答待ちタイムアウト（nonce %s）", nonce)
		}
		time.Sleep(1500 * time.Millisecond)
		return
	}
	clearComposer(t, hcli, info.Pane, nonce)
}

// TestLiveSendInputSubmits は newDoSendInput（daemon が send-input 命令で呼ぶ実体）
// を実 Claude Code pane に対して 45B〜8KB で打ち、全件が送信されることを要求する。
// 旧コード（send_text 直後に Enter）では 800 文字超が送信されず落ちる。
func TestLiveSendInputSubmits(t *testing.T) {
	info, hcli := loadLivePane(t)
	do := newDoSendInput(hcli)
	cases := []struct {
		name      string
		size      int
		multiline bool
	}{
		{"short-45B", 45, false},
		{"single-556B", 556, false},
		{"multi-600B", 600, true},
		{"multi-1.5KB", 1500, true},
		{"multi-5.5KB", 5500, true},
		{"multi-8KB", 8000, true},
	}
	future := time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339Nano)
	fails := 0
	for _, c := range cases {
		nonce := liveNonce()
		body := liveBody(nonce, c.size, c.multiline)
		t0 := time.Now()
		summary, err := do(context.Background(), info.Pane, map[string]string{
			"text": body, "expires_at": future, "require_status": "any",
		})
		if err != nil {
			t.Fatalf("%s: DoSendInput error: %v", c.name, err)
		}
		frag0 := fragmentCount(info.Proj)
		submitted := waitUntil(10*time.Second, func() bool { return transcriptHas(info.Proj, "user", nonce) })
		full := submitted && userMsgLen(info.Proj, nonce) >= len(body)
		frags := fragmentCount(info.Proj) - frag0
		t.Logf("%-12s %5dB submitted=%v full=%v fragments+%d (%s) summary=%q", c.name, len(body), submitted, full, frags, time.Since(t0).Round(100*time.Millisecond), summary)
		if !submitted || !full || frags != 0 {
			fails++
		}
		afterTrial(t, hcli, info, nonce, submitted)
	}
	if fails > 0 {
		t.Errorf("%d/%d 件が全文 1 メッセージで送信されなかった（Enter が paste 窓に吸われた／末尾の塊だけ別送）", fails, len(cases))
	}
}

// TestLiveSendInputDelayProbe は send_text → 待ち → Enter の待ち時間を変えて送信の
// 成否を測る（遅延定数の根拠取り。DROVER_LIVE_PROBE=1 のときだけ走る）。
func TestLiveSendInputDelayProbe(t *testing.T) {
	if os.Getenv("DROVER_LIVE_PROBE") == "" {
		t.Skip("DROVER_LIVE_PROBE 未設定")
	}
	info, hcli := loadLivePane(t)
	sizes := []int{1500, 5500, 8000}
	delays := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond}
	for _, size := range sizes {
		for _, d := range delays {
			nonce := liveNonce()
			body := liveBody(nonce, size, true)
			if err := hcli.PaneSendText(info.Pane, body); err != nil {
				t.Fatalf("send_text: %v", err)
			}
			time.Sleep(d)
			if err := hcli.PaneSendKeys(info.Pane, "enter"); err != nil {
				t.Fatalf("send_keys: %v", err)
			}
			submitted := waitUntil(10*time.Second, func() bool { return transcriptHas(info.Proj, "user", nonce) })
			t.Logf("size=%5dB delay=%-6s submitted=%v", len(body), d, submitted)
			afterTrial(t, hcli, info, nonce, submitted)
		}
	}
}

// userMsgLen は nonce を含む user メッセージの本文長（無ければ 0）。断片だけが
// 送信された（nonce を含まない user メッセージが増えた）かは fragmentCount で見る。
func userMsgLen(proj, nonce string) int {
	files, _ := filepath.Glob(filepath.Join(proj, "*.jsonl"))
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 1<<20), 64<<20)
		for sc.Scan() {
			l := sc.Text()
			if !strings.Contains(l, nonce) || !strings.Contains(l, `"type":"user"`) {
				continue
			}
			var d struct {
				Message struct {
					Content json.RawMessage `json:"content"`
				} `json:"message"`
			}
			if json.Unmarshal([]byte(l), &d) == nil {
				var s string
				if json.Unmarshal(d.Message.Content, &s) == nil {
					fh.Close()
					return len(s)
				}
				var arr []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				}
				if json.Unmarshal(d.Message.Content, &arr) == nil {
					n := 0
					for _, a := range arr {
						n += len(a.Text)
					}
					fh.Close()
					return n
				}
			}
			fh.Close()
			return -1
		}
		fh.Close()
	}
	return 0
}

func fragmentCount(proj string) int {
	files, _ := filepath.Glob(filepath.Join(proj, "*.jsonl"))
	n := 0
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 1<<20), 64<<20)
		for sc.Scan() {
			l := sc.Text()
			if strings.Contains(l, `"type":"user"`) && strings.Contains(l, "xxxxxxxx") && !strings.Contains(l, "ZQ") {
				n++
			}
		}
		fh.Close()
	}
	return n
}

// TestLiveSendInputBracketProbe は bracketed paste（ESC[200~ … ESC[201~）で包んだ
// send_text ＋ 待ち ＋ Enter の送信成否と、全文が 1 メッセージで届いたかを測る
// （DROVER_LIVE_PROBE2=1 のときだけ）。
func TestLiveSendInputBracketProbe(t *testing.T) {
	if os.Getenv("DROVER_LIVE_PROBE2") == "" {
		t.Skip("DROVER_LIVE_PROBE2 未設定")
	}
	info, hcli := loadLivePane(t)
	sizes := []int{45, 556, 1500, 5500, 8000}
	delays := []time.Duration{0, 150 * time.Millisecond, 300 * time.Millisecond}
	for _, size := range sizes {
		for _, d := range delays {
			nonce := liveNonce()
			body := liveBody(nonce, size, size > 600)
			frag0 := fragmentCount(info.Proj)
			if err := hcli.PaneSendText(info.Pane, "\x1b[200~"+body+"\x1b[201~"); err != nil {
				t.Fatalf("send_text: %v", err)
			}
			time.Sleep(d)
			if err := hcli.PaneSendKeys(info.Pane, "enter"); err != nil {
				t.Fatalf("send_keys: %v", err)
			}
			submitted := waitUntil(10*time.Second, func() bool { return transcriptHas(info.Proj, "user", nonce) })
			t.Logf("bracket size=%5dB delay=%-6s submitted=%v msglen=%d fragments+%d", len(body), d, submitted, userMsgLen(info.Proj, nonce), fragmentCount(info.Proj)-frag0)
			afterTrial(t, hcli, info, nonce, submitted)
		}
	}
}

func tailN(s string, n int) string {
	ls := []string{}
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			ls = append(ls, l)
		}
	}
	if len(ls) > n {
		ls = ls[len(ls)-n:]
	}
	return strings.Join(ls, "\n")
}

// TestLiveSendInputOtherPanes は bracketed paste が Claude Code 以外でも通ることを見る:
// 実 opencode の pane（DROVER_LIVE_OPENCODE_PANE）と素の zsh pane（DROVER_LIVE_SHELL_PANE）。
// zsh は 2 行の echo が**両方**実行されること（bracketed-paste-magic が多行を 1 バッファに
// 入れ、Enter で実行）、opencode は Ack detail に agent_status の idle→working が載り、
// 応答が画面に出ることを確認する。
func TestLiveSendInputOtherPanes(t *testing.T) {
	info, hcli := loadLivePane(t)
	_ = info
	do := newDoSendInput(hcli)
	future := time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339Nano)
	if sh := os.Getenv("DROVER_LIVE_SHELL_PANE"); sh != "" {
		nonce := liveNonce()
		text := "echo " + nonce + "-1\necho " + nonce + "-2"
		detail, err := do(context.Background(), sh, map[string]string{"text": text, "expires_at": future, "require_status": "any"})
		if err != nil {
			t.Fatalf("zsh: %v", err)
		}
		ok := waitUntil(6*time.Second, func() bool {
			rd, err := hcli.PaneRead(sh, "visible")
			if err != nil {
				return false
			}
			has1, has2 := false, false
			for _, l := range strings.Split(rd.Text, "\n") {
				switch strings.TrimSpace(l) {
				case nonce + "-1":
					has1 = true
				case nonce + "-2":
					has2 = true
				}
			}
			return has1 && has2
		})
		rd, _ := hcli.PaneRead(sh, "visible")
		t.Logf("zsh multi-line: both lines executed=%v detail=%q\n%s", ok, detail, tailN(rd.Text, 6))
		if !ok {
			t.Errorf("zsh: 2 行の echo が両方実行されなかった")
		}
	}
	if oc := os.Getenv("DROVER_LIVE_OPENCODE_PANE"); oc != "" {
		nonce := liveNonce()
		body := liveBody(nonce, 1500, true)
		detail, err := do(context.Background(), oc, map[string]string{"text": body, "expires_at": future})
		if err != nil {
			t.Fatalf("opencode: %v", err)
		}
		t.Logf("opencode 1.5KB: detail=%q", detail)
		if !strings.Contains(detail, "→working") {
			t.Errorf("opencode: 送信確認が取れない: %s", detail)
		}
		replied := waitUntil(120*time.Second, func() bool {
			rd, err := hcli.PaneRead(oc, "visible")
			return err == nil && strings.Count(rd.Text, nonce) >= 2
		})
		rd, _ := hcli.PaneRead(oc, "visible")
		t.Logf("opencode replied=%v screen tail:\n%s", replied, tailN(rd.Text, 14))
		if !replied {
			t.Errorf("opencode: 応答（nonce の再掲）が画面に出ない")
		}
	}
}
