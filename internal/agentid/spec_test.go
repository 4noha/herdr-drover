package agentid

import (
	"reflect"
	"testing"
)

// Spec 群の内部整合。BinNames が herdr の lookup_agent 表から外れると、
// その名前で起動しても herdr の検出に一切載らず（前景プロセス名基準）、
// resume も organize の検出系統も **silent に**無効化される。
func TestValidateSpecs(t *testing.T) {
	if err := ValidateSpecs(); err != nil {
		t.Fatal(err)
	}
}

// resume 対応は herdr agent_resume.rs plan() の 14 種ちょうど。
// 残り 7 種は「未実装」ではなく原理的に不可能（herdr が agent_session を出さない）。
func TestResumeSupportMatchesHerdr(t *testing.T) {
	want := map[string]bool{
		"claude": true, "codex": true, "copilot": true, "devin": true,
		"droid": true, "kimi": true, "mastracode": true, "pi": true,
		"omp": true, "hermes": true, "opencode": true, "qodercli": true,
		"kilo": true, "cursor": true,
	}
	n := 0
	for label := range CanonicalLabels {
		got := Resume(label).Supported
		if got != want[label] {
			t.Errorf("%s: Supported=%v, want %v", label, got, want[label])
		}
		if got {
			n++
		}
	}
	if n != 14 {
		t.Errorf("resume 対応 = %d 種, want 14（herdr agent_resume.rs plan）", n)
	}
	// 非対応 7 種を名指しで固定。
	for _, a := range []string{"agy", "amp", "cline", "gemini", "grok", "kiro", "maki"} {
		if Resume(a).Supported {
			t.Errorf("%s は herdr が agent_session を出さない＝resume 不能のはず", a)
		}
	}
}

// argv 組み立てが herdr の plan() と同じ形になること（全 14 種）。
func TestBuildResumeMatchesHerdrPlan(t *testing.T) {
	for _, tc := range []struct {
		agent string
		want  []string
	}{
		{"claude", []string{"claude", "--resume", "V"}},
		{"devin", []string{"devin", "--resume", "V"}},
		{"droid", []string{"droid", "--resume", "V"}},
		{"hermes", []string{"hermes", "--resume", "V"}},
		{"qodercli", []string{"qodercli", "--resume", "V"}},
		{"cursor", []string{"cursor-agent", "--resume", "V"}},
		{"kimi", []string{"kimi", "--session", "V"}},
		{"opencode", []string{"opencode", "--session", "V"}},
		{"kilo", []string{"kilo", "--session", "V"}},
		{"pi", []string{"pi", "--session", "V"}},
		{"mastracode", []string{"mastracode", "--thread", "V"}},
		{"copilot", []string{"copilot", "--resume=V"}},
		{"omp", []string{"omp", "--resume=V"}},
		{"codex", []string{"codex", "resume", "V"}},
	} {
		// argv[0] は Spec の Argv0（あれば）／なければ agent 名。
		argv0 := tc.agent
		if a := Resume(tc.agent).Argv0; a != "" {
			argv0 = a
		}
		got := BuildResume(tc.agent, []string{argv0}, "V")
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: %v, want %v", tc.agent, got, tc.want)
		}
	}
}

// resume 非対応は argv を一切いじらない（素起動へ落ちる）。
func TestBuildResumeLeavesUnsupportedUntouched(t *testing.T) {
	in := []string{"gemini", "--foo"}
	got := BuildResume("gemini", in, "V")
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("resume 非対応で argv を変えた: %v", got)
	}
}

// **値の書式で判断しない**のが旧実装との本質的な違い。
// pi / omp は path 形の ref を取るので、uuid 判定だと二重指定になる。
func TestStripResumeUsesSpecNotValueFormat(t *testing.T) {
	for _, tc := range []struct {
		name  string
		agent string
		in    []string
		want  []string
	}{
		{"path 形の値も落とす（旧 isUUID 判定では残っていた）", "pi",
			[]string{"pi", "--session", "/home/u/.pi/sessions/x.json", "--verbose"},
			[]string{"pi", "--verbose"}},
		{"= 形", "copilot",
			[]string{"copilot", "--resume=abc", "-x"}, []string{"copilot", "-x"}},
		{"alias も落とす", "claude",
			[]string{"claude", "-r", "8b1e0e2c-0000-4000-8000-000000000000"}, []string{"claude"}},
		{"値なし picker 形はフラグだけ落とす", "claude",
			[]string{"claude", "--resume", "--model", "opus"},
			[]string{"claude", "--model", "opus"}},
		{"サブコマンド形", "codex",
			[]string{"codex", "resume", "sess-1", "--full-auto"},
			[]string{"codex", "--full-auto"}},
		{"サブコマンド形・値なし", "codex",
			[]string{"codex", "resume", "--full-auto"}, []string{"codex", "--full-auto"}},
		{"サブコマンドでない位置引数は触らない", "codex",
			[]string{"codex", "exec", "hello"}, []string{"codex", "exec", "hello"}},
		{"無関係な引数は残す", "claude",
			[]string{"claude", "--model", "opus"}, []string{"claude", "--model", "opus"}},
	} {
		if got := StripResume(tc.agent, tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: StripResume(%s, %v) = %v, want %v", tc.name, tc.agent, tc.in, got, tc.want)
		}
	}
}

// 再構築は冪等（二重に付かない）。
func TestBuildResumeIdempotent(t *testing.T) {
	for _, agent := range []string{"claude", "codex", "copilot", "pi"} {
		argv0 := agent
		if a := Resume(agent).Argv0; a != "" {
			argv0 = a
		}
		once := BuildResume(agent, []string{argv0}, "V")
		twice := BuildResume(agent, once, "V")
		if !reflect.DeepEqual(once, twice) {
			t.Errorf("%s: 冪等でない\n 1 回目 %v\n 2 回目 %v", agent, once, twice)
		}
	}
}

// session ref の妥当性は herdr 相当（uuid 形を要求しない）。
func TestValidSessionRef(t *testing.T) {
	ok := []string{"8b1e0e2c-0000-4000-8000-000000000000", "/home/u/.pi/s.json", "abc123", "a"}
	ng := []string{"", "with\nnewline", "tab\there", string(make([]byte, 513))}
	for _, v := range ok {
		if !ValidSessionRef(v) {
			t.Errorf("ValidSessionRef(%q) = false, want true", v)
		}
	}
	for _, v := range ng {
		if ValidSessionRef(v) {
			t.Errorf("ValidSessionRef(%q) = true, want false", v)
		}
	}
}

func TestSupportsKind(t *testing.T) {
	if !SupportsKind("pi", "path") || !SupportsKind("omp", "path") {
		t.Error("pi / omp は path kind も扱えるはず")
	}
	if SupportsKind("claude", "path") {
		t.Error("claude は id のみのはず")
	}
	if SupportsKind("gemini", "id") {
		t.Error("resume 非対応は常に false")
	}
}
