//go:build windows

package main

// unix の実 SIGUSR1 往復（nudge_signal_unix_test.go）に対する Windows の対。
// Windows は即時 re-scan の合図が無い（SIGUSR1 不在）ので、nudge は
// **エラーにせず** 周期 tick へ委譲する旨を報告する契約になっている。
// silent no-op にしない（＝黙って何も起きないのを禁じる）ことが要点。

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSendNudgeWindowsReportsTickDelegation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.pid")
	if err := writePidfile(path, os.Getpid()); err != nil {
		t.Fatalf("writePidfile: %v", err)
	}
	var out bytes.Buffer
	if err := sendNudge(path, &out); err != nil {
		t.Fatalf("生存 daemon への nudge がエラーになった: %v", err)
	}
	// 「何も起きなかった」ことがユーザーに伝わる出力であること。
	for _, want := range []string{"非対応", "tick"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("周期 tick 委譲の明示が無い（silent no-op 禁止）: %q", out.String())
		}
	}
}
