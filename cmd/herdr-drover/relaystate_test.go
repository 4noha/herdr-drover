package main

// relayState（slave 用 relay 経由クライアント）の決定論テスト。fake /slave/*
// httptest サーバに対して:
//   - POST /slave/token で bearer を取り、以降の /slave/* に Authorization:
//     Bearer が乗る
//   - PushStatus の **client 側 content_hash ゲート**（差分なし tick は POST
//     を一切出さない＝near-$0）
//   - WatchWake の long-poll（since カーソル→cb(sid)）
//   - IsSelfRevoked / PutRelayGrant / OwnSessionKeys の写像
//   - 401 で token を force-refresh して 1 回だけ再試行
// を検証する。実 relay との突き合わせは test/ の実 relay e2e（herdr 不在時
// Skip）が担う。

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeRelay は /slave/* の最小実装。bearer 検証・token 鋳造・各操作の記録。
type fakeRelay struct {
	secret string

	mu           sync.Mutex
	tokenMints   int
	validTokens  map[string]bool
	pushCalls    int
	lastPushN    int
	lastGrantSID string
	lastGrantTTL int
	revoked      bool
	registerVer  string

	force401Once atomic.Bool // 次の /slave/revoked を 1 回だけ 401 にする
	wakeN        atomic.Int32
}

func newFakeRelay(t *testing.T, secret string) (*fakeRelay, *httptest.Server) {
	t.Helper()
	fr := &fakeRelay{secret: secret, validTokens: map[string]bool{}}
	mux := http.NewServeMux()

	mux.HandleFunc("/slave/token", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ PC, Secret string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Secret != fr.secret {
			http.Error(w, `{"error":"unauthorized"}`, 401)
			return
		}
		fr.mu.Lock()
		rev := fr.revoked
		fr.mu.Unlock()
		if rev { // 実 relay: 失効 slave は token mint も 403
			http.Error(w, `{"error":"revoked"}`, 403)
			return
		}
		fr.mu.Lock()
		fr.tokenMints++
		tok := "tok-" + itoa(fr.tokenMints)
		fr.validTokens[tok] = true
		fr.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"token": tok, "exp": time.Now().Add(time.Hour).Unix()})
	})

	authed := func(w http.ResponseWriter, r *http.Request) bool {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		fr.mu.Lock()
		ok := tok != "" && fr.validTokens[tok]
		rev := fr.revoked
		fr.mu.Unlock()
		if !ok {
			http.Error(w, `{"error":"unauthorized"}`, 401)
			return false
		}
		// 実 slaveGuard は失効 slave を handler 到達前に 403（全 /slave/*）。
		if rev {
			http.Error(w, `{"error":"revoked"}`, 403)
			return false
		}
		return true
	}

	mux.HandleFunc("/slave/register", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		var body struct {
			AgentVersion string `json:"agent_version"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		fr.mu.Lock()
		fr.registerVer = body.AgentVersion
		fr.mu.Unlock()
		writeJSON(w, map[string]any{"ok": true})
	})

	mux.HandleFunc("/slave/push", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		var body struct {
			Sessions []map[string]any `json:"sessions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		fr.mu.Lock()
		fr.pushCalls++
		fr.lastPushN = len(body.Sessions)
		fr.mu.Unlock()
		writeJSON(w, map[string]any{"changed": len(body.Sessions)})
	})

	mux.HandleFunc("/slave/delete", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	})

	mux.HandleFunc("/slave/sessions", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		writeJSON(w, map[string]any{"keys": []string{"w1:p1", "w2:p3"}})
	})

	mux.HandleFunc("/slave/grant", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		var body struct {
			SID        string `json:"sid"`
			TTLSeconds int    `json:"ttl_seconds"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		fr.mu.Lock()
		fr.lastGrantSID = body.SID
		fr.lastGrantTTL = body.TTLSeconds
		fr.mu.Unlock()
		writeJSON(w, map[string]any{"ok": true})
	})

	mux.HandleFunc("/slave/revoked", func(w http.ResponseWriter, r *http.Request) {
		if fr.force401Once.CompareAndSwap(true, false) {
			http.Error(w, `{"error":"unauthorized"}`, 401)
			return
		}
		if !authed(w, r) {
			return
		}
		fr.mu.Lock()
		rev := fr.revoked
		fr.mu.Unlock()
		writeJSON(w, map[string]any{"revoked": rev})
	})

	mux.HandleFunc("/slave/wake", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		n := fr.wakeN.Add(1)
		if n == 1 {
			writeJSON(w, map[string]any{"sid": "w1:p1", "ts": "2026-07-18T00:00:00.000000001Z"})
			return
		}
		// 以降は hold（long-poll 模擬）。client の ctx cancel で戻る。
		<-r.Context().Done()
	})

	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return fr, ts
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func newTestRelayState(ts *httptest.Server, secret string) *relayState {
	return &relayState{
		wsBase:   strings.Replace(ts.URL, "http://", "ws://", 1),
		httpBase: ts.URL,
		pc:       "pcX-herdr",
		secret:   secret,
		hc:       &http.Client{Timeout: 5 * time.Second},
		wakeHC:   &http.Client{},
		lg:       log.New(io.Discard, "", 0),
		lastHash: map[string]string{},
	}
}

func TestRelayStateTokenAndBasicOps(t *testing.T) {
	fr, ts := newFakeRelay(t, "s3cr3t")
	rs := newTestRelayState(ts, "s3cr3t")
	ctx := context.Background()

	if err := rs.RegisterPCVersion(ctx, "v9.9.9"); err != nil {
		t.Fatalf("RegisterPCVersion: %v", err)
	}
	fr.mu.Lock()
	if fr.registerVer != "v9.9.9" {
		t.Fatalf("register version 不一致: %q", fr.registerVer)
	}
	if fr.tokenMints < 1 {
		t.Fatalf("token が鋳造されていない（bearer 未取得）")
	}
	fr.mu.Unlock()

	if rs.IsSelfRevoked(ctx) {
		t.Fatalf("未失効のはず")
	}

	// data メソッドは未失効時に成功する（revoke 前に検証）。
	keys, err := rs.OwnSessionKeys(ctx)
	if err != nil {
		t.Fatalf("OwnSessionKeys: %v", err)
	}
	if len(keys) != 2 || keys[0] != "w1:p1" {
		t.Fatalf("keys 不一致: %v", keys)
	}
	if err := rs.PutRelayGrant(ctx, "w1:p1", "source", 60*time.Second); err != nil {
		t.Fatalf("PutRelayGrant: %v", err)
	}
	fr.mu.Lock()
	if fr.lastGrantSID != "w1:p1" || fr.lastGrantTTL != 60 {
		t.Fatalf("grant 不一致: sid=%q ttl=%d", fr.lastGrantSID, fr.lastGrantTTL)
	}
	fr.mu.Unlock()
	// viewer grant は no-op（slave は source のみ・relay も書かない）。
	if err := rs.PutRelayGrant(ctx, "w1:p1", "viewer", 60*time.Second); err != nil {
		t.Fatalf("viewer grant は no-op で nil のはず: %v", err)
	}

	// 失効: 実 relay は slaveGuard で全 /slave/* を 403 にする（旧 fake は
	// 200{revoked:true} を返し実 wire を模していなかった＝バグを隠していた）。
	fr.mu.Lock()
	fr.revoked = true
	fr.mu.Unlock()
	// /slave/revoked が 403 → IsSelfRevoked は 403=失効 と解釈して true
	// （旧 relaystate は非200 を false に潰し graceful dormancy が死んでいた）。
	if !rs.IsSelfRevoked(ctx) {
		t.Fatalf("失効(403)を検出できていない＝graceful dormancy が発火せず launchd 再起動ストーム")
	}
	// 失効後は data メソッドも 403 error（＝startup/tick が dormant へ遷移）。
	if _, err := rs.OwnSessionKeys(ctx); err == nil {
		t.Fatalf("失効後の OwnSessionKeys は 403 error のはず")
	}
}

// PushStatus の client 側 content_hash ゲート: 差分なし tick は POST を出さない。
func TestRelayStatePushContentHashGate(t *testing.T) {
	fr, ts := newFakeRelay(t, "s3cr3t")
	rs := newTestRelayState(ts, "s3cr3t")
	ctx := context.Background()

	sess := func(status string) []map[string]any {
		return []map[string]any{
			{"key": "w1:p1", "session_id": "w1:p1", "cwd": "/a/b", "short_dir": "b", "window_name": "x", "is_active": status == "working", "agent_status": status},
			{"key": "w2:p3", "session_id": "w2:p3", "cwd": "/c", "short_dir": "c", "window_name": "y", "is_active": false, "agent_status": "idle"},
		}
	}

	// 1) 初回: 2 件とも変化 → POST 1 回・2 件。
	if _, err := rs.PushStatus(ctx, sess("idle")); err != nil {
		t.Fatalf("push1: %v", err)
	}
	// 2) 同一: 差分なし → POST しない。
	if _, err := rs.PushStatus(ctx, sess("idle")); err != nil {
		t.Fatalf("push2: %v", err)
	}
	// 3) w1 の agent_status を変える → 1 件だけ POST。
	if _, err := rs.PushStatus(ctx, sess("working")); err != nil {
		t.Fatalf("push3: %v", err)
	}

	fr.mu.Lock()
	defer fr.mu.Unlock()
	if fr.pushCalls != 2 {
		t.Fatalf("POST 回数=%d（初回＋差分の 2 回のはず。差分なし tick で POST したら near-$0 破れ）", fr.pushCalls)
	}
	if fr.lastPushN != 1 {
		t.Fatalf("最終 POST の件数=%d（変化した 1 件のみのはず）", fr.lastPushN)
	}
}

// WatchWake の long-poll: 200 の {sid,ts} を cb(sid) に流し、以降 hold。
func TestRelayStateWatchWakeLongPoll(t *testing.T) {
	_, ts := newFakeRelay(t, "s3cr3t")
	rs := newTestRelayState(ts, "s3cr3t")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan string, 1)
	go func() {
		_ = rs.WatchWake(ctx, func(sid string) {
			select {
			case got <- sid:
			default:
			}
		})
	}()
	select {
	case sid := <-got:
		if sid != "w1:p1" {
			t.Fatalf("wake sid 不一致: %q", sid)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("WatchWake が cb を発火しない")
	}
	cancel() // hold 中の poll を解除して WatchWake を戻す
}

// 401 で token を force-refresh して 1 回だけ再試行する。
func TestRelayStateRefreshOn401(t *testing.T) {
	fr, ts := newFakeRelay(t, "s3cr3t")
	rs := newTestRelayState(ts, "s3cr3t")
	ctx := context.Background()

	// 先に bearer を確立（token 1 鋳造）。
	if rs.IsSelfRevoked(ctx) {
		t.Fatal("未失効のはず")
	}
	fr.mu.Lock()
	mints0 := fr.tokenMints
	fr.mu.Unlock()

	// 次の /slave/revoked を 1 回だけ 401 → call が force-refresh して再試行。
	fr.force401Once.Store(true)
	if rs.IsSelfRevoked(ctx) {
		t.Fatal("再試行後は 200 revoked=false のはず")
	}
	fr.mu.Lock()
	defer fr.mu.Unlock()
	if fr.tokenMints <= mints0 {
		t.Fatalf("401 で token を再鋳造していない（mints %d→%d）", mints0, fr.tokenMints)
	}
}

// newRelayState は slave.json を読む（不在は明示エラー）。
func TestNewRelayStateReadsSlaveFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".herdr-drover")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// 不在: エラー。
	if _, err := newRelayState("wss://r.example", "pcX-herdr", Config{}, log.New(io.Discard, "", 0)); err == nil {
		t.Fatal("slave.json 不在なのにエラーにならない")
	}
	// 配置後: 成功し secret/pc/wsBase が入る。
	body := `{"pc":"pcX-herdr","refresh_secret":"abc123","relay_url":"wss://r.example","gcp_project":"p"}`
	if err := os.WriteFile(filepath.Join(dir, "slave.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("write slave.json: %v", err)
	}
	rs, err := newRelayState("wss://r.example", "pcX-herdr", Config{}, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("newRelayState: %v", err)
	}
	if rs.secret != "abc123" || rs.pc != "pcX-herdr" {
		t.Fatalf("slave.json 反映不整合: secret=%q pc=%q", rs.secret, rs.pc)
	}
	if rs.httpBase != "https://r.example" {
		t.Fatalf("wss→https 変換不正: %q", rs.httpBase)
	}
}
