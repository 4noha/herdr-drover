package transcripthist

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// 行の形は 2026-10 の実 transcript から（Claude Code 2.1 / Codex 0.165）。
const claudeSample = `{"type":"permission-mode","permissionMode":"auto"}
{"type":"user","isSidechain":false,"message":{"role":"user","content":"新セッションを作って"}}
{"type":"user","isMeta":true,"message":{"role":"user","content":"<system-reminder>注入</system-reminder>"}}
{"type":"assistant","isSidechain":false,"message":{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"x"},{"type":"text","text":"作ります。\n2 行目"},{"type":"tool_use","name":"Bash","input":{"command":"ls  -la\n/tmp","description":"list"}}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"出力"}]}}
{"type":"assistant","isSidechain":true,"message":{"role":"assistant","content":[{"type":"text","text":"サブエージェントの独白"}]}}
{"type":"user","message":{"role":"user","content":[{"type":"text","text":"続けて"}]}}
`

const codexSample = `{"type":"session_meta","payload":{"id":"abc"}}
{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"# AGENTS.md instructions for /x\n\n<INSTRUCTIONS>…"}]}}
{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"テストを直して"}]}}
{"type":"response_item","payload":{"type":"reasoning","summary":[],"encrypted_content":"zzz"}}
{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"確認します。"}]}}
{"type":"response_item","payload":{"type":"function_call","name":"sleep","arguments":"{\"duration_ms\":20000}"}}
{"type":"response_item","payload":{"type":"custom_tool_call","name":"exec","input":"const r = await tools.exec_command({cmd: 'ls'})"}}
{"type":"response_item","payload":{"type":"function_call_output","output":"Wall time: 20s"}}
`

func TestRenderClaude(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, ".claude", "projects", "-p", "sid1.jsonl"), claudeSample)
	path, err := Find("claude", "sid1", home)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Render("claude", path, 1000)
	if err != nil {
		t.Fatal(err)
	}
	want := "\n> 新セッションを作って\n\n● 作ります。\n  2 行目\n  ⎿ Bash ls -la /tmp\n\n> 続けて\n"
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	for _, bad := range []string{"system-reminder", "出力", "サブエージェント", "thinking"} {
		if strings.Contains(got, bad) {
			t.Errorf("含めてはいけない %q が入っている: %q", bad, got)
		}
	}
}

func TestRenderCodex(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, ".codex", "sessions", "2026", "10", "30", "rollout-2026-10-30T00-00-00-sid2.jsonl"), codexSample)
	path, err := Find("codex", "sid2", home)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Render("codex", path, 1000)
	if err != nil {
		t.Fatal(err)
	}
	want := "\n> テストを直して\n\n● 確認します。\n  ⎿ sleep\n  ⎿ exec const r = await tools.exec_command({cmd: 'ls'})\n"
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestRenderTailOfHugeFile(t *testing.T) {
	home := t.TempDir()
	p := filepath.Join(home, ".claude", "projects", "-p", "big.jsonl")
	pad := `{"type":"attachment","x":"` + strings.Repeat("a", 1<<20) + `"}` + "\n"
	body := `{"type":"user","message":{"role":"user","content":"古い指示"}}` + "\n" +
		strings.Repeat(pad, 26) + // 26MB > tailBytes
		`{"type":"user","message":{"role":"user","content":"新しい指示"}}` + "\n"
	write(t, p, body)
	got, err := Render("claude", p, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "古い指示") || !strings.Contains(got, "新しい指示") {
		t.Fatalf("末尾だけ読むはず: %q", got)
	}
}

func TestRenderMaxLines(t *testing.T) {
	home := t.TempDir()
	p := filepath.Join(home, ".claude", "projects", "-p", "s.jsonl")
	var b strings.Builder
	for i := 0; i < 50; i++ {
		b.WriteString(`{"type":"user","message":{"role":"user","content":"指示` + strings.Repeat("x", i%3) + `"}}` + "\n")
	}
	write(t, p, b.String())
	got, _ := Render("claude", p, 10)
	if n := strings.Count(got, "\n"); n != 10 {
		t.Fatalf("末尾 10 行のはず: %d 行 %q", n, got)
	}
}

func TestFindRejectsBadSid(t *testing.T) {
	for _, sid := range []string{"", "../x", "a/b", "*", "a?b"} {
		if _, err := Find("claude", sid, t.TempDir()); err != ErrNotFound {
			t.Errorf("sid=%q: %v", sid, err)
		}
	}
	if _, err := Find("opencode", "x", t.TempDir()); err != ErrNotFound {
		t.Error("未対応 agent は ErrNotFound")
	}
}
