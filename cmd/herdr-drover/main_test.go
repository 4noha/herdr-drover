package main

// 引数 dispatch と設定解決の単体テスト（daemon 全体の e2e は統合フェーズ）。
// dispatch は実バイナリと同じ run() を直接呼ぶ＝合成の別経路を作らない。

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/4noha/herdr-drover/internal/herdrapi"
)

func runCapture(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestDispatchVersion(t *testing.T) {
	code, out, _ := runCapture(t, "version")
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if !strings.Contains(out, version) {
		t.Fatalf("version 出力に %q が無い: %q", version, out)
	}
}

func TestDispatchHelp(t *testing.T) {
	code, out, _ := runCapture(t, "help")
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	for _, want := range []string{"agent", "status", "nudge", "GCP_PROJECT", "DROVER_TICK"} {
		if !strings.Contains(out, want) {
			t.Fatalf("usage に %q が無い: %q", want, out)
		}
	}
}

func TestDispatchNoArgs(t *testing.T) {
	code, _, errb := runCapture(t)
	if code != 2 {
		t.Fatalf("exit=%d want 2", code)
	}
	if !strings.Contains(errb, "使い方") {
		t.Fatalf("usage が stderr に出ていない: %q", errb)
	}
}

func TestDispatchUnknown(t *testing.T) {
	code, _, errb := runCapture(t, "bogus")
	if code != 2 {
		t.Fatalf("exit=%d want 2", code)
	}
	if !strings.Contains(errb, "未知のサブコマンド") || !strings.Contains(errb, "bogus") {
		t.Fatalf("未知コマンドの明示エラーが無い: %q", errb)
	}
}

// 未実装コマンドは黙って no-op にせず明示エラー（任務要件）。
// install/uninstall は launchd フェーズで実装済み＝このリストから卒業
// （実装テストは install_test.go）。
func TestDispatchUnimplemented(t *testing.T) {
	for _, cmd := range []string{"attach"} {
		code, _, errb := runCapture(t, cmd)
		if code != 2 {
			t.Fatalf("%s: exit=%d want 2", cmd, code)
		}
		if !strings.Contains(errb, "未実装") {
			t.Fatalf("%s: 未実装エラーが無い: %q", cmd, errb)
		}
	}
}

func TestDispatchRejectsExtraArgs(t *testing.T) {
	code, _, errb := runCapture(t, "nudge", "now")
	if code != 2 {
		t.Fatalf("exit=%d want 2", code)
	}
	if !strings.Contains(errb, "余分な引数") {
		t.Fatalf("余分引数の明示エラーが無い: %q", errb)
	}
}

// ============ 設定解決 ============

// clearDroverEnv は本テストが読む環境変数を全て空にする（t.Setenv の
// 空文字は「未設定」と同義に扱う実装）。HOME も一時 dir へ隔離する:
// resolveConfig が ~/.herdr-drover/config.json（enroll の永続設定）を
// 読むようになったため、実 HOME の設定ファイルでテスト結果が変わらない
// ようにする（hermetic）。
func clearDroverEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"GCP_PROJECT", "CLOUD_RELAY_URL", "GOOGLE_APPLICATION_CREDENTIALS",
		"PC_ID", "HERDR_SOCKET_PATH", "DROVER_TICK", "DROVER_IDLE",
		"FIRESTORE_EMULATOR_HOST",
	} {
		t.Setenv(k, "")
	}
	t.Setenv("HOME", t.TempDir())
}

func TestResolveConfigDefaults(t *testing.T) {
	clearDroverEnv(t)
	cfg, err := resolveConfig()
	if err != nil {
		t.Fatalf("resolveConfig: %v", err)
	}
	if cfg.Tick != defaultTick {
		t.Fatalf("tick 既定: got %s want %s", cfg.Tick, defaultTick)
	}
	if cfg.Idle != 0 {
		// 0=「bridge 既定 30s に委譲」（webterm.go が b.Idle へそのまま渡し、
		// bridge.Run が 0 を DefaultIdle に解決する契約）。
		t.Fatalf("idle 既定: got %s want 0（bridge 既定へ委譲）", cfg.Idle)
	}
	if !strings.HasSuffix(cfg.PCID, "-herdr") {
		t.Fatalf("PC_ID 既定に -herdr サフィックスが無い: %q", cfg.PCID)
	}
	if cfg.PCID != strings.ToLower(cfg.PCID) {
		t.Fatalf("PC_ID 既定が小文字でない: %q", cfg.PCID)
	}
	if cfg.Project != "" || cfg.RelayURL != "" || cfg.Credentials != "" {
		t.Fatalf("未設定 env が空になっていない: %+v", cfg)
	}
}

