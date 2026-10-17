package main

// webhookNotifier の遷移検知ロジックを検証する。drover-cloud 側（Firestore
// 永続化・https/CRLF検証）は webhook/state パッケージの自前テストが担保
// 済み＝ここは working→{idle,done,blocked} 遷移の検知条件（taskNotifier と
// 対称）だけを軽量に確認する。webhook.Validate が https を必須にしているため
// 受信側は httptest.NewTLSServer を使い、自己署名証明書を一時的に信頼させる。

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/4noha/drover-cloud/webhook"
)

type fakeWebhookStore struct {
	cfg webhook.Config
}

func (f *fakeWebhookStore) GetWebhookConfig(context.Context) (webhook.Config, error) {
	return f.cfg, nil
}

// webhookReceiver は受信した Payload を蓄積するテスト用 https サーバー。
type webhookReceiver struct {
	mu  sync.Mutex
	got []webhook.Payload
	srv *httptest.Server
}

func newWebhookReceiver(t *testing.T) *webhookReceiver {
	t.Helper()
	wr := &webhookReceiver{}
	wr.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p webhook.Payload
		_ = json.NewDecoder(r.Body).Decode(&p)
		wr.mu.Lock()
		wr.got = append(wr.got, p)
		wr.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(wr.srv.Close)
	orig := http.DefaultClient.Transport
	http.DefaultClient.Transport = wr.srv.Client().Transport
	t.Cleanup(func() { http.DefaultClient.Transport = orig })
	return wr
}

func (wr *webhookReceiver) waitCount(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		wr.mu.Lock()
		c := len(wr.got)
		wr.mu.Unlock()
		if c >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("webhook 受信件数が %d に届かない", n)
}

func (wr *webhookReceiver) assertStableCount(t *testing.T, n int) {
	t.Helper()
	time.Sleep(200 * time.Millisecond)
	wr.mu.Lock()
	defer wr.mu.Unlock()
	if len(wr.got) != n {
		t.Fatalf("受信件数=%d, want %d: %+v", len(wr.got), n, wr.got)
	}
}

func (wr *webhookReceiver) last() webhook.Payload {
	wr.mu.Lock()
	defer wr.mu.Unlock()
	return wr.got[len(wr.got)-1]
}

// 初観測（prev 無し）の idle/done/blocked は送らない（daemon 再起動直後に
// 既に idle な pane を誤通知しない。taskNotifier の working 限定ロジックと
// 対称）。
func TestWebhookNotifierNoSendOnFirstObservation(t *testing.T) {
	wr := newWebhookReceiver(t)
	wn := newWebhookNotifier("mac-studio-herdr", log.New(io.Discard, "", 0))
	wn.cachedCfg = webhook.Config{Enabled: true, URL: wr.srv.URL, HeaderName: "X", HeaderSecret: "y"}
	wn.cachedAt = time.Now()
	ctx := context.Background()

	wn.check(ctx, &fakeWebhookStore{}, []map[string]any{sess("w1:p1", "idle", "claude")})
	wr.assertStableCount(t, 0)
}

// working→idle（CLI エージェントの通常の作業完了。herdr は "done" ではなく
// "idle" で報告する＝webhook.NotifyStates のコメント参照）で送信・idle→idle
// （同一状態）は再送しない・idle→blocked は working 起点でないため送らない
// （taskNotifier と同じ working 限定の規律）・working に戻ってからの
// working→blocked では再度送信する。
func TestWebhookNotifierDetectsTransitions(t *testing.T) {
	wr := newWebhookReceiver(t)
	wn := newWebhookNotifier("mac-studio-herdr", log.New(io.Discard, "", 0))
	wn.cachedCfg = webhook.Config{Enabled: true, URL: wr.srv.URL, HeaderName: "X-Sec", HeaderSecret: "s3cr3t"}
	wn.cachedAt = time.Now()
	ctx := context.Background()
	store := &fakeWebhookStore{cfg: wn.cachedCfg}

	wn.check(ctx, store, []map[string]any{sessDir("w1:p1", "working", "claude", "proj")})
	wr.assertStableCount(t, 0)

	wn.check(ctx, store, []map[string]any{sessDir("w1:p1", "idle", "claude", "proj")})
	wr.waitCount(t, 1)
	p := wr.last()
	if p.PC != "mac-studio-herdr" || p.SID != "w1:p1" || p.OldStatus != "working" ||
		p.NewStatus != "idle" || p.WindowName != "claude" || p.Cwd != "" || p.At == "" {
		t.Fatalf("payload 不正: %+v", p)
	}

	wn.check(ctx, store, []map[string]any{sessDir("w1:p1", "idle", "claude", "proj")})
	wr.assertStableCount(t, 1)

	wn.check(ctx, store, []map[string]any{sessDir("w1:p1", "blocked", "claude", "proj")})
	wr.assertStableCount(t, 1) // idle→blocked は working 起点でない＝送らない

	wn.check(ctx, store, []map[string]any{sessDir("w1:p1", "working", "claude", "proj")})
	wr.assertStableCount(t, 1) // blocked→working（working 自体は通知対象外）

	wn.check(ctx, store, []map[string]any{sessDir("w1:p1", "blocked", "claude", "proj")})
	wr.waitCount(t, 2)
	if p2 := wr.last(); p2.OldStatus != "working" || p2.NewStatus != "blocked" {
		t.Fatalf("working→blocked の payload 不正: %+v", p2)
	}
}

