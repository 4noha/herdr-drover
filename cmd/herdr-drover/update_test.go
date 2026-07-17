package main

// update の稼働バイナリ同期テスト。
//
// 【指摘再現】手動 `herdr-drover update` は os.Executable（例:
// ~/.local/bin/herdr-drover）しか置換しないのに「launchctl kickstart -k で
// 反映」と案内していた。launchd の Program は ~/.herdr-drover/bin/
// herdr-drover（install.go binDst＝開発ビルド分離のための別コピー）なので、
// kickstart は旧バイナリを再 exec する＝ユーザーは更新済みと信じたまま
// daemon が旧版で走り続ける（cm の「旧 inode proxy 旧版🔴」と同型）。
//
// selfupdate.Update は実 GitHub へ出るため seam（updateSelf/
// updateExecutable）で差し替え、「置換後の稼働バイナリ同期」だけを
// 実ファイル・実 run() 経路で検証する（selfupdate 本体の HTTP/sha256/
// 原子置換は internal/selfupdate のローカル fixture テストが実経路で担保）。

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// setUpdateSeams は seam を差し替え、テスト終了時に復元する。
func setUpdateSeams(t *testing.T, exe string, doUpdate func(string) (string, bool, error)) {
	t.Helper()
	origSelf, origExe := updateSelf, updateExecutable
	t.Cleanup(func() { updateSelf, updateExecutable = origSelf, origExe })
	updateExecutable = func() (string, error) { return exe, nil }
	updateSelf = doUpdate
}

func TestUpdateSyncsDaemonBinary(t *testing.T) {
	clearDroverEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	p := resolveInstallPaths(home)

	// 稼働バイナリ（旧版）＝launchd Program の実体。
	if err := os.MkdirAll(filepath.Dir(p.binDst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.binDst, []byte("OLD-DAEMON"), 0o755); err != nil {
		t.Fatal(err)
	}
	st1, err := os.Stat(p.binDst)
	if err != nil {
		t.Fatal(err)
	}
	ino1 := st1.Sys().(*syscall.Stat_t).Ino

	// 手動 update の自バイナリ（~/.local/bin 相当＝binDst と別パス）。
	exe := filepath.Join(home, "local-bin", "herdr-drover")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("OLD-SELF"), 0o755); err != nil {
		t.Fatal(err)
	}
	// seam: selfupdate.Update の replaceSelf 相当（自バイナリ置換）を模す。
	setUpdateSeams(t, exe, func(string) (string, bool, error) {
		if err := os.WriteFile(exe, []byte("NEW-BIN"), 0o755); err != nil {
			return "", false, err
		}
		return "v9.9.9", true, nil
	})

	code, out, errb := runCapture(t, "update")
	if code != 0 {
		t.Fatalf("update exit=%d stderr=%q", code, errb)
	}
	// ここが指摘の核心: 稼働バイナリも新版へ同期されていること。
	b, err := os.ReadFile(p.binDst)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "NEW-BIN" {
		t.Fatalf("稼働バイナリが旧版のまま（kickstart しても旧バイナリを再 exec）: %q", b)
	}
	// rm→新 inode 配置（macOS 署名キャッシュ罠＝同 inode 上書きは exec SIGKILL）。
	st2, err := os.Stat(p.binDst)
	if err != nil {
		t.Fatal(err)
	}
	if ino2 := st2.Sys().(*syscall.Stat_t).Ino; ino1 == ino2 {
		t.Fatalf("稼働バイナリが同 inode のまま（署名キャッシュ罠）: ino=%d", ino1)
	}
	// 同期の報告と kickstart 案内（同期後なら案内は正しい手順になる）。
	if !strings.Contains(out, p.binDst) || !strings.Contains(out, "kickstart") {
		t.Fatalf("稼働バイナリ同期の報告と kickstart 案内が無い: %q", out)
	}
}

// binDst 不在（launchd 未 install）なら同期対象なし＝kickstart 案内は出さない
// （存在しない常駐への誤誘導を防ぐ）。update 自体は成功。
func TestUpdateWithoutDaemonBinary(t *testing.T) {
	clearDroverEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	exe := filepath.Join(home, "local-bin", "herdr-drover")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("NEW-BIN"), 0o755); err != nil {
		t.Fatal(err)
	}
	setUpdateSeams(t, exe, func(string) (string, bool, error) { return "v9.9.9", true, nil })

	code, out, errb := runCapture(t, "update")
	if code != 0 {
		t.Fatalf("update exit=%d stderr=%q", code, errb)
	}
	if strings.Contains(out, "kickstart") {
		t.Fatalf("launchd 未 install なのに kickstart 案内が出た（誤誘導）: %q", out)
	}
}

// 自バイナリ＝稼働バイナリ（binDst 直接実行や agent 経由の self-update と
// 同型）なら再配置は不要（selfupdate が既に新 inode 配置済み）。上書きで
// 壊さないこと・kickstart 案内は出ることを確認。
func TestUpdateFromDaemonBinaryItself(t *testing.T) {
	clearDroverEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	p := resolveInstallPaths(home)
	if err := os.MkdirAll(filepath.Dir(p.binDst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.binDst, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	setUpdateSeams(t, p.binDst, func(string) (string, bool, error) {
		// replaceSelf 相当＝binDst 自身が新版になる。
		if err := os.WriteFile(p.binDst, []byte("NEW-BIN"), 0o755); err != nil {
			return "", false, err
		}
		return "v9.9.9", true, nil
	})

	code, out, errb := runCapture(t, "update")
	if code != 0 {
		t.Fatalf("update exit=%d stderr=%q", code, errb)
	}
	b, err := os.ReadFile(p.binDst)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "NEW-BIN" {
		t.Fatalf("binDst 自身からの update で内容が壊れた: %q", b)
	}
	if !strings.Contains(out, "kickstart") {
		t.Fatalf("kickstart 案内が無い: %q", out)
	}
}

// 既に最新なら何も配置せず成功（従来挙動の非回帰）。
func TestUpdateAlreadyLatest(t *testing.T) {
	clearDroverEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	exe := filepath.Join(home, "local-bin", "herdr-drover")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("SELF"), 0o755); err != nil {
		t.Fatal(err)
	}
	setUpdateSeams(t, exe, func(string) (string, bool, error) { return version, false, nil })
	code, out, errb := runCapture(t, "update")
	if code != 0 || !strings.Contains(out, "既に最新") {
		t.Fatalf("exit=%d out=%q stderr=%q", code, out, errb)
	}
}