func TestResolveConfigEnvOverrides(t *testing.T) {
	clearDroverEnv(t)
	t.Setenv("GCP_PROJECT", "proj-x")
	t.Setenv("CLOUD_RELAY_URL", "wss://relay.example/session")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "/tmp/sa.json")
	t.Setenv("PC_ID", "custom-herdr")
	t.Setenv("HERDR_SOCKET_PATH", "/tmp/hx.sock")
	t.Setenv("DROVER_TICK", "750ms")
	t.Setenv("DROVER_IDLE", "3s")
	cfg, err := resolveConfig()
	if err != nil {
		t.Fatalf("resolveConfig: %v", err)
	}
	if cfg.Project != "proj-x" || cfg.RelayURL != "wss://relay.example/session" ||
		cfg.Credentials != "/tmp/sa.json" || cfg.PCID != "custom-herdr" ||
		cfg.SocketPath != "/tmp/hx.sock" || cfg.Tick != 750*time.Millisecond ||
		cfg.Idle != 3*time.Second {
		t.Fatalf("env 上書きが反映されていない: %+v", cfg)
	}
}

func TestResolveConfigBadTick(t *testing.T) {
	clearDroverEnv(t)
	for _, bad := range []string{"abc", "-1s", "0s"} {
		t.Setenv("DROVER_TICK", bad)
		if _, err := resolveConfig(); err == nil {
			t.Fatalf("DROVER_TICK=%q でエラーにならない", bad)
		}
	}
}

// DROVER_IDLE の負値/0 は quiescence 無効化＝near-$0 設計破壊なので拒否
// （config.go のコメント参照。無効化はテスト専用の bridge.Idle 直接指定のみ）。
func TestResolveConfigBadIdle(t *testing.T) {
	clearDroverEnv(t)
	for _, bad := range []string{"abc", "-1s", "0s"} {
		t.Setenv("DROVER_IDLE", bad)
		if _, err := resolveConfig(); err == nil {
			t.Fatalf("DROVER_IDLE=%q でエラーにならない", bad)
		}
	}
}

func TestDefaultPCID(t *testing.T) {
	cases := map[string]string{
		"Mac-Studio.local": "mac-studio-herdr", // 短縮＋小文字化
		"linuxbox":         "linuxbox-herdr",
	}
	for in, want := range cases {
		if got := defaultPCID(in); got != want {
			t.Fatalf("defaultPCID(%q)=%q want %q", in, got, want)
		}
	}
}

func TestWarnConfig(t *testing.T) {
	clearDroverEnv(t)
	t.Setenv("FIRESTORE_EMULATOR_HOST", "127.0.0.1:9999") // creds 警告を消す

	// -herdr サフィックス無し＝削除合戦リスクの警告（DESIGN 決定事項）
	ws := warnConfig(Config{PCID: "mac-studio", RelayURL: "wss://x"})
	if len(ws) != 1 || !strings.Contains(ws[0], "-herdr") {
		t.Fatalf("PC_ID サフィックス警告が無い/多い: %v", ws)
	}
	// 全て充足なら警告ゼロ
	if ws := warnConfig(Config{PCID: "mac-studio-herdr", RelayURL: "wss://x"}); len(ws) != 0 {
		t.Fatalf("充足設定で警告が出る: %v", ws)
	}
	// relay 未設定は Phase 2 警告
	ws = warnConfig(Config{PCID: "a-herdr"})
	if len(ws) != 1 || !strings.Contains(ws[0], "CLOUD_RELAY_URL") {
		t.Fatalf("relay 警告が無い/多い: %v", ws)
	}
}

func TestVerifyHerdrProtocol(t *testing.T) {
	// 実測値（v0.7.4=protocol16）は警告なし・exact-match
	if ws := verifyHerdr(&herdrapi.PongInfo{Version: "0.7.4", Protocol: knownProtocol}); len(ws) != 0 {
		t.Fatalf("既知 protocol で警告: %v", ws)
	}
	ws := verifyHerdr(&herdrapi.PongInfo{Version: "9.9.9", Protocol: 17})
	if len(ws) != 1 || !strings.Contains(ws[0], "17") {
		t.Fatalf("未知 protocol の警告が無い: %v", ws)
	}
}
