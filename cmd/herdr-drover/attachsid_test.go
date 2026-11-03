package main

import "testing"

func TestParseVersionAndViewerSidGate(t *testing.T) {
	cases := []struct {
		v    string
		ok   bool
		gate bool
	}{
		{"v0.5.47", true, false},           // release そのもの＝旧（webterm が新形式を知らない）
		{"v0.5.47-1-gabcdef0", true, true}, // release 後の describe 形＝新
		{"v0.5.47-7-g03b48f9-dirty", true, true},
		{"v0.5.48", true, true},
		{"v0.6.0", true, true},
		{"v1.0.0", true, true},
		{"v0.5.46", true, false},
		{"v0.5.41", true, false},
		{"0.5.48", true, true},
		{"dev", false, false},
		{"", false, false},
		{"v0.5", false, false},
	}
	for _, c := range cases {
		_, ok := parseVersion(c.v)
		if ok != c.ok {
			t.Errorf("parseVersion(%q) ok=%v want %v", c.v, ok, c.ok)
		}
		if got := sourceSupportsViewerSid(c.v); got != c.gate {
			t.Errorf("sourceSupportsViewerSid(%q)=%v want %v", c.v, got, c.gate)
		}
	}
}

func TestInjSidForAndInjPaneID(t *testing.T) {
	if got := injSidFor("w1:p4F", "mac-studio-herdr", "v0.5.48"); got != "w1:p4F#inj@mac-studio-herdr" {
		t.Fatalf("新形式: %q", got)
	}
	if got := injSidFor("w1:p4F", "mac-studio-herdr", "v0.5.47"); got != "w1:p4F#inj" {
		t.Fatalf("旧 source には旧形式: %q", got)
	}
	if got := injSidFor("w1:p4F", "", "v0.5.48"); got != "w1:p4F#inj" {
		t.Fatalf("viewer pc 不明なら旧形式: %q", got)
	}
	for _, c := range []struct {
		sid, pane string
		ok        bool
	}{
		{"w1:p4F#inj", "w1:p4F", true},
		{"w1:p4F#inj@mac-studio-herdr", "w1:p4F", true},
		{"w1:p4F", "", false},           // 通常の Web sid はそのまま（呼び手が pane_id として扱う）
		{"w1:p4F#injx", "", false},      // 区切りの直後が末尾でも @ でもない
		{"#inj", "", false},             // pane 無し
		{"w1:p4F#inj@", "w1:p4F", true}, // pc 空でも区切りは成立（pane 解決には使わない）
	} {
		pane, ok := injPaneID(c.sid)
		if ok != c.ok || pane != c.pane {
			t.Errorf("injPaneID(%q)=(%q,%v) want (%q,%v)", c.sid, pane, ok, c.pane, c.ok)
		}
	}
}
