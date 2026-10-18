//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// npm の global 導入（`npm i -g @openai/codex` 等）は Windows では `codex.cmd`
// （＋.ps1・拡張子なし sh）だけを置き、`codex.exe` は無い。旧コードは `<name>.exe`
// しか探さず、npm 導入の agent をシムも update も見つけられなかった。herdr 0.9.5 は
// `.cmd` の argv[0] を agent と検出し引数も本体へ届く（TestWindowsHerdrAliasDetection）。
func TestLookupAgentBinFindsNpmCmdShim(t *testing.T) {
	setTestHome(t, t.TempDir())
	npm := t.TempDir()
	shim := filepath.Join(npm, "codex.cmd")
	if err := os.WriteFile(shim, []byte("@echo off\r\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// npm は同じ dir に拡張子なし（sh 用）と .ps1 も置く＝それらは実行対象にしない。
	for _, n := range []string{"codex", "codex.ps1"} {
		if err := os.WriteFile(filepath.Join(npm, n), []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", npm)
	got, err := lookupAgentBin("codex")
	if err != nil {
		t.Fatalf("npm の codex.cmd を見つけられない: %v", err)
	}
	if !strings.EqualFold(got, shim) {
		t.Fatalf("got %q want %q", got, shim)
	}
}

// 同じ dir に .exe と .cmd が両方あれば .exe（PATHEXT の既定順と同じ）。
func TestLookupAgentBinPrefersExeOverCmd(t *testing.T) {
	setTestHome(t, t.TempDir())
	dir := t.TempDir()
	for _, n := range []string{"claude.cmd", "claude.exe"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	got, err := lookupAgentBin("claude")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(filepath.Base(got), "claude.exe") {
		t.Fatalf("got %q want claude.exe", got)
	}
}
