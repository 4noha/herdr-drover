package main

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/4noha/herdr-drover/internal/herdrapi"
)

func TestTailLines(t *testing.T) {
	cases := []struct {
		name      string
		text      string
		n         int
		want      string
		truncated bool
	}{
		{"短い・切らない", "a\nb\nc", 5, "a\nb\nc", false},
		{"ちょうど", "a\nb\nc", 3, "a\nb\nc", false},
		{"末尾n行だけ", "a\nb\nc\nd", 2, "c\nd", true},
		{"1行", "only", 1, "only", false},
		{"空文字", "", 1, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, trunc := tailLines(c.text, c.n)
			if got != c.want || trunc != c.truncated {
				t.Fatalf("tailLines(%q,%d) = (%q,%v), want (%q,%v)", c.text, c.n, got, trunc, c.want, c.truncated)
			}
		})
	}
}

func TestTailBytesUTF8Safe(t *testing.T) {
	if got, cut := tailBytesUTF8Safe("hello", 10); got != "hello" || cut {
		t.Fatalf("短い入力で切られた: %q %v", got, cut)
	}
	if got, cut := tailBytesUTF8Safe("hello", 5); got != "hello" || cut {
		t.Fatalf("ちょうどの長さで切られた: %q %v", got, cut)
	}

	// マルチバイト文字（日本語）の3バイト境界をちょうど跨ぐケース。愚直な
	// byte slice だと「本」の途中で切れて無効な UTF-8 になる（修正前の実装が
	// 実際にこれで壊れることを確認済み）。
	text := strings.Repeat("a", 10) + "本" + strings.Repeat("b", 10) // 10+3+10=23 bytes
	got, cut := tailBytesUTF8Safe(text, 12)
	if !cut {
		t.Fatal("切られるはずなのに cut=false")
	}
	if !utf8.ValidString(got) {
		t.Fatalf("切った結果が無効な UTF-8: %q (% x)", got, []byte(got))
	}
	want := strings.Repeat("b", 10)
	if got != want {
		t.Fatalf("境界調整の結果が想定外: got=%q want=%q", got, want)
	}
}

// ---- send-input（screenio.go newDoSendInputWith）----
//
// 実機の根拠は screenio_live_test.go（実 Claude Code / Codex）。ここでは seam で
// 「何を・どの順で・どれだけ待って」herdr に打つかと、Ack detail の中身を固定する。

type fakeSendPane struct {
	status  string
	texts   []string
	keys    [][]string
	sleeps  []time.Duration
	clock   time.Time
	onEnter func(f *fakeSendPane)
	getErr  error
}

func (f *fakeSendPane) deps() sendInputDeps {
	return sendInputDeps{
		sendText: func(_ string, text string) error { f.texts = append(f.texts, text); return nil },
		sendKeys: func(_ string, keys ...string) error {
			f.keys = append(f.keys, keys)
			if len(keys) == 1 && keys[0] == "enter" && f.onEnter != nil {
				f.onEnter(f)
			}
			return nil
		},
		paneGet: func(string) (*herdrapi.PaneInfo, error) {
			if f.getErr != nil {
				return nil, f.getErr
			}
			return &herdrapi.PaneInfo{AgentStatus: f.status}, nil
		},
		sleep: func(d time.Duration) { f.sleeps = append(f.sleeps, d); f.clock = f.clock.Add(d) },
		now:   func() time.Time { return f.clock },
	}
}

func TestSendInputWrapsInBracketedPasteThenSettleThenEnter(t *testing.T) {
	f := &fakeSendPane{status: "idle", onEnter: func(f *fakeSendPane) { f.status = "working" }}
	do := newDoSendInputWith(f.deps())
	detail, err := do(context.Background(), "w1:p1", map[string]string{"text": "hello\nworld"})
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if len(f.texts) != 1 || f.texts[0] != bracketedPasteStart+"hello\nworld"+bracketedPasteEnd {
		t.Fatalf("send_text が bracketed paste で包まれていない: %q", f.texts)
	}
	if len(f.keys) != 1 || len(f.keys[0]) != 1 || f.keys[0][0] != "enter" {
		t.Fatalf("Enter が 1 回だけ別送されていない: %v", f.keys)
	}
	if len(f.sleeps) == 0 || f.sleeps[0] != submitSettle {
		t.Fatalf("send_text と Enter の間の待ち %v（期待 %v）", f.sleeps, submitSettle)
	}
	for _, want := range []string{"11 bytes", "submit=true", "bracketed-paste", "idle→working"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("detail に %q が無い: %s", want, detail)
		}
	}
}

