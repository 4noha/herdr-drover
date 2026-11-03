package herdrapi

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// awaitReady は SubscribeReady の確立通知を待つ。
func awaitReady(t *testing.T, ready <-chan struct{}, timeout time.Duration, what string) {
	t.Helper()
	select {
	case <-ready:
	case <-time.After(timeout):
		t.Fatalf("%s が %s 以内に確立しない", what, timeout)
	}
}

// awaitPaneCreated は ch から目当ての pane_created が来るまで読む。
// herdr は購読ごとに過去 event のバックログを再送する（実測）ので、無関係な
// event は読み捨てて pane_id の exact-match だけを待つ。
func awaitPaneCreated(t *testing.T, ch <-chan Event, paneID string, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev := <-ch:
			if ev.Name != "pane_created" {
				continue
			}
			p, err := ev.Pane()
			if err != nil {
				t.Fatalf("decode pane from event: %v (data=%s)", err, ev.Data)
			}
			if p != nil && p.PaneID == paneID {
				return
			}
		case <-deadline:
			t.Fatalf("timeout waiting for pane_created of %s", paneID)
		}
	}
}

// TestSubscribeReceivesPaneCreated は実サーバで購読→pane 作成→event 受信。
// 購読名は dot 形（pane.created）・配信 Event.Name は underscore 形
// （pane_created）という非対称も込みで検証する。
func TestSubscribeReceivesPaneCreated(t *testing.T) {
	_, c := startHerdr(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan Event, 64)
	ready := make(chan struct{}, 1)
	subDone := make(chan error, 1)
	go func() { subDone <- c.SubscribeReady(ctx, []string{"pane.created", "pane.closed"}, ch, ready) }()

	// 購読確立を待ってから作る。0.7.4 は購読前の event も再送したので待たなくても
	// 届いたが、0.9.5 は再送しない（実測）＝待たないと pane_created を取りこぼす。
	awaitReady(t, ready, 15*time.Second, "初回購読")
	ws, err := c.WorkspaceCreate()
	if err != nil {
		t.Fatalf("workspace.create: %v", err)
	}
	awaitPaneCreated(t, ch, ws.RootPane.PaneID, 15*time.Second)

	cancel()
	select {
	case err := <-subDone:
		if err != context.Canceled {
			t.Fatalf("Subscribe should return ctx.Err() on cancel, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Subscribe did not return after cancel")
	}
}

// TestSubscribeReconnectsAfterServerRestart は切断→backoff 再購読の実検証:
// 実サーバを graceful stop → 同じ socket パスで再起動 → 新 pane の event が
// 同じ Subscribe 呼び出しに届くこと（cm socket-client の「切断=窓死亡」欠陥を
// 繰り返さないための中核性質）。
func TestSubscribeReconnectsAfterServerRestart(t *testing.T) {
	s, c := startHerdr(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan Event, 64)
	ready := make(chan struct{}, 1)
	go func() { _ = c.SubscribeReady(ctx, []string{"pane.created"}, ch, ready) }()

	// 1 本目のサーバで event が届くところまで確立
	awaitReady(t, ready, 15*time.Second, "初回購読")
	ws1, err := c.WorkspaceCreate()
	if err != nil {
		t.Fatalf("workspace.create #1: %v", err)
	}
	awaitPaneCreated(t, ch, ws1.RootPane.PaneID, 15*time.Second)

	// サーバ再起動（graceful stop は session.json を隔離 XDG に永続化する。
	// 復元 pane は event を出さない実測なので、待つのは新規 pane のみ）
	s.stop()
	s.startProcess()

	// 新サーバで**再購読が確立してから**新規 pane → 同じ ch に届く。
	// 再確立の通知が来ること自体が「切断＝窓死亡にならない」の物証。
	awaitReady(t, ready, 30*time.Second, "再購読")
	ws2, err := c.WorkspaceCreate()
	if err != nil {
		t.Fatalf("workspace.create #2: %v", err)
	}
	if ws2.RootPane.PaneID == ws1.RootPane.PaneID {
		// 復元で w1 が埋まる前提が崩れたら（=同一 id）本テストの識別が
		// 成立しないので前提ごと失敗させる（黙って偽緑にしない）
		t.Fatalf("pane id collision across restart: %s", ws2.RootPane.PaneID)
	}
	awaitPaneCreated(t, ch, ws2.RootPane.PaneID, 30*time.Second)
}

// TestSubscribeInvalidNameIsPermanentError は購読名不正が即 error で戻ること
// （backoff 無限リトライで沈黙しない）。
func TestSubscribeInvalidNameIsPermanentError(t *testing.T) {
	_, c := startHerdr(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ch := make(chan Event, 1)
	err := c.Subscribe(ctx, []string{"bogus.event"}, ch)
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if apiErr.Code != "invalid_request" {
		t.Fatalf("unexpected code %q (msg=%q)", apiErr.Code, apiErr.Message)
	}
	_ = fmt.Sprintf("%v", apiErr) // Error() 経路も踏んでおく
}
