package main

// enroll --slave の配置検証: 共用 PC は SA レスで config.json（role=slave・
// google_application_credentials 無し）＋slave.json（refresh secret・0600）を
// 置き、古い sa.json/clouds.json を掃除する。併せて master 経路の byte 不変
// （§8.6: POST は code のみ・sa 鍵配置・role/slave.json 非生成）を回帰で守る。
//
// 旧コード（--slave 未実装・cmdEnrollSlave 不在）では slave 経路が master 扱い
// になり slave.json が生成されず role も書かれず FAIL する（鉄則: 実装前 FAIL）。

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

// recordingEnrollServer は master/slave 双方の enroll を捌き、受信した form
// （code/pc/role）を記録する。slave（role=slave）には slave_secret を返し
// sa_json は返さない。master には sa_json を返す（既存 relay 契約）。
type recordedEnroll struct {
	mu   sync.Mutex
	code string
	pc   string
	role string
	used bool
}

func recordingEnrollServer(t *testing.T, code, project, relayURL, saJSON, slaveSecret string) (wsURL string, rec *recordedEnroll) {
	t.Helper()
	rec = &recordedEnroll{}
	mux := http.NewServeMux()
	mux.HandleFunc("/enroll", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST のみ", http.StatusMethodNotAllowed)
			return
		}
		_ = r.ParseForm()
		rec.mu.Lock()
		rec.code = r.PostFormValue("code")
		rec.pc = r.PostFormValue("pc")
		rec.role = r.PostFormValue("role")
		ok := rec.code == code && !rec.used
		if ok {
			rec.used = true
		}
		role := rec.role
		rec.mu.Unlock()
		if !ok {
			http.Error(w, "コードが無効か期限切れです", http.StatusUnauthorized)
			return
		}
		resp := map[string]any{"gcp_project": project, "relay_url": relayURL}
		if role == "slave" {
			resp["slave_secret"] = slaveSecret // SA 鍵は返さない
		} else if saJSON != "" {
			resp["sa_json"] = saJSON
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return "ws://" + strings.TrimPrefix(ts.URL, "http://"), rec
}

func TestEnrollSlavePlacesSaLessConfigAndSecret(t *testing.T) {
	clearDroverEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".herdr-drover")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// master→slave 再 enroll を模した残骸（掃除されることの検証）。
	if err := os.WriteFile(filepath.Join(dir, "sa.json"), []byte(`{"stale":"sa"}`), 0o600); err != nil {
		t.Fatalf("seed sa.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "clouds.json"), []byte(`[]`), 0o600); err != nil {
		t.Fatalf("seed clouds.json: %v", err)
	}

	wsURL, rec := recordingEnrollServer(t, "SLAVECODE", "proj-slave", "wss://relay.example", "", "deadbeefsecret")
	code, out, errb := runCapture(t, "enroll", "SLAVECODE", "--relay", wsURL, "--slave")
	if code != 0 {
		t.Fatalf("enroll --slave exit=%d\nstdout:%s\nstderr:%s", code, out, errb)
	}

	// POST form に pc/role が乗ったこと（relay の slaves/{pc} bind に必須）。
	rec.mu.Lock()
	gotPC, gotRole := rec.pc, rec.role
	rec.mu.Unlock()
	if gotRole != "slave" {
		t.Fatalf("slave POST に role=slave が無い: role=%q", gotRole)
	}
	if !strings.HasSuffix(gotPC, "-herdr") {
		t.Fatalf("slave POST の pc が -herdr でない: %q", gotPC)
	}

	// config.json: role=slave・SA 鍵キー無し。
	raw, err := readRawFileConfig(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("read config.json: %v", err)
	}
	if string(raw["role"]) != `"slave"` {
		t.Fatalf("config.json role != slave: %s", raw["role"])
	}
	if _, ok := raw["google_application_credentials"]; ok {
		t.Fatalf("SA レスのはずが google_application_credentials が残っている: %s", raw["google_application_credentials"])
	}
	if string(raw["gcp_project"]) != `"proj-slave"` {
		t.Fatalf("gcp_project 不一致: %s", raw["gcp_project"])
	}

	// slave.json: pc/refresh_secret/0600。
	slavePath := filepath.Join(dir, "slave.json")
	sb, err := os.ReadFile(slavePath)
	if err != nil {
		t.Fatalf("slave.json 未配置: %v", err)
	}
	var sf slaveFile
	if err := json.Unmarshal(sb, &sf); err != nil {
		t.Fatalf("slave.json 解析: %v", err)
	}
	if sf.RefreshSecret != "deadbeefsecret" {
		t.Fatalf("refresh_secret 不一致: %q", sf.RefreshSecret)
	}
	if sf.PC != gotPC {
		t.Fatalf("slave.json pc=%q が POST pc=%q と不一致", sf.PC, gotPC)
	}
	if fi, _ := os.Stat(slavePath); fi.Mode().Perm() != 0o600 {
		t.Fatalf("slave.json 権限が %v（0600 のはず）", fi.Mode().Perm())
	}

	// 残骸掃除: sa.json / clouds.json は消えていること（真に SA レス）。
	if _, err := os.Stat(filepath.Join(dir, "sa.json")); !os.IsNotExist(err) {
		t.Fatalf("古い sa.json が残っている（掃除されていない）")
	}
	if _, err := os.Stat(filepath.Join(dir, "clouds.json")); !os.IsNotExist(err) {
		t.Fatalf("古い clouds.json が残っている（掃除されていない）")
	}

	// resolveConfig が role=slave を拾うこと（agent が slave 経路に入る前提）。
	cfg, _ := resolveConfig()
	if cfg.Role != "slave" {
		t.Fatalf("enroll 後 resolveConfig().Role=%q（slave のはず）", cfg.Role)
	}
}

// §8.6 invariant: --slave 無しの master enroll は POST=code のみ・SA 鍵配置・
// role/slave.json を一切生成しない（byte 不変の回帰）。
func TestEnrollMasterUnchangedNoSlaveArtifacts(t *testing.T) {
	clearDroverEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	const saJSON = `{"fake":"sa"}`
	wsURL, rec := recordingEnrollServer(t, "MASTERCODE", "proj-master", "wss://relay.example", saJSON, "unused")

	code, out, errb := runCapture(t, "enroll", "MASTERCODE", "--relay", wsURL)
	if code != 0 {
		t.Fatalf("master enroll exit=%d\nstdout:%s\nstderr:%s", code, out, errb)
	}

	// POST form: code のみ（pc/role は空＝byte 不変）。
	rec.mu.Lock()
	gotPC, gotRole := rec.pc, rec.role
	rec.mu.Unlock()
	if gotPC != "" || gotRole != "" {
		t.Fatalf("master POST に slave 用フィールドが混入: pc=%q role=%q", gotPC, gotRole)
	}

	dir := filepath.Join(home, ".herdr-drover")
	// SA 鍵は配置される。
	if _, err := os.Stat(filepath.Join(dir, "sa.json")); err != nil {
		t.Fatalf("master enroll で sa.json 未配置: %v", err)
	}
	// slave.json は生成されない。
	if _, err := os.Stat(filepath.Join(dir, "slave.json")); !os.IsNotExist(err) {
		t.Fatalf("master enroll で slave.json が生成された（byte 不変違反）")
	}
	// config.json に role キーは無い・SA 鍵パスは有る。
	raw, err := readRawFileConfig(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("read config.json: %v", err)
	}
	if _, ok := raw["role"]; ok {
		t.Fatalf("master config.json に role キーが混入: %s", raw["role"])
	}
	if _, ok := raw["google_application_credentials"]; !ok {
		t.Fatalf("master config.json に SA 鍵パスが無い（byte 不変違反）")
	}
}
