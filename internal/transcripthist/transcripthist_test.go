package transcripthist

import (
	"os"
	"os/exec"
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
	got, err := Render("claude", path, "", 1000)
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
	got, err := Render("codex", path, "", 1000)
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
	got, err := Render("claude", p, "", 1000)
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
	got, _ := Render("claude", p, "", 10)
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

// opencode 2.2 の session_message（実測の列・data の形）で実 sqlite を作って描く。
func TestRenderOpencode(t *testing.T) {
	bin, err := exec.LookPath("sqlite3")
	if err != nil {
		t.Fatalf("sqlite3 コマンドが無い（opencode の履歴に必須）: %v", err)
	}
	dir := t.TempDir()
	db := filepath.Join(dir, "opencode.db")
	const sid = "ses_ecd098379ffeiAelqU09C1FkeB"
	sql := `create table session_message (id text primary key, session_id text not null, type text not null, seq integer not null, time_created integer not null, time_updated integer not null, data text not null);
insert into session_message values
('1','` + sid + `','user',1,1,1,'{"text":"状況確認","files":[]}'),
('2','` + sid + `','assistant',2,2,2,'{"content":[{"type":"reasoning","text":"内部の考え"},{"type":"text","text":"確認します。"},{"type":"tool","name":"bash","state":{"status":"completed","input":{"command":"git status"}}}]}'),
('3','` + sid + `','idle',3,3,3,'{"outcome":"failed"}'),
('4','` + sid + `','system',4,4,4,'{"text":"The instructions changed"}'),
('5','ses_other','user',1,5,5,'{"text":"別セッション"}'),
('6','` + sid + `','user',5,6,6,'{"text":"続けて"}');`
	if out, err := exec.Command(bin, db, sql).CombinedOutput(); err != nil {
		t.Fatalf("sqlite3: %v: %s", err, out)
	}
	t.Setenv("XDG_DATA_HOME", dir)
	// Find は <XDG_DATA_HOME>/opencode/opencode.db を探す。
	if err := os.MkdirAll(filepath.Join(dir, "opencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(db, filepath.Join(dir, "opencode", "opencode.db")); err != nil {
		t.Fatal(err)
	}
	path, err := Find("opencode", sid, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	got, err := Render("opencode", path, sid, 1000)
	if err != nil {
		t.Fatal(err)
	}
	want := "\n> 状況確認\n\n● 確認します。\n  ⎿ bash git status\n\n> 続けて\n"
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	// sid は厳格に検証する（SQL へ渡すため）。
	for _, bad := range []string{"ses_x'; drop table session_message;--", "x", "ses_", ""} {
		if _, err := Find("opencode", bad, t.TempDir()); err != ErrNotFound {
			t.Errorf("sid=%q を受け付けてはいけない: %v", bad, err)
		}
		if _, err := Render("opencode", path, bad, 10); err != ErrNotFound {
			t.Errorf("Render sid=%q を受け付けてはいけない: %v", bad, err)
		}
	}
}
