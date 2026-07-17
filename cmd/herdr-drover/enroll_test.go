package main

// enroll の配置検証: ローカル fake enroll server（cm web.go enroll handler の
// HTTP 契約準拠＝POST のみ・form `code`・**一回消費**・無効/期限切れ 401・
// 200 JSON {gcp_project, relay_url, sa_json}）に対して実 HTTP で
// `run(["enroll", ...])` の実バイナリ経路を通し、
//   - ~/.herdr-drover/sa.json（0600・内容一致）
//   - ~/.herdr-drover/config.json（gcp_project/cloud_relay_url/鍵パス）
//   - resolveConfig が file を拾う（env > file）
//   - コード再利用は 401＝exit 1／usage エラーは exit 2
// を検証する。relay 実体は cm 資産（無改変）なので、この契約 exact-match の
// fake が守るべき仕様の写し（実 relay との突き合わせは cm 側 e2e が担保）。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeEnrollServer は一回消費の enroll エンドポイントを立て、
// ws:// 形式の --relay 引数（cmdEnroll が http:// へ変換する）を返す。
func fakeEnrollServer(t *testing.T, code, project, relayURL, saJSON string) (wsURL string) {
	t.Helper()
	var mu sync.Mutex
	used := false
	mux := http.NewServeMux()
	mux.HandleFunc("/enroll", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST のみ", http.StatusMethodNotAllowed)
			return
		}
		got := r.FormValue("code")
		mu.Lock()
		ok := got == code && !used
		if ok {
			used = true // 一回消費（cm ConsumePairing: 成否問わず削除）
		}
		mu.Unlock()
		if !ok {
			http.Error(w, "コードが無効か期限切れです", http.StatusUnauthorized)
			return
		}
		resp := map[string]any{"gcp_project": project, "relay_url": relayURL}
		if saJSON != "" {
			resp["sa_json"] = saJSON
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return "ws://" + strings.TrimPrefix(ts.URL, "http://")
}

func TestEnrollPlacesKeyAndConfig(t *testing.T) {
	clearDroverEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	// sa_json は「資格情報として無効な JSON」を使う＝enroll 末尾の
	// ClearRevoked best-effort が NewWithCredentials で即エラー→skip し、
	// テストが GCP/エミュレータへ一切出ないことを保証する。
	const saJSON = `{"fake":"sa"}`
	wsURL := fakeEnrollServer(t, "ABCD1234", "proj-enroll", "wss://relay.example", saJSON)

	code, out, errb := runCapture(t, "enroll", "ABCD1234", "--relay", wsURL)
	if code != 0 {
		t.Fatalf("enroll exit=%d\nstdout:%s\nstderr:%s", code, out, errb)
	}

	// SA 鍵: 内容一致＋0600（秘密鍵の権限規律）。
	saPath := filepath.Join(home, ".herdr-drover", "sa.json")
	b, err := os.ReadFile(saPath)
	if err != nil {
		t.Fatalf("sa.json 未配置: %v", err)
	}
	if string(b) != saJSON {
		t.Fatalf("sa.json 内容不一致: %q", b)
	}
	if fi, _ := os.Stat(saPath); fi.Mode().Perm() != 0o600 {
		t.Fatalf("sa.json 権限が %v（0600 のはず）", fi.Mode().Perm())
	}

	// config.json: enroll の 3 キーが入る。
	fc, err := readFileConfig(filepath.Join(home, ".herdr-drover", "config.json"))
	if err != nil {
		t.Fatalf("config.json: %v", err)
	}
	if fc.GCPProject != "proj-enroll" || fc.CloudRelayURL != "wss://relay.example" ||
		fc.GoogleApplicationCredentials != saPath {
		t.Fatalf("config.json 内容が想定外: %+v", fc)
	}

	// resolveConfig が file を拾う（enroll 直後に agent がそのまま動く形）。
	cfg, err := resolveConfig()
	if err != nil {
		t.Fatalf("resolveConfig: %v", err)
	}
	if cfg.Project != "proj-enroll" || cfg.RelayURL != "wss://relay.example" ||
		cfg.Credentials != saPath {
		t.Fatalf("resolveConfig が enroll 結果を拾わない: %+v", cfg)
	}
	// env > file（launchd plist 運用の上書きが常に勝つ）。
	t.Setenv("GCP_PROJECT", "proj-env")
	cfg2, _ := resolveConfig()
	if cfg2.Project != "proj-env" {
		t.Fatalf("env > file が破れている: %+v", cfg2)
	}

	// 同じコードの再利用は 401（一回消費）＝exit 1・明示エラー。
	code2, _, errb2 := runCapture(t, "enroll", "ABCD1234", "--relay", wsURL)
	if code2 != 1 || !strings.Contains(errb2, "無効か期限切れ") {
		t.Fatalf("consumed コードの再利用が拒否されない: exit=%d stderr=%q", code2, errb2)
	}
}

// relay_url が応答に無い場合は --relay 引数へ fallback（cm 同順）。
// sa_json 無し（relay 側 ENROLL_SA_JSON 未設定）でも config は書かれる。
func TestEnrollFallbacksWithoutSAJSON(t *testing.T) {
	clearDroverEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	wsURL := fakeEnrollServer(t, "ZZZZ9999", "proj-nosa", "", "")

	code, out, errb := runCapture(t, "enroll", "ZZZZ9999", "--relay", wsURL)
	if code != 0 {
		t.Fatalf("enroll exit=%d\nstdout:%s\nstderr:%s", code, out, errb)
	}
	if _, err := os.Stat(filepath.Join(home, ".herdr-drover", "sa.json")); err == nil {
		t.Fatalf("sa_json 無しなのに sa.json が生えた")
	}
	fc, err := readFileConfig(filepath.Join(home, ".herdr-drover", "config.json"))
	if err != nil {
		t.Fatalf("config.json: %v", err)
	}
	if fc.GCPProject != "proj-nosa" || fc.CloudRelayURL != wsURL ||
		fc.GoogleApplicationCredentials != "" {
		t.Fatalf("fallback 配置が想定外: %+v", fc)
	}
}

// 既存 config.json の他キー（pc_id 等）は enroll が保持する（cm
// writeTomlKeys の「同名キーのみ置換」規律）。
func TestEnrollPreservesOtherFileKeys(t *testing.T) {
	clearDroverEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".herdr-drover")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"),
		[]byte(`{"pc_id":"custom-herdr","gcp_project":"old-proj"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	wsURL := fakeEnrollServer(t, "KEEP1111", "new-proj", "wss://r.example", "")
	if code, out, errb := runCapture(t, "enroll", "KEEP1111", "--relay", wsURL); code != 0 {
		t.Fatalf("enroll exit=%d\nstdout:%s\nstderr:%s", code, out, errb)
	}
	fc, err := readFileConfig(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if fc.PCID != "custom-herdr" || fc.GCPProject != "new-proj" {
		t.Fatalf("pc_id 保持/gcp_project 更新が想定外: %+v", fc)
	}
}

func TestEnrollUsageErrors(t *testing.T) {
	clearDroverEnv(t)
	// code 無し / --relay 無しは exit 2（usage）。ネットへ出ない。
	for _, args := range [][]string{
		{"enroll"},
		{"enroll", "ABCD1234"},
		{"enroll", "--relay", "wss://x.example"},
	} {
		code, _, errb := runCapture(t, args...)
		if code != 2 || !strings.Contains(errb, "--relay") {
			t.Fatalf("%v: exit=%d stderr=%q（usage exit 2 のはず）", args, code, errb)
		}
	}
}

// fake server が守る一回消費の器の自己検証（fake が契約からズレると enroll
// テスト全体の意味が消えるため）: 誤コードは 401 のまま消費しない。
func TestFakeEnrollServerContract(t *testing.T) {
	wsURL := fakeEnrollServer(t, "GOOD0001", "p", "", "")
	httpURL := "http://" + strings.TrimPrefix(wsURL, "ws://")
	post := func(code string) int {
		resp, err := http.PostForm(httpURL+"/enroll", map[string][]string{"code": {code}})
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := post("WRONG"); got != 401 {
		t.Fatalf("誤コードが 401 でない: %d", got)
	}
	if got := post("GOOD0001"); got != 200 {
		t.Fatalf("誤コード試行後の正コードが 200 でない（誤って消費した）: %d", got)
	}
	if got := post("GOOD0001"); got != 401 {
		t.Fatalf("消費済コードが 401 でない: %d", got)
	}
}
