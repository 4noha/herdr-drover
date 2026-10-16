package main

// runLastSeenHeartbeat の決定論テスト（関数注入。runAgentLoop のテスト
// スタイルに合わせる。実 Firestore 往復（TouchLastSeen/PCLastSeen）は
// drover-cloud state パッケージ側で検証済み＝ここはタイマー駆動だけを見る）。

import (
	"context"
	"errors"
	"io"
	"log"
	"sync"
	"testing"
	"time"
)

func TestRunLastSeenHeartbeatFiresImmediatelyAndOnTick(t *testing.T) {
	calls := make(chan struct{}, 16)
	touch := func(context.Context) error {
		calls <- struct{}{}
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runLastSeenHeartbeat(ctx, 30*time.Millisecond, touch, log.New(io.Discard, "", 0))
	}()

	select {
	case <-calls:
	case <-time.After(2 * time.Second):
		t.Fatal("起動直後の初回 touch が来ない")
	}
	select {
	case <-calls:
	case <-time.After(2 * time.Second):
		t.Fatal("周期 tick の touch が来ない")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ctx cancel で終了しない")
	}
}

// touch の単発失敗はログのみ＝周期を止めない（単発の Firestore 不調で
// daemon の生存報告自体が止まる事故を防ぐ）。
func TestRunLastSeenHeartbeatErrorDoesNotStopLoop(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	touch := func(context.Context) error {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			return errors.New("一時的な Firestore 不調")
		}
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runLastSeenHeartbeat(ctx, 20*time.Millisecond, touch, log.New(io.Discard, "", 0))
	}()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := calls
		mu.Unlock()
		if n >= 3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	n := calls
	mu.Unlock()
	if n < 3 {
		t.Fatalf("エラー後に周期が止まった（calls=%d）", n)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ctx cancel で終了しない")
	}
}

// ctx が既にキャンセル済みでも、起動直後の 1 回は呼ばれずに即座に戻る
// （do() 内で touch → select の順だが ctx.Err() チェックはログ抑制のみ＝
// 初回 touch は必ず走る契約。この契約を固定するテスト）。
func TestRunLastSeenHeartbeatStopsPromptlyAfterCancel(t *testing.T) {
	touch := func(context.Context) error { return nil }
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 開始前に cancel 済み
	done := make(chan struct{})
	go func() {
		defer close(done)
		runLastSeenHeartbeat(ctx, time.Hour, touch, log.New(io.Discard, "", 0))
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancel 済み ctx で起動しても終了しない（初回 touch 後の select で即終了するはず）")
	}
}
