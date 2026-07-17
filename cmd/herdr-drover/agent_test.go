package main

// runAgentLoop の決定論テスト（チャネル/関数注入。時間依存を排すため周期は
// 1h にして nudge だけで駆動する）。daemon 全体（実 Firestore＋実 producer）
// の e2e は統合フェーズ。

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// startLoop は runAgentLoop を goroutine で起動し、tick 完了通知・nudge 注入・
// 終了待ちの各チャネルを返す。
func startLoop(t *testing.T, tickFn func(context.Context) error, lg *log.Logger) (nudge chan os.Signal, done chan struct{}, cancel context.CancelFunc) {
	t.Helper()
	ctx, cancelCtx := context.WithCancel(context.Background())
	nudge = make(chan os.Signal, 1)
	done = make(chan struct{})
	go func() {
		defer close(done)
		runAgentLoop(ctx, time.Hour, nudge, tickFn, lg)
	}()
	return nudge, done, cancelCtx
}

func waitTick(t *testing.T, calls <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-calls:
	case <-time.After(5 * time.Second):
		t.Fatalf("timeout: %s", what)
	}
}

// 起動直後に 1 回 tick し、nudge（SIGUSR1 相当）で即時 re-scan が走ること。
func TestRunAgentLoopNudgeTriggersImmediateRescan(t *testing.T) {
	calls := make(chan struct{}, 16)
	tickFn := func(context.Context) error {
		calls <- struct{}{}
		return nil
	}
	nudge, done, cancel := startLoop(t, tickFn, log.New(io.Discard, "", 0))
	defer cancel()

	waitTick(t, calls, "起動直後の初回 tick") // 周期 1h＝ticker では説明不能
	nudge <- syscall.SIGUSR1
	waitTick(t, calls, "nudge 後の即時 tick")

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("ctx cancel でループが終了しない")
	}
}

// tick エラーは skip（ループ継続）し、ログは遷移時のみ（同文連発しない・
// 復帰も 1 回記録）であること。
func TestRunAgentLoopErrorTransitionLogging(t *testing.T) {
	var buf bytes.Buffer
	lg := log.New(&buf, "", 0)
	seq := []error{errors.New("boom"), errors.New("boom"), nil}
	i := 0
	calls := make(chan struct{}, 16)
	tickFn := func(context.Context) error {
		defer func() { calls <- struct{}{} }()
		if i < len(seq) {
			e := seq[i]
			i++
			return e
		}
		return nil
	}
	nudge, done, cancel := startLoop(t, tickFn, lg)
	defer cancel()

	waitTick(t, calls, "tick1(boom)")
	nudge <- syscall.SIGUSR1
	waitTick(t, calls, "tick2(boom 再発)")
	nudge <- syscall.SIGUSR1
	waitTick(t, calls, "tick3(復帰)")
	cancel()
	<-done // ループ終了後にのみ buf を読む（ログはループ goroutine が書く）

	logs := buf.String()
	if got := strings.Count(logs, "tick エラー"); got != 1 {
		t.Fatalf("エラーログは遷移時 1 回のはず: %d 回\n%s", got, logs)
	}
	if !strings.Contains(logs, "boom") {
		t.Fatalf("エラー内容が残っていない:\n%s", logs)
	}
	if got := strings.Count(logs, "tick 復帰"); got != 1 {
		t.Fatalf("復帰ログは 1 回のはず: %d 回\n%s", got, logs)
	}
}
