package main

// DROVER_MIRROR_AGENTS / mirror_agents（config.json）の解決を検証する。
// HOME を隔離して実 ~/.herdr-drover/config.json を読まない。

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConfigJSON(t *testing.T, home, body string) {
	t.Helper()
	dir := filepath.Join(home, ".herdr-drover")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestResolveConfigMirrorAgents(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home) // configFilePath は os.UserHomeDir()=$HOME を使う

	// 既定 OFF（env も file も無し）。
	t.Setenv("DROVER_MIRROR_AGENTS", "")
	if cfg, err := resolveConfig(); err != nil {
		t.Fatalf("resolveConfig: %v", err)
	} else if cfg.MirrorAgents {
		t.Fatal("既定は false のはず")
	}

	// env=true → ON。
	t.Setenv("DROVER_MIRROR_AGENTS", "true")
	if cfg, _ := resolveConfig(); !cfg.MirrorAgents {
		t.Fatal("DROVER_MIRROR_AGENTS=true で ON のはず")
	}

	// env=0 → OFF。
	t.Setenv("DROVER_MIRROR_AGENTS", "0")
	if cfg, _ := resolveConfig(); cfg.MirrorAgents {
		t.Fatal("DROVER_MIRROR_AGENTS=0 で OFF のはず")
	}

	// 不正値 → error（silent に既定へ倒さない）。
	t.Setenv("DROVER_MIRROR_AGENTS", "maybe")
	if _, err := resolveConfig(); err == nil {
		t.Fatal("不正値でエラーになるはず")
	}

	// file=true・env 無し → ON（file を採用）。
	t.Setenv("DROVER_MIRROR_AGENTS", "")
	writeConfigJSON(t, home, `{"mirror_agents": true}`)
	if cfg, _ := resolveConfig(); !cfg.MirrorAgents {
		t.Fatal("mirror_agents:true(file) で ON のはず")
	}

	// env=false が file=true を上書き（env > file）。
	t.Setenv("DROVER_MIRROR_AGENTS", "false")
	if cfg, _ := resolveConfig(); cfg.MirrorAgents {
		t.Fatal("env=false が file=true を上書きするはず")
	}
}
