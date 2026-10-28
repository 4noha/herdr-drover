package bridge

import (
	"bytes"
	"strings"
	"testing"
)

func TestMetaFraming(t *testing.T) {
	got := string(MetaBegin(179, 70, true))
	if got != "\x1b]7707;v=2;cols=179;rows=70;trunc=1\x1b\\" {
		t.Fatalf("MetaBegin: %q", got)
	}
	if got := string(MetaHistEnd()); got != "\x1b]7707;hist_end\x1b\\" {
		t.Fatalf("MetaHistEnd: %q", got)
	}
	// 2026 同期シムのマーカーと衝突しない
	if bytes.Contains(MetaBegin(1, 1, false), []byte("\x1b[?2026")) {
		t.Fatal("META が DECSET 2026 を含む")
	}
}

func TestHistoryPayload(t *testing.T) {
	rows := 3
	// 可視画面ぶん以下 → 何も書かない
	if p := HistoryPayload("a\nb\nc\n", rows); p != nil {
		t.Fatalf("可視画面ぶんだけなのに payload: %q", p)
	}
	if p := HistoryPayload("", rows); p != nil {
		t.Fatalf("空入力で payload: %q", p)
	}
	// 5 行（末尾 3 行＝可視画面）→ 2 行の履歴＋rows 行のパディング
	p := string(HistoryPayload("h1  \nh2\nv1\nv2\nv3\n", rows))
	want := "h1\r\nh2\r\n" + strings.Repeat("\r\n", rows)
	if p != want {
		t.Fatalf("payload=%q want=%q", p, want)
	}
	if strings.Contains(p, "\n\n") || strings.Count(p, "\n") != strings.Count(p, "\r\n") {
		t.Fatalf("LF が CRLF になっていない: %q", p)
	}
	// 末尾改行の無い本文でも同じ
	if q := string(HistoryPayload("h1\nh2\nv1\nv2\nv3", rows)); q != want {
		t.Fatalf("末尾改行なし: %q", q)
	}
}
