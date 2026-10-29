package main

import (
	"context"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAPIErrorLine(t *testing.T) {
	yes := []string{
		"⏺ API Error: Server error mid-response. The response above may be incomplete.",
		"API Error: 529 {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\"}}",
		"API Error: Connection error.",
		"stream disconnected before completion: stream closed before response.completed",
		"API Error: 503 Service Unavailable",
	}
	for _, s := range yes {
		screen := "…作業の出力…\n" + s + "\n\n❯ \n──────\n  ⏵⏵ auto mode on"
		if apiErrorLine(screen, 14) == "" {
			t.Errorf("API エラーとして拾えない: %q", s)
		}
	}
	no := []string{
		"API Error: 429 rate limit exceeded. Resets in 2h", // 待っても効かない
		"You've hit your usage limit. Try again at 6 PM",   // 上限
		"通常の出力です。テストは全部通りました。",                             // エラーなし
		"ここで API Error という語を説明します。",                        // 文言の説明（形が違う）
	}
	for _, s := range no {
		if l := apiErrorLine("…\n"+s+"\n❯ ", 14); l != "" {
			t.Errorf("拾ってはいけない: %q → %q", s, l)
		}
	}
	// 14 行より前（古い履歴）に出たエラーは拾わない。
	old := "API Error: Server error mid-response.\n" + strings.Repeat("ok\n", 30)
	if apiErrorLine(old, 14) != "" {
		t.Error("古いエラーを拾った")
	}
}

type fakePane struct {
	mu      sync.Mutex
	screen  string
	status  string
	enabled bool
	sent    []string
	log     []string
}

func (f *fakePane) retrier() *apiRetrier {
	immediate := func(time.Duration) <-chan time.Time {
		c := make(chan time.Time, 1)
		c <- time.Now()
		return c
	}
	return &apiRetrier{
		prev: map[string]string{}, tries: map[string]int{}, pending: map[string]bool{},
		enabled: func(string) bool { f.mu.Lock(); defer f.mu.Unlock(); return f.enabled },
		read:    func(string) (string, error) { f.mu.Lock(); defer f.mu.Unlock(); return f.screen, nil },
		send: func(_, text string) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.sent = append(f.sent, text)
			return nil
		},
		status: func(string) string { f.mu.Lock(); defer f.mu.Unlock(); return f.status },
		after:  immediate,
		record: func(_, line string) { f.mu.Lock(); f.log = append(f.log, line); f.mu.Unlock() },
		lg:     log.New(io.Discard, "", 0),
	}
}

func sessionsOf(status string) []map[string]any {
	return []map[string]any{{"key": "w1:p1", "agent_status": status}}
}

func (f *fakePane) waitSent(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		got := len(f.sent)
		f.mu.Unlock()
		if got >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	t.Fatalf("送信 %d 回を待ったが %d 回: log=%v", n, len(f.sent), f.log)
}

func (f *fakePane) quiet(t *testing.T) {
	t.Helper()
	time.Sleep(150 * time.Millisecond)
}

func TestAPIRetryResumesOnlyForEnabledErrorStops(t *testing.T) {
	ctx := context.Background()
	f := &fakePane{screen: "出力\nAPI Error: Server error mid-response. The response above may be incomplete.\n❯ ", status: "idle", enabled: true}
	a := f.retrier()

	// working → idle（エラー画面）→ 再開が 1 回入る。
	a.check(ctx, sessionsOf("working"))
	a.check(ctx, sessionsOf("idle"))
	f.waitSent(t, 1)
	if got := f.sent[0]; got != "[semi-auto api-retry 1/3] つづけて\r" {
		t.Fatalf("送った文言: %q", got)
	}
	// 同じ状態が続いても（idle のまま）二重に送らない。
	a.check(ctx, sessionsOf("idle"))
	f.quiet(t)
	if len(f.sent) != 1 {
		t.Fatalf("idle 維持で二重送信: %v", f.sent)
	}
	// 再開 → また落ちる（連続 2 回目・3 回目）→ 3 回で止まる。
	for i := 2; i <= 3; i++ {
		a.check(ctx, sessionsOf("working"))
		a.check(ctx, sessionsOf("idle"))
		f.waitSent(t, i)
	}
	a.check(ctx, sessionsOf("working"))
	a.check(ctx, sessionsOf("idle"))
	f.quiet(t)
	if len(f.sent) != 3 {
		t.Fatalf("連続 3 回を超えて送った: %v", f.sent)
	}
	f.mu.Lock()
	last := f.log[len(f.log)-1]
	f.mu.Unlock()
	if !strings.Contains(last, "停止（人の確認待ち）") {
		t.Fatalf("上限到達のログが無い: %q", last)
	}

	// エラー以外の停止で連続回数が戻り、次のエラーで再び再開できる。
	f.mu.Lock()
	f.screen = "完了しました。\n❯ "
	f.mu.Unlock()
	a.check(ctx, sessionsOf("working"))
	a.check(ctx, sessionsOf("idle"))
	f.quiet(t)
	f.mu.Lock()
	f.screen = "API Error: Connection error.\n❯ "
	f.mu.Unlock()
	a.check(ctx, sessionsOf("working"))
	a.check(ctx, sessionsOf("idle"))
	f.waitSent(t, 4)
	if f.sent[3] != "[semi-auto api-retry 1/3] つづけて\r" {
		t.Fatalf("リセット後の文言: %q", f.sent[3])
	}
}

func TestAPIRetrySkipsWhenDisabledOrStateChanged(t *testing.T) {
	ctx := context.Background()
	// OFF の pane は何もしない。
	f := &fakePane{screen: "API Error: Server error mid-response.\n", status: "idle", enabled: false}
	a := f.retrier()
	a.check(ctx, sessionsOf("working"))
	a.check(ctx, sessionsOf("idle"))
	f.quiet(t)
	if len(f.sent) != 0 {
		t.Fatalf("OFF なのに送った: %v", f.sent)
	}
	// 待機中に状態が working に戻った（人が自分で再開した）なら送らない。
	f2 := &fakePane{screen: "API Error: Server error mid-response.\n", status: "working", enabled: true}
	a2 := f2.retrier()
	a2.check(ctx, sessionsOf("working"))
	a2.check(ctx, sessionsOf("idle"))
	f2.quiet(t)
	if len(f2.sent) != 0 {
		t.Fatalf("状態が変わったのに送った: %v", f2.sent)
	}
	// レート制限は対象外。
	f3 := &fakePane{screen: "API Error: 429 rate limit exceeded. Resets in 2h\n", status: "idle", enabled: true}
	a3 := f3.retrier()
	a3.check(ctx, sessionsOf("working"))
	a3.check(ctx, sessionsOf("idle"))
	f3.quiet(t)
	if len(f3.sent) != 0 {
		t.Fatalf("レート制限で送った: %v", f3.sent)
	}
	// 最初から idle（daemon 再起動直後など prev が無い）は working→idle ではない＝送らない。
	f4 := &fakePane{screen: "API Error: Server error mid-response.\n", status: "idle", enabled: true}
	a4 := f4.retrier()
	a4.check(ctx, sessionsOf("idle"))
	f4.quiet(t)
	if len(f4.sent) != 0 {
		t.Fatalf("遷移なしで送った: %v", f4.sent)
	}
}
