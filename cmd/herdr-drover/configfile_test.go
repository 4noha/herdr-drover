package main

// 設定ファイル（~/.herdr-drover/config.json・enroll が書く）の優先順位
// env > file > default の単体テスト。旧コード（env のみの resolveConfig）
// では file の値が一切反映されず FAIL する（鉄則: 実装前に FAIL を実確認）。

import (
	"os"
	"path/filepath"
	"testing"
)

// writeTestConfigFile は隔離 HOME 配下に config.json を置く。
func writeTestConfigFile(t *testing.T, home, body string) string {
	t.Helper()
	dir := filepath.Join(home, ".herdr-drover")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config.json: %v", err)
	}
	return path
}

func TestResolveConfigReadsFile(t *testing.T) {
	clearDroverEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeTestConfigFile(t, home, `{
  "gcp_project": "proj-file",
  "cloud_relay_url": "wss://file.example",
  "google_application_credentials": "/x/sa-file.json",
  "pc_id": "filepc-herdr"
}`)
	cfg, err := resolveConfig()
	if err != nil {
		t.Fatalf("resolveConfig: %v", err)
	}
	if cfg.Project != "proj-file" || cfg.RelayURL != "wss://file.example" ||
		cfg.Credentials != "/x/sa-file.json" || cfg.PCID != "filepc-herdr" {
		t.Fatalf("file の値が反映されない: %+v", cfg)
	}
}

func TestResolveConfigEnvBeatsFile(t *testing.T) {
	clearDroverEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeTestConfigFile(t, home, `{"gcp_project":"proj-file","cloud_relay_url":"wss://file.example"}`)
	t.Setenv("GCP_PROJECT", "proj-env")
	cfg, err := resolveConfig()
	if err != nil {
		t.Fatalf("resolveConfig: %v", err)
	}
	if cfg.Project != "proj-env" {
		t.Fatalf("env > file が破れている: Project=%q", cfg.Project)
	}
	// env に無いキーは file から補完される（キー単位のフォールバック）。
	if cfg.RelayURL != "wss://file.example" {
		t.Fatalf("env に無いキーが file から補完されない: RelayURL=%q", cfg.RelayURL)
	}
}

func TestResolveConfigFileAbsentIsDefault(t *testing.T) {
	clearDroverEnv(t)
	t.Setenv("HOME", t.TempDir()) // file 無し
	cfg, err := resolveConfig()
	if err != nil {
		t.Fatalf("resolveConfig: %v", err)
	}
	if cfg.Project != "" || cfg.RelayURL != "" || cfg.Credentials != "" {
		t.Fatalf("file 不在で default になっていない: %+v", cfg)
	}
}

func TestResolveConfigFileMalformed(t *testing.T) {
	clearDroverEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeTestConfigFile(t, home, `{broken json`)
	t.Setenv("GCP_PROJECT", "proj-env")
	cfg, err := resolveConfig()
	if err == nil {
		t.Fatalf("壊れた config.json でエラーが返らない（沈黙は事故の種）")
	}
	// 契約: エラー時も判明した分（env）は埋めて返す＝status が表示を続行できる。
	if cfg.Project != "proj-env" {
		t.Fatalf("エラー時に env 判明分が埋まっていない: %+v", cfg)
	}
}
