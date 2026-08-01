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
		// copilot / devin の短縮形 -r（実測 `-r, --resume`）。**落とし損ねると
		// BuildResume が付け直した指定と二重になる**。
		{"copilot: alias の = 形", "copilot",
			[]string{"copilot", "-r=abc", "-x"}, []string{"copilot", "-x"}},
		{"copilot: alias の値なし picker 形", "copilot",
			[]string{"copilot", "-r", "--banner"}, []string{"copilot", "--banner"}},
		{"devin: alias はスペース形で値も落とす", "devin",
			[]string{"devin", "-r", "sess-1", "--sandbox"}, []string{"devin", "--sandbox"}},
		{"devin: 値なし picker 形はフラグだけ落とす", "devin",
			[]string{"devin", "--resume", "--model", "opus"},
			[]string{"devin", "--model", "opus"}},
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

// IsDirectInvocation は **21 種すべて**で機能しなければならない。
// 実装当初は claude だけの独自テーブルを見ており、codex / cursor の pane は
// 「対象に入るが必ず skip」という静かな不発になっていた（restart-agent-session
// --agent codex が永久に何もしない）。判定表は herdr の lookup_agent と同一にする。
func TestIsDirectInvocationCoversAllAgents(t *testing.T) {
	for label := range CanonicalLabels {
		aliases := HerdrExecAliases(label)
		if len(aliases) == 0 {
			t.Errorf("%s: herdr の alias 表に無い", label)
			continue
		}
		for _, a := range aliases {
			if !IsDirectInvocation(label, []string{"/usr/local/bin/" + a, "--flag"}) {
				t.Errorf("%s: alias %q を直接起動と判定しない", label, a)
			}
		}
		// wrapper 経由は常に false。
		if IsDirectInvocation(label, []string{"/bin/zsh", "-lc", aliases[0]}) {
			t.Errorf("%s: wrapper 経由を直接起動と判定した", label)
		}
	}
	// cursor は argv[0] が agent 名と異なる（実行名 cursor-agent）。
	if !IsDirectInvocation("cursor", []string{"/usr/local/bin/cursor-agent"}) {
		t.Error("cursor-agent を cursor の直接起動と判定できていない")
	}
	// 未知 kind は常に false。
	if IsDirectInvocation("nosuch", []string{"/usr/bin/nosuch"}) {
		t.Error("未知 kind を直接起動と判定した")
	}
}

// ModelSpec は**実 CLI の --help で確認した agent のみ**（推測で書かない）。
// フラグ名が同じでも**モデル名は互換でない**ので、種別を跨いで同じ値を
// 渡してはいけない（呼び手が --agent を要求する）。
func TestModelSpec(t *testing.T) {
	for _, a := range []string{"claude", "codex", "cursor", "copilot", "devin", "opencode"} {
		sp, ok := Model(a)
		if !ok || sp.Flag != "--model" {
			t.Errorf("%s: ModelSpec = %+v ok=%v", a, sp, ok)
		}
	}
	// opencode は codex と同じく短縮形 -m を持つ（実測 1.18.10 `-m, --model`）。
	if sp, _ := Model("opencode"); len(sp.Aliases) != 1 || sp.Aliases[0] != "-m" {
		t.Errorf("opencode の Aliases = %v（-m のはず）", sp.Aliases)
	}
	// copilot / devin とも短縮形は無い（実測 copilot 1.0.75 / devin 3000.2.17）。
	for _, a := range []string{"copilot", "devin"} {
		if sp, _ := Model(a); len(sp.Aliases) != 0 {
			t.Errorf("%s に短縮形は無いはず: %v", a, sp.Aliases)
		}
	}
	// codex だけ短縮形 -m を持つ（実測 `-m, --model <MODEL>`）。
	if sp, _ := Model("codex"); len(sp.Aliases) != 1 || sp.Aliases[0] != "-m" {
		t.Errorf("codex の Aliases = %v（-m のはず）", sp.Aliases)
	}
	if sp, _ := Model("claude"); len(sp.Aliases) != 0 {
		t.Errorf("claude に短縮形は無い（実測 2.1.220）: %v", sp.Aliases)
	}
	if _, ok := Model("gemini"); ok {
		t.Error("未確認の agent に ModelSpec を書いてはいけない")
	}
}

// モデル指定の張り替えは Spec 駆動。**短縮形を剥がし損ねると二重指定になる**
// （codex の -m で実際に起きうる）。
func TestBuildModel(t *testing.T) {
	for _, tc := range []struct {
		name  string
		agent string
		argv  []string
		model string
		want  []string
	}{
		{"新規付与", "claude", []string{"/p/claude"}, "opus",
			[]string{"/p/claude", "--model", "opus"}},
		{"既存を張り替え", "claude", []string{"/p/claude", "--model", "sonnet"}, "opus",
			[]string{"/p/claude", "--model", "opus"}},
		{"= 形も剥がす", "claude", []string{"/p/claude", "--model=sonnet"}, "opus",
			[]string{"/p/claude", "--model", "opus"}},
		{"codex の短縮形 -m を剥がす（二重指定の防止）", "codex",
			[]string{"/p/codex", "-m", "gpt-5", "--full-auto"}, "gpt-5-codex",
			[]string{"/p/codex", "--full-auto", "--model", "gpt-5-codex"}},
		{"model 空なら触らない", "claude", []string{"/p/claude", "--model", "sonnet"}, "",
			[]string{"/p/claude", "--model", "sonnet"}},
		{"Spec 無しは触らない（勝手なフラグを足さない）", "gemini",
			[]string{"/p/gemini", "--foo"}, "x", []string{"/p/gemini", "--foo"}},
		{"無関係な引数は順序ごと保つ", "claude",
			[]string{"/p/claude", "--verbose", "--resume", "u"}, "opus",
			[]string{"/p/claude", "--verbose", "--resume", "u", "--model", "opus"}},
	} {
		got := BuildModel(tc.agent, tc.argv, tc.model)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: BuildModel(%s, %v, %q) = %v, want %v",
				tc.name, tc.agent, tc.argv, tc.model, got, tc.want)
		}
	}
}

