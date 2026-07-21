package main

// taskNotifier の遷移検知ロジックを検証する。Firestore/FCM は fake で
// 注入する（Firestore 側の SavePushToken/ListPushTokens/DeletePushToken の
// 実挙動は drover-cloud/state の実 emulator テストが担保済み・push.Send の
// HTTP 契約は drover-cloud/push の httptest テストが担保済み＝ここは
// working→idle/done/blocked 遷移の検知条件だけを軽量に確認する）。

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTaskNotifyState(t *testing.T) {
	for _, s := range []string{"idle", "done", "blocked"} {
		if !taskNotifyState(s) {
			t.Errorf("taskNotifyState(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"working", "unknown", ""} {
		if taskNotifyState(s) {
			t.Errorf("taskNotifyState(%q) = true, want false", s)
		}
	}
}

type fakePushStore struct {
	tokens  []string
	deleted []string
}

func (f *fakePushStore) ListPushTokens(context.Context) ([]string, error) { return f.tokens, nil }
func (f *fakePushStore) DeletePushToken(_ context.Context, tok string) error {
	f.deleted = append(f.deleted, tok)
	return nil
}

func sess(key, status, name string) map[string]any {
	return map[string]any{"key": key, "agent_status": status, "window_name": name}
}

// fakeFCM は FCM v1 API を模す。sendCount はトークンごとの呼出回数を記録し、
// unregistered に含まれる token へは UNREGISTERED エラーを返す。
func fakeFCM(t *testing.T, unregistered map[string]bool) (*httptest.Server, map[string]int) {
	t.Helper()
	sendCount := map[string]int{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Message struct {
				Token string `json:"token"`
			} `json:"message"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		sendCount[body.Message.Token]++
		if unregistered[body.Message.Token] {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{"code": 404, "status": "NOT_FOUND",
					"details": []map[string]any{{"errorCode": "UNREGISTERED"}}},
			})
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"name": "ok"})
	}))
	t.Cleanup(ts.Close)
	return ts, sendCount
}

func newTestNotifier(ts *httptest.Server) *taskNotifier {
	return &taskNotifier{
		prev:      map[string]string{},
		hc:        ts.Client(),
		baseURL:   ts.URL,
		projectID: "demo-proj",
		lg:        log.New(io.Discard, "", 0),
	}
}

// working→idle への遷移だけが通知を送る（working 以外→idle・working のまま
// 維持・unknown への遷移は送らない）。
func TestTaskNotifierDetectsTransition(t *testing.T) {
	ts, sendCount := fakeFCM(t, nil)
	tn := newTestNotifier(ts)
	store := &fakePushStore{tokens: []string{"tok-1"}}
	ctx := context.Background()

	// 1周目: 初出現の working。まだ prev が無いので通知しない
	// （daemon 再起動直後に既に working なだけの pane を誤通知しない）。
	tn.check(ctx, store, []map[string]any{sess("w1:p1", "working", "claude")})
	if sendCount["tok-1"] != 0 {
		t.Fatalf("初回 working で通知してはいけない: sendCount=%d", sendCount["tok-1"])
	}

	// 2周目: working→idle 遷移。通知が飛ぶ。
	tn.check(ctx, store, []map[string]any{sess("w1:p1", "idle", "claude")})
	if sendCount["tok-1"] != 1 {
		t.Fatalf("working→idle で通知1回のはず: sendCount=%d", sendCount["tok-1"])
	}

	// 3周目: idle のまま（非 working → 非 working）。再通知しない。
	tn.check(ctx, store, []map[string]any{sess("w1:p1", "idle", "claude")})
	if sendCount["tok-1"] != 1 {
		t.Fatalf("idle 維持で再通知してはいけない: sendCount=%d", sendCount["tok-1"])
	}

	// 4周目: idle→working（再開）。通知しない。
	tn.check(ctx, store, []map[string]any{sess("w1:p1", "working", "claude")})
	if sendCount["tok-1"] != 1 {
		t.Fatalf("idle→working で通知してはいけない: sendCount=%d", sendCount["tok-1"])
	}

	// 5周目: working→unknown（検出揺れ想定）。通知しない。
	tn.check(ctx, store, []map[string]any{sess("w1:p1", "unknown", "claude")})
	if sendCount["tok-1"] != 1 {
		t.Fatalf("working→unknown で通知してはいけない: sendCount=%d", sendCount["tok-1"])
	}

	// 6周目: working→blocked。通知する。
	tn.check(ctx, store, []map[string]any{sess("w1:p1", "working", "claude")}) // まず working に戻す
	tn.check(ctx, store, []map[string]any{sess("w1:p1", "blocked", "claude")})
	if sendCount["tok-1"] != 2 {
		t.Fatalf("working→blocked で通知2回目のはず: sendCount=%d", sendCount["tok-1"])
	}
}

// 消滅した pane_id は prev から掃除される（次に同じ pane_id が別セッション
// として working で現れても「初回」扱い＝誤って旧状態を引きずらない）。
func TestTaskNotifierPrunesGoneKeys(t *testing.T) {
	ts, sendCount := fakeFCM(t, nil)
	tn := newTestNotifier(ts)
	store := &fakePushStore{tokens: []string{"tok-1"}}
	ctx := context.Background()

	tn.check(ctx, store, []map[string]any{sess("w1:p1", "working", "a")})
	tn.check(ctx, store, []map[string]any{}) // pane 消滅
	if _, ok := tn.prev["w1:p1"]; ok {
		t.Fatal("消滅した pane_id が prev に残っている")
	}
	// 同じ id が復活しても working→idle でなく idle 単体は「初回」扱いで通知しない。
	tn.check(ctx, store, []map[string]any{sess("w1:p1", "idle", "a")})
	if sendCount["tok-1"] != 0 {
		t.Fatalf("枝刈り後の初回 idle で通知してはいけない: sendCount=%d", sendCount["tok-1"])
	}
}

// 登録トークンが複数あれば全員に送る。
func TestTaskNotifierSendsToAllTokens(t *testing.T) {
	ts, sendCount := fakeFCM(t, nil)
	tn := newTestNotifier(ts)
	store := &fakePushStore{tokens: []string{"tok-1", "tok-2", "tok-3"}}
	ctx := context.Background()

	tn.check(ctx, store, []map[string]any{sess("w1:p1", "working", "a")})
	tn.check(ctx, store, []map[string]any{sess("w1:p1", "done", "a")})
	for _, tok := range store.tokens {
		if sendCount[tok] != 1 {
			t.Fatalf("token %s への送信回数=%d, want 1", tok, sendCount[tok])
		}
	}
}

// UNREGISTERED を返す token は DeletePushToken で自己修復する。
func TestTaskNotifierDeletesUnregisteredToken(t *testing.T) {
	ts, _ := fakeFCM(t, map[string]bool{"stale-tok": true})
	tn := newTestNotifier(ts)
	store := &fakePushStore{tokens: []string{"stale-tok", "ok-tok"}}
	ctx := context.Background()

	tn.check(ctx, store, []map[string]any{sess("w1:p1", "working", "a")})
	tn.check(ctx, store, []map[string]any{sess("w1:p1", "idle", "a")})

	if len(store.deleted) != 1 || store.deleted[0] != "stale-tok" {
		t.Fatalf("stale-tok が削除されるはず: deleted=%v", store.deleted)
	}
}

// hc=nil（push 無効構成）では通知を試みない（遷移追跡自体は継続する）。
func TestTaskNotifierNoopWhenPushDisabled(t *testing.T) {
	tn := &taskNotifier{prev: map[string]string{}, hc: nil, lg: log.New(io.Discard, "", 0)}
	store := &fakePushStore{tokens: []string{"tok-1"}}
	ctx := context.Background()

	tn.check(ctx, store, []map[string]any{sess("w1:p1", "working", "a")})
	tn.check(ctx, store, []map[string]any{sess("w1:p1", "idle", "a")}) // panic せず no-op のはず
	if len(store.deleted) != 0 {
		t.Fatalf("push 無効なのに削除が起きている: %v", store.deleted)
	}
}
