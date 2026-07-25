package main

// herdr server の自動起動時に「呼び出し元固有の環境変数」を引き継がないこと。
//
// ⚠これが破れると **そのマシンの claude セッションが全て resume 不能**になる。
// herdr は pane を server の env で起こすので、server が
// CLAUDE_CODE_CHILD_SESSION を持つと全 pane が継承し、claude が
// 「子セッション」とみなして transcript を保存しない → --resume が読むものが無い。
//
// 実障害（2026-07-18 に混入・07-25 に発覚）: Claude Code の中からシムが呼ばれ、
// herdr 未起動だったため親の env をそのまま渡して常駐させてしまった。

import (
	"bytes"
	"github.com/4noha/herdr-drover/internal/herdrapi"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestSanitizedServerEnvDropsCallerMarkers(t *testing.T) {
	in := []string{
		"PATH=/usr/bin",
		"HOME=/Users/x",
		"HERDR_SOCKET_PATH=/tmp/h.sock", // 透過が必要（隔離テストの前提）
		"XDG_CONFIG_HOME=/tmp/cfg",      // 透過が必要
		"CLAUDE_CODE_CHILD_SESSION=1",   // 落とす
		"CLAUDE_CODE_SESSION_ID=abc",    // 落とす
		"CLAUDE_CODE_ENTRYPOINT=cli",    // 落とす
		"CLAUDE_CODE_SSE_PORT=1234",     // 落とす
		"CLAUDE_CODE_UNRELATED=keep",    // 前方一致では落とさない（exact のみ）
	}
	var log bytes.Buffer
	got := sanitizedServerEnv(in, &log)
	has := func(k string) bool {
		for _, kv := range got {
			if strings.HasPrefix(kv, k+"=") {
				return true
			}
		}
		return false
	}
	for _, k := range serverEnvBlocklist {
		if has(k) {
			t.Errorf("%s が残った（常駐サーバに引き継ぐと全 pane が継承する）", k)
		}
	}
	for _, k := range []string{"PATH", "HOME", "HERDR_SOCKET_PATH", "XDG_CONFIG_HOME"} {
		if !has(k) {
			t.Errorf("%s が落ちた（透過が必要）", k)
		}
	}
	// **exact-match で落とす**（前方一致にすると無関係な変数まで巻き込む）。
	if !has("CLAUDE_CODE_UNRELATED") {
		t.Error("blocklist に無い CLAUDE_CODE_* まで落とした（exact-match のはず）")
	}
	// silent に環境を変えない（鉄則⑤）。
	for _, k := range serverEnvBlocklist {
		if !strings.Contains(log.String(), k) {
			t.Errorf("落とした %s が報告されていない: %s", k, log.String())
		}
	}
}

// 落とすものが無ければ何も出さない（毎回ログを汚さない）。
func TestSanitizedServerEnvQuietWhenClean(t *testing.T) {
	var log bytes.Buffer
	got := sanitizedServerEnv([]string{"PATH=/usr/bin", "HOME=/Users/x"}, &log)
	if len(got) != 2 {
		t.Fatalf("env が変わった: %v", got)
	}
	if log.Len() != 0 {
		t.Fatalf("落とすものが無いのに出力した: %s", log.String())
	}
}

// シムが**実際に自動起動した** herdr server がマーカーを持たないこと（e2e）。
// 純関数テストだけだと「配線を外しても緑」になるので、実プロセスで確かめる。
func TestAutoStartedServerHasNoCallerMarkers(t *testing.T) {
	if os.Getenv("CLAUDE_CODE_CHILD_SESSION") == "" {
		t.Skip("親に CHILD マーカーが無い＝この環境では混入を再現できない")
	}
	// 隔離した socket パス（sun_path 104B 制約のため短い /tmp を使う）。
	dir, err := os.MkdirTemp("/tmp", "se")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "h.sock")
	t.Setenv("HERDR_SOCKET_PATH", sock)
	t.Setenv("XDG_CONFIG_HOME", dir)

	api := herdrapi.New(sock)
	var log bytes.Buffer
	if err := ensureHerdrServer(api, &log); err != nil {
		t.Fatalf("自動起動に失敗: %v\n%s", err, log.String())
	}
	pid := lastSpawnedServerPID
	if pid == 0 {
		t.Fatal("spawn した PID が記録されていない")
	}
	// ⚠**自分が spawn した PID だけ**を対象に落とす（裸の pkill 禁止の恒久規律）。
	t.Cleanup(func() {
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Kill()
		}
	})
	t.Logf("自動起動した herdr server pid=%d\n%s", pid, strings.TrimSpace(log.String()))

	out, err := exec.Command("ps", "eww", "-p", strconv.Itoa(pid)).CombinedOutput()
	if err != nil {
		t.Fatalf("ps: %v %s", err, out)
	}
	for _, k := range serverEnvBlocklist {
		if strings.Contains(string(out), k+"=") {
			t.Errorf("自動起動したサーバが %s を持っている"+
				"（このサーバから生える全 pane が継承し resume が壊れる）", k)
		}
	}
	// 透過が必要なものは残っていること（socket を渡せないと隔離が壊れる）。
	if !strings.Contains(string(out), "HERDR_SOCKET_PATH=") {
		t.Error("HERDR_SOCKET_PATH が渡っていない（隔離が効かない）")
	}
}
