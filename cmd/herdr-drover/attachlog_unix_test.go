//go:build unix

package main

// attachlog の集約（Cycle）テスト。**量の見積もりを外してローテートで比較材料を
// 失った**реgression の担保（2026-08-01）。実ファイルへ書いて中身を読む。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestAttachLogger は一時ファイルへ書くロガーを返す。⚠setTestHome で HOME を
// 隔離してから作る（実 ~/.herdr-drover を汚さない・2026-07-25 の実害の教訓）。
func newTestAttachLogger(t *testing.T) (*attachLogger, string) {
	t.Helper()
	home := t.TempDir()
	setTestHome(t, home)
	lg, err := newAttachLogger("testpc", "w1:p1")
	if err != nil {
		t.Fatalf("newAttachLogger: %v", err)
	}
	t.Cleanup(lg.Close)
	return lg, filepath.Join(home, ".herdr-drover", "attach.log")
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var out []string
	for _, ln := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(ln) != "" {
			out = append(out, ln)
		}
	}
	return out
}

// TestCycleCollapsesRepeats は **同じ結果が続く間は行を増やさない**ことを確認する。
// これが効かないと 1 日 10MB に達し、ローテートで機ごとの比較材料が消える
// （実測でそうなった）。
func TestCycleCollapsesRepeats(t *testing.T) {
	lg, path := newTestAttachLogger(t)
	for i := 0; i < 100; i++ {
		lg.Cycle("quiescence", "接続=30s received=200000B idleClosed=true")
	}
	n := len(readLines(t, path))
	if n > 2 {
		t.Fatalf("同じ結果 100 回で %d 行出た（畳めていない）", n)
	}
}

// TestCycleEmitsOnKindChange は **種類が変わった瞬間は必ず素の行を出す**ことを
// 確認する。異常（frame-error 等）が正常系の repeat に埋もれてはいけない。
func TestCycleEmitsOnKindChange(t *testing.T) {
	lg, path := newTestAttachLogger(t)
	for i := 0; i < 50; i++ {
		lg.Cycle("quiescence", "正常")
	}
	lg.Cycle("frame-error", "接続=1s received=1000000B ／ conn 読取終了: failed to read frame header")

	lines := readLines(t, path)
	var sawSummary, sawAnomaly bool
	for _, ln := range lines {
		if strings.Contains(ln, "同じ結果が続いた ×") {
			sawSummary = true
		}
		if strings.Contains(ln, "failed to read frame header") {
			sawAnomaly = true
		}
	}
	if !sawSummary {
		t.Error("畳んだぶんの要約行が出ていない（何回続いたか分からなくなる）")
	}
	if !sawAnomaly {
		t.Error("種類が変わった行（異常）が出ていない＝異常が正常系に埋もれる")
	}
}

// TestCycleFlushesOnClose は Close 時に畳んでいる分を吐くことを確認する。
// 黙って捨てると「最後に何が起きていたか」が消える。
func TestCycleFlushesOnClose(t *testing.T) {
	lg, path := newTestAttachLogger(t)
	for i := 0; i < 10; i++ {
		lg.Cycle("peer-eof", "接続=1s received=200000B")
	}
	lg.Close()
	found := false
	for _, ln := range readLines(t, path) {
		if strings.Contains(ln, "同じ結果が続いた ×9") {
			found = true
		}
	}
	if !found {
		t.Error("Close 時に畳んだ分が吐かれていない")
	}
}

// TestCycleEmitsPeriodicallyEvenWhenSame は「同じ結果でも一定間隔では 1 行出す」
// ことを確認する。無いと数時間沈黙し、**動いているのか止まったのか**が読めない。
func TestCycleEmitsPeriodicallyEvenWhenSame(t *testing.T) {
	lg, path := newTestAttachLogger(t)
	lg.Cycle("quiescence", "正常 1")
	before := len(readLines(t, path))
	// 経過時間を偽装（runSince を過去へ）。
	lg.mu.Lock()
	lg.runSince = time.Now().Add(-attachLogSummaryEvery - time.Second)
	lg.mu.Unlock()
	lg.Cycle("quiescence", "正常 2")
	if after := len(readLines(t, path)); after <= before {
		t.Fatalf("%s 経過しても行が増えない（生存確認ができない）", attachLogSummaryEvery)
	}
}

// TestCycleKind は終了理由の畳み方（可変部分をキーにしない）を固定する。
func TestCycleKind(t *testing.T) {
	for _, c := range []struct {
		idle bool
		why  string
		want string
	}{
		{true, "conn 読取終了: failed to get reader: context canceled", "quiescence"},
		{false, "conn 読取終了: EOF", "peer-eof"},
		{false, "conn 読取終了: failed to get reader: failed to read frame header", "frame-error"},
		{false, "conn 読取終了: failed to read: use of closed network connection", "self-close"},
		{false, "pane への書込失敗（12345 バイト目）: broken pipe", "pane-write-fail"},
		{false, "conn 読取終了: なんらかの新しいエラー", "other"},
	} {
		if got := cycleKind(c.idle, c.why); got != c.want {
			t.Errorf("cycleKind(%v,%q) = %q, want %q", c.idle, c.why, got, c.want)
		}
	}
	// ⚠ 同じ種類ならバイト数が違ってもキーが一致すること（違うと畳めない）。
	a := cycleKind(false, "conn 読取終了: EOF")
	b := cycleKind(false, "conn 読取終了: EOF")
	if a != b {
		t.Fatal("同種なのにキーが割れている＝畳めず量が減らない")
	}
}