func TestSendInputUnconfirmedDoesNotRetryEnter(t *testing.T) {
	// agent_status が idle のまま＝送信を確認できない。Enter は冪等でない（承認
	// ダイアログが出ていれば再送が承認になる）ので再送せず、detail に未確認と残す。
	f := &fakeSendPane{status: "idle"}
	do := newDoSendInputWith(f.deps())
	detail, err := do(context.Background(), "w1:p1", map[string]string{"text": "x"})
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if len(f.keys) != 1 {
		t.Fatalf("Enter が再送された: %v", f.keys)
	}
	if !strings.Contains(detail, "未確認") || !strings.Contains(detail, "agent_status=idle") {
		t.Fatalf("未確認が detail に無い: %s", detail)
	}
	var waited time.Duration
	for _, d := range f.sleeps[1:] { // [0] は settle
		waited += d
	}
	if waited < submitVerifyWait {
		t.Fatalf("確認待ちが %v しかない（期待 ≥ %v）", waited, submitVerifyWait)
	}
}

func TestSendInputRequireStatusDefaultAcceptsDoneRejectsWorking(t *testing.T) {
	for _, st := range []string{"idle", "blocked", "done"} {
		f := &fakeSendPane{status: st}
		if _, err := newDoSendInputWith(f.deps())(context.Background(), "w1:p1", map[string]string{"text": "x", "submit": "false"}); err != nil {
			t.Fatalf("agent_status=%s が既定で拒否された: %v", st, err)
		}
	}
	f := &fakeSendPane{status: "working"}
	_, err := newDoSendInputWith(f.deps())(context.Background(), "w1:p1", map[string]string{"text": "x"})
	if err == nil || !strings.Contains(err.Error(), "agent_status=working のため拒否") || !strings.Contains(err.Error(), sendInputRequireStatusDefault) {
		t.Fatalf("working が既定で拒否されない／理由が不明: %v", err)
	}
	if len(f.texts) != 0 || len(f.keys) != 0 {
		t.Fatalf("拒否したのに打鍵した: %v %v", f.texts, f.keys)
	}
}

func TestSendInputSubmitFalseSendsNoEnter(t *testing.T) {
	f := &fakeSendPane{status: "idle"}
	detail, err := newDoSendInputWith(f.deps())(context.Background(), "w1:p1", map[string]string{"text": "draft", "submit": "false"})
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if len(f.keys) != 0 || len(f.sleeps) != 0 {
		t.Fatalf("submit=false で Enter/待ちが発生: keys=%v sleeps=%v", f.keys, f.sleeps)
	}
	if !strings.Contains(detail, "submit=false") || !strings.Contains(detail, "bracketed-paste") {
		t.Fatalf("detail: %s", detail)
	}
}

func TestSendInputKeysPathIsRawKeys(t *testing.T) {
	f := &fakeSendPane{status: "blocked"}
	detail, err := newDoSendInputWith(f.deps())(context.Background(), "w1:p1", map[string]string{"keys": "Down,Enter"})
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if len(f.texts) != 0 || len(f.keys) != 1 || strings.Join(f.keys[0], ",") != "Down,Enter" {
		t.Fatalf("keys 経路: texts=%v keys=%v", f.texts, f.keys)
	}
	if !strings.Contains(detail, "キー入力完了（Down,Enter）") {
		t.Fatalf("detail: %s", detail)
	}
}

func TestSendInputVerifySkippedWhenBeforeIsWorking(t *testing.T) {
	f := &fakeSendPane{status: "working"}
	detail, err := newDoSendInputWith(f.deps())(context.Background(), "w1:p1", map[string]string{"text": "x", "require_status": "any"})
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if !strings.Contains(detail, "省略（入力前 agent_status=working）") {
		t.Fatalf("detail: %s", detail)
	}
	if len(f.sleeps) != 1 { // settle だけ。確認待ちで時間を使わない
		t.Fatalf("working 中に確認待ちをした: %v", f.sleeps)
	}
}

func TestSendInputStripsEmbeddedPasteMarkers(t *testing.T) {
	f := &fakeSendPane{status: "idle"}
	detail, err := newDoSendInputWith(f.deps())(context.Background(), "w1:p1", map[string]string{"text": "a" + bracketedPasteEnd + "b", "submit": "false"})
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if f.texts[0] != bracketedPasteStart+"ab"+bracketedPasteEnd {
		t.Fatalf("本文中の終端印が残っている: %q", f.texts[0])
	}
	if !strings.Contains(detail, "paste 印を除去") || !strings.Contains(detail, "2 bytes") {
		t.Fatalf("detail: %s", detail)
	}
}
