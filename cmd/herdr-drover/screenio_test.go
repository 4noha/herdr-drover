package main

import "testing"

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
