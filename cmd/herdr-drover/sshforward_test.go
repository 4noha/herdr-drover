//go:build unix

package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestIsSSHForwardSid(t *testing.T) {
	cases := map[string]bool{
		"afwd:deadbeef":     true,
		"afwd:repoA-verify": true,
		"afwd:":             true, // prefix のみでも forward 扱い（空 tail は上位で作らない）
		"w1:p2":             false,
		"w1:p2#inj":         false,
		"":                  false,
		"xafwd:y":           false, // 先頭一致のみ（部分一致で誤爆しない）
	}
	for sid, want := range cases {
		if got := isSSHForwardSid(sid); got != want {
			t.Errorf("isSSHForwardSid(%q)=%v want %v", sid, got, want)
		}
	}
}

func TestSSHForwardSockNameSanitizes(t *testing.T) {
	cases := map[string]string{
		"afwd:deadbeef":     "afwd-deadbeef",
		"afwd:repoA-verify": "afwd-repoA-verify", // - は保持
		"afwd:a/b c:d":      "afwd-a-b-c-d",      // / space : は - へ
	}
	for in, want := range cases {
		if got := sshForwardSockName(in); got != want {
			t.Errorf("sshForwardSockName(%q)=%q want %q", in, got, want)
		}
	}
}

func TestSSHForwardSockPathAndDisplayAgree(t *testing.T) {
	afSid := "afwd:0123456789abcdef"

	abs, err := sshForwardSockPath(afSid)
	if err != nil {
		t.Fatalf("sshForwardSockPath: %v", err)
	}
	disp := sshForwardSockDisplay(afSid)

	// owner が表示する ~ パスと slave が作る実体は **同じ basename**
	// （slave のシェルが ~ を展開＝同一ファイルになる不変条件）。
	if filepath.Base(abs) != filepath.Base(disp) {
		t.Fatalf("basename 不一致: abs=%q disp=%q", abs, disp)
	}
	if !strings.HasPrefix(disp, "~/.herdr-drover/agent-fwd/") {
		t.Fatalf("display が ~ 相対でない: %q", disp)
	}
	if !strings.HasSuffix(abs, ".sock") {
		t.Fatalf("abs が .sock で終わらない: %q", abs)
	}
	// macOS の sun_path 104 byte 上限に収まること（実 home で）。
	if len(abs) >= 104 {
		t.Fatalf("socket パスが 104 byte 以上（macOS で bind 不可）: len=%d %q", len(abs), abs)
	}
}
