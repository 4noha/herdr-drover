package main

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTailLines(t *testing.T) {
	cases := []struct {
		name      string
		text      string
		n         int
		want      string
		truncated bool
	}{
		{"短い・切らない", "a\nb\nc", 5, "a\nb\nc", false},
		{"ちょうど", "a\nb\nc", 3, "a\nb\nc", false},
		{"末尾n行だけ", "a\nb\nc\nd", 2, "c\nd", true},
		{"1行", "only", 1, "only", false},
		{"空文字", "", 1, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, trunc := tailLines(c.text, c.n)
			if got != c.want || trunc != c.truncated {
				t.Fatalf("tailLines(%q,%d) = (%q,%v), want (%q,%v)", c.text, c.n, got, trunc, c.want, c.truncated)
			}
		})
	}
}

func TestTailBytesUTF8Safe(t *testing.T) {
	if got, cut := tailBytesUTF8Safe("hello", 10); got != "hello" || cut {
		t.Fatalf("短い入力で切られた: %q %v", got, cut)
	}
	if got, cut := tailBytesUTF8Safe("hello", 5); got != "hello" || cut {
		t.Fatalf("ちょうどの長さで切られた: %q %v", got, cut)
	}

	// マルチバイト文字（日本語）の3バイト境界をちょうど跨ぐケース。愚直な
	// byte slice だと「本」の途中で切れて無効な UTF-8 になる（修正前の実装が
	// 実際にこれで壊れることを確認済み）。
	text := strings.Repeat("a", 10) + "本" + strings.Repeat("b", 10) // 10+3+10=23 bytes
	got, cut := tailBytesUTF8Safe(text, 12)
	if !cut {
		t.Fatal("切られるはずなのに cut=false")
	}
	if !utf8.ValidString(got) {
		t.Fatalf("切った結果が無効な UTF-8: %q (% x)", got, []byte(got))
	}
	want := strings.Repeat("b", 10)
	if got != want {
		t.Fatalf("境界調整の結果が想定外: got=%q want=%q", got, want)
	}
}
