package bridge

import (
	"bytes"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/4noha/herdr-drover/internal/herdrapi"
)

func TestMetaFraming(t *testing.T) {
	got := string(MetaBegin(179, 70, true, true, 12))
	if got != "\x1b]7707;v=2;cols=179;rows=70;trunc=1;hist=1;scrolled=12\x1b\\" {
		t.Fatalf("MetaBegin: %q", got)
	}
	if got := string(MetaHistEnd()); got != "\x1b]7707;hist_end\x1b\\" {
		t.Fatalf("MetaHistEnd: %q", got)
	}
	// 2026 同期シムのマーカーと衝突しない
	if bytes.Contains(MetaBegin(1, 1, false, false, 0), []byte("\x1b[?2026")) {
		t.Fatal("META が DECSET 2026 を含む")
	}
}

// TestHistoryCacheBudget — 予算内に終わらない読取は ok=false で戻し、読取自体は
// 継続して完了後にキャッシュへ入る（次の fetch は即 ok）。読取は 1 回だけ。
func TestHistoryCacheBudget(t *testing.T) {
	c := NewHistoryCache()
	var calls atomic.Int32
	read := func() (*herdrapi.PaneReadInfo, error) {
		calls.Add(1)
		time.Sleep(300 * time.Millisecond)
		return &herdrapi.PaneReadInfo{Text: "a\nb\nc\n", Truncated: true}, nil
	}
	logf := func(string, ...any) {}
	t0 := time.Now()
	if _, ok := c.fetch("p", 50*time.Millisecond, read, logf); ok {
		t.Fatal("予算 50ms で 300ms の読取が ok になった")
	}
	if d := time.Since(t0); d > 200*time.Millisecond {
		t.Fatalf("予算超過なのに %s 待った", d)
	}
	// 進行中の読取には相乗り（2 本目を起動しない）
	if _, ok := c.fetch("p", 10*time.Millisecond, read, logf); ok {
		t.Fatal("まだ終わっていないのに ok")
	}
	time.Sleep(400 * time.Millisecond)
	e, ok := c.fetch("p", 10*time.Millisecond, read, logf)
	if !ok || e.text != "a\nb\nc\n" || !e.truncated {
		t.Fatalf("完了後のキャッシュが使われない: ok=%v e=%+v", ok, e)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("読取が %d 回（1 回のはず）", n)
	}
}

func TestHistoryPayload(t *testing.T) {
	rows := 3
	// 可視画面ぶん以下 → 何も書かない
	if p := HistoryPayload("a\nb\nc\n", rows); p != nil {
		t.Fatalf("可視画面ぶんだけなのに payload: %q", p)
	}
	if p := HistoryPayload("", rows); p != nil {
		t.Fatalf("空入力で payload: %q", p)
	}
	// 5 行（末尾 3 行＝可視画面）→ 2 行の履歴＋rows 行のパディング
	p := string(HistoryPayload("h1  \nh2\nv1\nv2\nv3\n", rows))
	want := "h1\r\nh2\r\n" + strings.Repeat("\r\n", rows)
	if p != want {
		t.Fatalf("payload=%q want=%q", p, want)
	}
	if strings.Contains(p, "\n\n") || strings.Count(p, "\n") != strings.Count(p, "\r\n") {
		t.Fatalf("LF が CRLF になっていない: %q", p)
	}
	// 末尾改行の無い本文でも同じ
	if q := string(HistoryPayload("h1\nh2\nv1\nv2\nv3", rows)); q != want {
		t.Fatalf("末尾改行なし: %q", q)
	}
}
