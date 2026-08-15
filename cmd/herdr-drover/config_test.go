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
	setTestHome(t, home) // configFilePath は os.UserHomeDir()=$HOME を使う

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

// TestResolveConfigInjectRemotePanes は DROVER_INJECT_REMOTE / inject_remote_panes の
// 解決を検証する（BUG-2）。MirrorAgents と違い **既定 true=opt-out**（既存挙動＝注入
// 継続を変えない・false で明示的に止める）。
func TestResolveConfigInjectRemotePanes(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)

	// 既定 ON（env も file も無し）＝注入は既定で継続（後方互換）。
	t.Setenv("DROVER_INJECT_REMOTE", "")
	if cfg, err := resolveConfig(); err != nil {
		t.Fatalf("resolveConfig: %v", err)
	} else if !cfg.InjectRemotePanes {
		t.Fatal("既定は true のはず（opt-out）")
	}

	// env=false → OFF（注入停止）。
	t.Setenv("DROVER_INJECT_REMOTE", "false")
	if cfg, _ := resolveConfig(); cfg.InjectRemotePanes {
		t.Fatal("DROVER_INJECT_REMOTE=false で OFF のはず")
	}

	// env=0 → OFF。
	t.Setenv("DROVER_INJECT_REMOTE", "0")
	if cfg, _ := resolveConfig(); cfg.InjectRemotePanes {
		t.Fatal("DROVER_INJECT_REMOTE=0 で OFF のはず")
	}

	// 不正値 → error（silent に既定へ倒さない）。
	t.Setenv("DROVER_INJECT_REMOTE", "maybe")
	if _, err := resolveConfig(); err == nil {
		t.Fatal("不正値でエラーになるはず")
	}

	// file=false・env 無し → OFF（file を採用）。
	t.Setenv("DROVER_INJECT_REMOTE", "")
	writeConfigJSON(t, home, `{"inject_remote_panes": false}`)
	if cfg, _ := resolveConfig(); cfg.InjectRemotePanes {
		t.Fatal("inject_remote_panes:false(file) で OFF のはず")
	}

	// env=true が file=false を上書き（env > file）。
	t.Setenv("DROVER_INJECT_REMOTE", "true")
	if cfg, _ := resolveConfig(); !cfg.InjectRemotePanes {
		t.Fatal("env=true が file=false を上書きするはず")
	}
}

// TestResolveConfigWebImagePaste は DROVER_WEB_IMAGE_PASTE / web_image_paste の
// 解決を検証する。**既定 false=opt-in**（cm の WebImagePaste と同じ）で、
// **role=slave では設定を無視して強制 false**（共用 PC のクリップボードは同一
// アカウントの他人が読める＝DESIGN_SLAVE の脅威モデル）。
func TestResolveConfigWebImagePaste(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	t.Setenv("HERDR_ROLE", "")

	// 既定 OFF（env も file も無し）。
	t.Setenv("DROVER_WEB_IMAGE_PASTE", "")
	if cfg, err := resolveConfig(); err != nil {
		t.Fatalf("resolveConfig: %v", err)
	} else if cfg.WebImagePaste {
		t.Fatal("既定は false のはず（opt-in）")
	}

	// env=true → ON。
	t.Setenv("DROVER_WEB_IMAGE_PASTE", "true")
	if cfg, _ := resolveConfig(); !cfg.WebImagePaste {
		t.Fatal("DROVER_WEB_IMAGE_PASTE=true で ON のはず")
	}

	// 不正値はエラー（silent に既定へ落とさない）。
	t.Setenv("DROVER_WEB_IMAGE_PASTE", "perhaps")
	if _, err := resolveConfig(); err == nil {
		t.Fatal("不正値がエラーになっていない")
	}

	// file(web_image_paste)=true・env 未設定 → ON。
	t.Setenv("DROVER_WEB_IMAGE_PASTE", "")
	writeConfigJSON(t, home, `{"web_image_paste": true}`)
	if cfg, _ := resolveConfig(); !cfg.WebImagePaste {
		t.Fatal("file の web_image_paste=true が効いていない")
	}

	// env が file に勝つ（env=false）。
	t.Setenv("DROVER_WEB_IMAGE_PASTE", "false")
	if cfg, _ := resolveConfig(); cfg.WebImagePaste {
		t.Fatal("env が file に優先していない")
	}

	// ⚠ role=slave は設定より強い（env=true でも強制 false）。
	t.Setenv("DROVER_WEB_IMAGE_PASTE", "true")
	t.Setenv("HERDR_ROLE", "slave")
	if cfg, _ := resolveConfig(); cfg.WebImagePaste {
		t.Fatal("slave では強制 false のはず（共用 PC のクリップボードは他人も読める）")
	}
}
