package main

// slave 用 config.Role の解決テスト（env HERDR_ROLE > file role > 既定 ""）。
// 旧コード（Config/fileConfig に Role が無い・resolveConfig が role を読まない）
// では cfg.Role が常に "" で以下が FAIL する（鉄則: 実装前に FAIL を実確認）。

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveConfigRoleDefaultEmpty(t *testing.T) {
	clearDroverEnv(t)
	cfg, err := resolveConfig()
	if err != nil {
		t.Fatalf("resolveConfig: %v", err)
	}
	if cfg.Role != "" {
		t.Fatalf("role 既定は空（master）のはず: got %q", cfg.Role)
	}
}

func TestResolveConfigRoleFromFile(t *testing.T) {
	clearDroverEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeTestConfigFile(t, home, `{
  "gcp_project": "proj-slave",
  "cloud_relay_url": "wss://relay.example",
  "role": "slave"
}`)
	cfg, err := resolveConfig()
	if err != nil {
		t.Fatalf("resolveConfig: %v", err)
	}
	if cfg.Role != "slave" {
		t.Fatalf("file の role=slave を拾えていない: got %q", cfg.Role)
	}
}

func TestResolveConfigRoleEnvOverridesFile(t *testing.T) {
	clearDroverEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeTestConfigFile(t, home, `{"role":"slave"}`)
	t.Setenv("HERDR_ROLE", "master")
	cfg, err := resolveConfig()
	if err != nil {
		t.Fatalf("resolveConfig: %v", err)
	}
	// env が file を上書きする（env > file の一般規律）。
	if cfg.Role != "master" {
		t.Fatalf("env HERDR_ROLE が file role を上書きしていない: got %q", cfg.Role)
	}
}

// 既存 config.json に role を書いても他キーが壊れない（learn_moves 等の未知
// キーは enroll 側の生 map 経路が守る＝ここは fileConfig decode の健全性）。
func TestResolveConfigRoleCoexistsWithOtherKeys(t *testing.T) {
	clearDroverEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".herdr-drover")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := `{"gcp_project":"p","pc_id":"x-herdr","role":"slave","learn_moves":true}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := resolveConfig()
	if err != nil {
		t.Fatalf("resolveConfig: %v", err)
	}
	if cfg.Role != "slave" || cfg.PCID != "x-herdr" || cfg.Project != "p" {
		t.Fatalf("キー解決不整合: %+v", cfg)
	}
}
