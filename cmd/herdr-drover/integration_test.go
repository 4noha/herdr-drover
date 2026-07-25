package main

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"
)

const realStatusOutput = `pi: not installed (/Users/x/.pi/agent/extensions/herdr-agent-state.ts)
omp: not installed (/Users/x/.omp/agent/extensions/herdr-omp-agent-state.ts)
claude: current (v7) (/Users/x/.claude/hooks/herdr-agent-state.sh)
codex: not installed (/Users/x/.codex/herdr-agent-state.sh)
copilot: current (v2) (/Users/x/.copilot/hooks/herdr-agent-state.sh)
qodercli: not installed (/Users/x/.qoder/hooks/herdr-agent-state.sh)
cursor: current (v1) (/Users/x/.cursor/herdr-agent-state.sh)
`

// status 行の解析は exact prefix（`<agent>: `）で行う。部分一致にすると
// codex / qodercli のような紛らわしい組で誤判定しうる。
func TestParseIntegrationStatus(t *testing.T) {
	for _, tc := range []struct {
		agent            string
		known, installed bool
	}{
		{"claude", true, true},
		{"cursor", true, true},
		{"copilot", true, true},
		{"codex", true, false},
		{"pi", true, false},
		{"qodercli", true, false},
		{"gemini", false, false}, // herdr が integration を持たない種別
		{"", false, false},
	} {
		got := parseIntegrationStatus(realStatusOutput, tc.agent)
		if got.Known != tc.known || got.Installed != tc.installed {
			t.Errorf("%q: Known=%v Installed=%v, want %v/%v (raw=%q)",
				tc.agent, got.Known, got.Installed, tc.known, tc.installed, got.Raw)
		}
	}
	// 未知の出力形式は Known=false＝**何もしない**（推測で解釈しない）。
	if got := parseIntegrationStatus("something totally different\n", "codex"); got.Known {
		t.Errorf("未知形式を解釈した: %+v", got)
	}
}

func TestEnsureAgentIntegration(t *testing.T) {
	restore := herdrIntegrationCmd
	t.Cleanup(func() { herdrIntegrationCmd = restore })

	t.Run("未導入なら install する", func(t *testing.T) {
		var calls []string
		herdrIntegrationCmd = func(args ...string) (string, error) {
			calls = append(calls, strings.Join(args, " "))
			if args[0] == "status" {
				return realStatusOutput, nil
			}
			return "installed codex integration hook to /Users/x/.codex/herdr-agent-state.sh\n", nil
		}
		var out bytes.Buffer
		if !ensureAgentIntegration("codex", &out) {
			t.Fatalf("install されなかった: %s", out.String())
		}
		if len(calls) != 2 || calls[1] != "install codex" {
			t.Fatalf("呼び出し = %v", calls)
		}
		// silent に設定を書き換えない＋遡らないことを明示する（鉄則⑤）。
		for _, want := range []string{"未導入", "install codex", "遡りません"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("出力に %q が無い:\n%s", want, out.String())
			}
		}
	})

	t.Run("導入済みには触らない（手直しを上書きしない）", func(t *testing.T) {
		var calls []string
		herdrIntegrationCmd = func(args ...string) (string, error) {
			calls = append(calls, strings.Join(args, " "))
			return realStatusOutput, nil
		}
		var out bytes.Buffer
		if ensureAgentIntegration("claude", &out) {
			t.Fatal("導入済みなのに install した")
		}
		if len(calls) != 1 {
			t.Fatalf("install を呼んだ: %v", calls)
		}
	})

	t.Run("herdr が持たない種別には何もしない", func(t *testing.T) {
		var calls []string
		herdrIntegrationCmd = func(args ...string) (string, error) {
			calls = append(calls, strings.Join(args, " "))
			return realStatusOutput, nil
		}
		var out bytes.Buffer
		if ensureAgentIntegration("gemini", &out) || len(calls) != 1 {
			t.Fatalf("未対応種別に install した: %v", calls)
		}
	})

	t.Run("status 失敗でも起動を止めない", func(t *testing.T) {
		herdrIntegrationCmd = func(args ...string) (string, error) {
			return "", fmt.Errorf("herdr not found")
		}
		var out bytes.Buffer
		if ensureAgentIntegration("codex", &out) {
			t.Fatal("失敗したのに true")
		}
		if !strings.Contains(out.String(), "見送り") {
			t.Errorf("理由が出ていない: %s", out.String())
		}
	})

	t.Run("install 失敗でも起動を止めない（resume 不可と明示）", func(t *testing.T) {
		herdrIntegrationCmd = func(args ...string) (string, error) {
			if args[0] == "status" {
				return realStatusOutput, nil
			}
			return "permission denied", fmt.Errorf("exit 1")
		}
		var out bytes.Buffer
		if ensureAgentIntegration("codex", &out) {
			t.Fatal("失敗したのに true")
		}
		if !strings.Contains(out.String(), "resume 不可") {
			t.Errorf("影響が明示されていない: %s", out.String())
		}
	})

	t.Run("DROVER_AUTO_INTEGRATION=off で無効化できる", func(t *testing.T) {
		t.Setenv("DROVER_AUTO_INTEGRATION", "off")
		var calls int
		herdrIntegrationCmd = func(args ...string) (string, error) { calls++; return realStatusOutput, nil }
		var out bytes.Buffer
		if ensureAgentIntegration("codex", &out) || calls != 0 {
			t.Fatalf("off なのに実行した（calls=%d）", calls)
		}
	})
}

// プロセス内で agent ごとに 1 回だけ（並行呼び出しでも）。
func TestEnsureAgentIntegrationOnce(t *testing.T) {
	restore := herdrIntegrationCmd
	t.Cleanup(func() { herdrIntegrationCmd = restore })
	integrationOnce.Delete("codex")
	var mu sync.Mutex
	n := 0
	herdrIntegrationCmd = func(args ...string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if args[0] == "status" {
			n++
		}
		return realStatusOutput, nil
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); ensureAgentIntegrationOnce("codex", &bytes.Buffer{}) }()
	}
	wg.Wait()
	if n != 1 {
		t.Fatalf("status を %d 回叩いた（1 回のはず）", n)
	}
}