// 更新口は実 CLI で確認した 3 種（いずれも `<bin> update` と `--version`）。
func TestUpdaterSpecCoversVerifiedAgents(t *testing.T) {
	// 自己更新まで**非対話で完走することを実測した**もの。
	for _, a := range []string{"claude", "codex", "cursor", "copilot"} {
		sp, ok := Updater(a)
		if !ok || len(sp.UpdateArgv) != 1 || sp.UpdateArgv[0] != "update" {
			t.Errorf("%s: UpdaterSpec = %+v ok=%v", a, sp, ok)
		}
		if len(sp.VersionArgv) != 1 || sp.VersionArgv[0] != "--version" {
			t.Errorf("%s: VersionArgv = %v", a, sp.VersionArgv)
		}
	}
	// opencode は `upgrade`（`update` ではない）。⚠サブコマンド名は agent ごとに違う
	// ＝表の値をそのまま使うこと（"update" 決め打ちにしない）。導入経路を自分で
	// 検知して brew 経由で更新するので、brew 管理と食い違わない（devin との違い）。
	if sp, ok := Updater("opencode"); !ok || len(sp.UpdateArgv) != 1 || sp.UpdateArgv[0] != "upgrade" {
		t.Errorf("opencode: UpdateArgv = %v（upgrade のはず）", sp.UpdateArgv)
	}
	// devin は **版は取れるが自己更新は載せない**（`devin update` は存在するが
	// 非対話で完走しない＝実測 3000.2.17 で stdin を閉じると rc=130・出力 9B。
	// さらに brew cask 管理なので自己更新は brew と食い違う）。UpdateArgv=nil は
	// 型の first-class な状態（errNoUpdater＝再起動のみ）＝**書き忘れではない**。
	sp, ok := Updater("devin")
	if !ok {
		t.Fatal("devin: UpdaterSpec が無い（版取得だけは載せる）")
	}
	if len(sp.VersionArgv) != 1 || sp.VersionArgv[0] != "--version" {
		t.Errorf("devin: VersionArgv = %v", sp.VersionArgv)
	}
	if sp.UpdateArgv != nil {
		t.Errorf("devin: UpdateArgv は nil のはず（非対話で完走しない）: %v", sp.UpdateArgv)
	}
	if _, ok := Updater("gemini"); ok {
		t.Error("未確認の agent に UpdaterSpec を書いてはいけない")
	}
}

// UpdaterAgents は表から導出する（呼び手のメッセージに種別名を焼かないため。
// 以前「更新口を持つのは claude のみ」と出力に焼いてあり、codex/cursor 追加後も
// 直っておらず利用者に嘘を表示していた）。
func TestUpdaterAgents(t *testing.T) {
	got := UpdaterAgents()
	want := []string{"claude", "codex", "copilot", "cursor", "devin", "opencode"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("UpdaterAgents() = %v, want %v（昇順・表と一致）", got, want)
	}
	// 表に足したら自動で増えること＝ハードコードでないことの担保。
	if len(got) != len(updaterSpecs) {
		t.Errorf("表の件数 %d と戻り値 %d が食い違う", len(updaterSpecs), len(got))
	}
}

// InstallSpec は**実機で実行ファイルの所在を確認できた agent のみ**載せる。
// BinNames が herdr の alias 表の要素であることは ValidateSpecs が別途検証する
// （表に無い名前で起動すると herdr の検出に一切載らない）。
func TestInstallSpecCoversVerifiedAgents(t *testing.T) {
	for _, tc := range []struct {
		agent string
		bin   string
	}{
		{"claude", "claude"},
		{"codex", "codex"},
		{"cursor", "cursor-agent"},
		{"copilot", "copilot"},   // npm -g @github/copilot（実測 /opt/homebrew/bin/copilot）
		{"devin", "devin"},       // brew --cask devin-cli（実測 /opt/homebrew/bin/devin）
		{"opencode", "opencode"}, // brew anomalyco/tap/opencode（実測 /opt/homebrew/bin/opencode）
	} {
		sp, ok := Install(tc.agent)
		if !ok {
			t.Errorf("%s: InstallSpec が無い", tc.agent)
			continue
		}
		if len(sp.BinNames) == 0 || sp.BinNames[0] != tc.bin {
			t.Errorf("%s: BinNames = %v（先頭は %q のはず）", tc.agent, sp.BinNames, tc.bin)
		}
	}
	if _, ok := Install("gemini"); ok {
		t.Error("未確認の agent に InstallSpec を書いてはいけない（実バイナリ名を推測しない）")
	}
}
