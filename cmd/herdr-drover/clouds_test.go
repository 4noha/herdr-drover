package main

// clouds（端末ごとマルチ Google アカウント fan-out）設定の機械検証。
// 実 temp-HOME で clouds.json を隔離し、LoadClouds の env fallback / file 優先 /
// PCName 補完 / 不完全除外、AppendCloud の seed / dedupe(更新) / 追記を検証する
// （cm internal/config/clouds_test.go 相当）。herdr/Firestore 不要の純ファイル I/O。

import (
	"os"
	"path/filepath"
	"testing"
)

func cloudsTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

// clouds.json 不在 → env/config.json の単一クラウドにフォールバック（後方互換）。
func TestLoadCloudsEnvFallback(t *testing.T) {
	cloudsTestHome(t)
	if cs := (Config{}).LoadClouds(); len(cs) != 0 {
		t.Fatalf("Project 空で非空: %+v", cs)
	}
	cfg := Config{Project: "proj-a", RelayURL: "wss://a", Credentials: "/k.json", PCID: "pc-herdr"}
	cs := cfg.LoadClouds()
	if len(cs) != 1 || cs[0].Project != "proj-a" || cs[0].RelayURL != "wss://a" ||
		cs[0].SAKeyPath != "/k.json" || cs[0].PCName != "pc-herdr" {
		t.Fatalf("単一クラウド fallback が想定外: %+v", cs)
	}
}

// 単一クラウドの SAKeyPath 既定: Credentials 空 → sa.json（存在時）、明示 → それ。
func TestLoadCloudsDefaultSA(t *testing.T) {
	home := cloudsTestHome(t)
	base := Config{Project: "p", RelayURL: "wss://x"}
	if cs := base.LoadClouds(); len(cs) != 1 || cs[0].SAKeyPath != "" {
		t.Fatalf("SA 無しのはず: %+v", cs)
	}
	dir := filepath.Join(home, ".herdr-drover")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	sa := filepath.Join(dir, "sa.json")
	if err := os.WriteFile(sa, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if cs := base.LoadClouds(); cs[0].SAKeyPath != sa {
		t.Fatalf("sa.json が seed されない: %+v want %s", cs, sa)
	}
	withCred := Config{Project: "p", RelayURL: "wss://x", Credentials: "/explicit.json"}
	if cs := withCred.LoadClouds(); cs[0].SAKeyPath != "/explicit.json" {
		t.Fatalf("Credentials 明示が優先されない: %+v", cs)
	}
}

// clouds.json があればそれを使い、PCName 既定補完・不完全エントリ除外・file 優先。
func TestLoadCloudsFromFile(t *testing.T) {
	home := cloudsTestHome(t)
	dir := filepath.Join(home, ".herdr-drover")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	js := `[
	  {"project":"p1","relay_url":"wss://1","sa_key_path":"/1.json"},
	  {"project":"p2","relay_url":"wss://2"},
	  {"project":"p3"}
	]`
	if err := os.WriteFile(filepath.Join(dir, "clouds.json"), []byte(js), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Project: "env-proj", RelayURL: "wss://env", PCID: "pc-herdr"}
	cs := cfg.LoadClouds()
	if len(cs) != 2 { // p3 は relay_url 欠落で除外
		t.Fatalf("有効エントリ 2 のはず: %d (%+v)", len(cs), cs)
	}
	if cs[1].PCName != "pc-herdr" { // p2 は pc_name 欠落 → PCID 補完
		t.Fatalf("PCName 既定補完されない: %+v", cs[1])
	}
	if cloudsHaveProject(cs, "env-proj") {
		t.Fatal("clouds.json 優先のはずが env クラウドが混入")
	}
}

// AppendCloud: 初回は existing を seed、同 project は dedupe(更新)、原子追記。
func TestAppendCloud(t *testing.T) {
	cloudsTestHome(t)
	cfg := Config{Project: "env-proj", RelayURL: "wss://env", PCID: "pc-herdr"}
	env := Cloud{Project: "env-proj", RelayURL: "wss://env", PCName: "pc-herdr"}

	if err := cfg.AppendCloud(Cloud{Project: "proj-b", RelayURL: "wss://b", SAKeyPath: "/b.json", PCName: "pc-herdr"}, []Cloud{env}); err != nil {
		t.Fatal(err)
	}
	cs := cfg.LoadClouds()
	if len(cs) != 2 || !cloudsHaveProject(cs, "env-proj") || !cloudsHaveProject(cs, "proj-b") {
		t.Fatalf("初回 seed+追加が想定外: %+v", cs)
	}

	if err := cfg.AppendCloud(Cloud{Project: "proj-b", RelayURL: "wss://b2", PCName: "pc-herdr"}, nil); err != nil {
		t.Fatal(err)
	}
	cs = cfg.LoadClouds()
	if len(cs) != 2 {
		t.Fatalf("同 project は更新のはず（重複追加された）: %+v", cs)
	}
	for _, c := range cs {
		if c.Project == "proj-b" && c.RelayURL != "wss://b2" {
			t.Fatalf("proj-b が更新されていない: %+v", c)
		}
	}

	if err := cfg.AppendCloud(Cloud{Project: "proj-c", RelayURL: "wss://c", PCName: "pc-herdr"}, nil); err != nil {
		t.Fatal(err)
	}
	if cs = cfg.LoadClouds(); len(cs) != 3 {
		t.Fatalf("3 つ目追加が想定外: %+v", cs)
	}
}

func TestCloudsHelpers(t *testing.T) {
	if fileExists("") || fileExists("/nonexistent-xyz-123-drover") {
		t.Fatal("fileExists 誤判定")
	}
	cs := []Cloud{{Project: "a"}, {Project: "b"}}
	if !cloudsHaveProject(cs, "a") || cloudsHaveProject(cs, "z") {
		t.Fatal("cloudsHaveProject 誤判定")
	}
}