// Webhook 無効（既定 OFF）なら何も送らない。
func TestWebhookNotifierNoopWhenDisabled(t *testing.T) {
	wr := newWebhookReceiver(t)
	wn := newWebhookNotifier("mac-studio-herdr", log.New(io.Discard, "", 0))
	wn.cachedCfg = webhook.Config{Enabled: false}
	wn.cachedAt = time.Now()
	store := &fakeWebhookStore{cfg: wn.cachedCfg}
	ctx := context.Background()

	wn.check(ctx, store, []map[string]any{sess("w1:p1", "working", "a")})
	wn.check(ctx, store, []map[string]any{sess("w1:p1", "idle", "a")})
	wr.assertStableCount(t, 0)
}

// working 以外からの遷移は通知対象外（working→X の X が NotifyStates に
// 入っていても、遷移元が working でなければ送らない。taskNotifier と同じ
// 規律。unknown からの遷移は一時的な検出揺れで誤通知しないため特に重要）。
func TestWebhookNotifierIgnoresNonWorkingOrigin(t *testing.T) {
	wr := newWebhookReceiver(t)
	wn := newWebhookNotifier("mac-studio-herdr", log.New(io.Discard, "", 0))
	wn.cachedCfg = webhook.Config{Enabled: true, URL: wr.srv.URL, HeaderName: "X", HeaderSecret: "y"}
	wn.cachedAt = time.Now()
	store := &fakeWebhookStore{cfg: wn.cachedCfg}
	ctx := context.Background()

	wn.check(ctx, store, []map[string]any{sess("w1:p1", "unknown", "a")}) // 初観測
	wn.check(ctx, store, []map[string]any{sess("w1:p1", "idle", "a")})    // unknown→idle
	wn.check(ctx, store, []map[string]any{sess("w1:p1", "blocked", "a")}) // idle→blocked
	wn.check(ctx, store, []map[string]any{sess("w1:p1", "working", "a")}) // blocked→working（working 自体も対象外）
	wr.assertStableCount(t, 0)
}

// 消滅した pane_id は prev から掃除される（taskNotifier と同じ規律）。
func TestWebhookNotifierPrunesGoneKeys(t *testing.T) {
	wn := newWebhookNotifier("pc1", log.New(io.Discard, "", 0))
	ctx := context.Background()
	store := &fakeWebhookStore{cfg: webhook.Config{Enabled: false}}

	wn.check(ctx, store, []map[string]any{sess("w1:p1", "working", "a")})
	wn.check(ctx, store, []map[string]any{}) // pane 消滅
	if _, ok := wn.prev["w1:p1"]; ok {
		t.Fatal("消滅した pane_id が prev に残っている")
	}
}

// configFor は TTL 内なら GetWebhookConfig を呼ばずキャッシュを返す。
func TestWebhookNotifierConfigCaching(t *testing.T) {
	// cachedAt はゼロ値のまま（=十分古い）にする＝初回呼び出しが必ず取得に
	// 行くことを保証する（newWebhookNotifier は cachedAt を設定しない）。
	wn := newWebhookNotifier("pc1", log.New(io.Discard, "", 0))
	calls := 0
	store := &countingWebhookStore{cfg: webhook.Config{Enabled: false}, calls: &calls}
	ctx := context.Background()

	cfg1 := wn.configFor(ctx, store)
	cfg2 := wn.configFor(ctx, store)
	if calls != 1 {
		t.Fatalf("TTL内なのに GetWebhookConfig が %d 回呼ばれた", calls)
	}
	if cfg1 != cfg2 {
		t.Fatalf("キャッシュされた値が一致しない: %+v vs %+v", cfg1, cfg2)
	}

	wn.cachedAt = time.Now().Add(-webhookConfigCacheTTL - time.Second) // 期限切れにする
	_ = wn.configFor(ctx, store)
	if calls != 2 {
		t.Fatalf("TTL切れ後に再取得されない: calls=%d", calls)
	}
}

type countingWebhookStore struct {
	cfg   webhook.Config
	calls *int
}

func (c *countingWebhookStore) GetWebhookConfig(context.Context) (webhook.Config, error) {
	*c.calls++
	return c.cfg, nil
}
