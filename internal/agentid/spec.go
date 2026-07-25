package agentid

// spec — エージェントごとの「会話の再開」「本体の更新」「本体バイナリの解決」の
// 差分を**データとして**持つ（DESIGN_MULTI_AGENT.md §3.2〜3.4 の P4）。
//
// 骨格（argv を組み立て直して pane を作り直す・版比較で更新有無を決める・
// 稼働 argv[0] を最優先でバイナリ解決する）は agent 非依存なので、差分だけを
// ここに集約する。**分岐をコードに散らさない**のが要点。
//
// # ResumeSpec の出所
//
// herdr の `src/agent_resume.rs plan()` の**実ソースから写経**した（2026-07-25 に
// 全 14 分岐を照合済み）。この表は **herdr の API では公開されていない**ので、
// drover にミラーを持つしかない。
//
// ⚠herdr 側が増えたらここも増やす必要がある。取りこぼしても「resume 非対応」へ
// loud に落ちるだけで破壊はしない（Supported=false と同じ扱い）。
//
// # resume 非対応 7 種は「未実装」ではない
//
// agy / amp / cline / gemini / grok / kiro / maki は herdr が agent_session を
// **出さない**＝原理的に再開できない。restart 時は resume 無しの素起動へ落とし、
// その旨を loud に出力して Ack detail に残す（黙って会話を失わない）。

import (
	"fmt"
	"strings"
	"time"
)

// Form は resume 引数の形。
type Form int

const (
	// FormSpace は `--resume <v>`（フラグと値が別トークン）。
	FormSpace Form = iota
	// FormEquals は `--resume=<v>`（1 トークン）。
	FormEquals
	// FormSubcommand は `codex resume <v>`（**位置引数のサブコマンド**）。
	// フラグではないので「フラグ 1 個を落とす」構造では表現できない＝
	// argv[1] がサブコマンドなら 2 語落とす、という別の処理が要る。
	FormSubcommand
)

// ResumeSpec は「会話の再開」の差分。
type ResumeSpec struct {
	// Supported=false は resume 非対応（herdr が agent_session を出さない）。
	// このとき他のフィールドは意味を持たない。
	Supported bool
	// Flag は "--resume" / "--session" / "--thread"。FormSubcommand では未使用。
	Flag string
	// Aliases は strip 対象の別表記（claude の "-r" など）。**再構築には使わない**
	// ＝落とすときだけ広く、付けるときは Flag に統一する。
	Aliases []string
	// Form は引数の形。
	Form Form
	// Subcommand は FormSubcommand のときの語（codex の "resume"）。
	Subcommand string
	// Argv0 は実行ファイル名が agent 名と異なる場合のみ非空（cursor→cursor-agent）。
	Argv0 string
	// Kinds は herdr が出す session ref の kind（"id" / "path"）。
	Kinds []string
}

// resumeSpecs は herdr agent_resume.rs plan() の写し（14 種）。
// ここに無い canonical label は resume 非対応（＝残り 7 種）。
var resumeSpecs = map[string]ResumeSpec{
	// `<bin> --resume <v>`
	"claude":   {Supported: true, Flag: "--resume", Aliases: []string{"-r"}, Form: FormSpace, Kinds: []string{"id"}},
	"devin":    {Supported: true, Flag: "--resume", Form: FormSpace, Kinds: []string{"id"}},
	"droid":    {Supported: true, Flag: "--resume", Form: FormSpace, Kinds: []string{"id"}},
	"hermes":   {Supported: true, Flag: "--resume", Form: FormSpace, Kinds: []string{"id"}},
	"qodercli": {Supported: true, Flag: "--resume", Form: FormSpace, Kinds: []string{"id"}},
	// argv[0] が agent 名と違う唯一の例。
	"cursor": {Supported: true, Flag: "--resume", Form: FormSpace, Argv0: "cursor-agent", Kinds: []string{"id"}},
	// `<bin> --session <v>`
	"kimi":     {Supported: true, Flag: "--session", Form: FormSpace, Kinds: []string{"id"}},
	"opencode": {Supported: true, Flag: "--session", Form: FormSpace, Kinds: []string{"id"}},
	"kilo":     {Supported: true, Flag: "--session", Form: FormSpace, Kinds: []string{"id"}},
	"pi":       {Supported: true, Flag: "--session", Form: FormSpace, Kinds: []string{"id", "path"}},
	// `<bin> --thread <v>`
	"mastracode": {Supported: true, Flag: "--thread", Form: FormSpace, Kinds: []string{"id"}},
	// `<bin> --resume=<v>`
	"copilot": {Supported: true, Flag: "--resume", Form: FormEquals, Kinds: []string{"id"}},
	// omp は `-r, --resume=<value>`（ID prefix または path）。pi と違い --session は無い。
	"omp": {Supported: true, Flag: "--resume", Aliases: []string{"-r"}, Form: FormEquals, Kinds: []string{"id", "path"}},
	// `codex resume <v>` — 位置引数サブコマンド。
	"codex": {Supported: true, Subcommand: "resume", Form: FormSubcommand, Kinds: []string{"id"}},
}

// Resume は agent の ResumeSpec を返す。未知／非対応は Supported=false。
func Resume(agent string) ResumeSpec { return resumeSpecs[agent] }

// UpdaterSpec は「本体の更新」の差分。
type UpdaterSpec struct {
	// VersionArgv は版取得の引数。nil = 版を問い合わせる口が無い
	// （＝版比較 skip・更新有無不明として loud にログし、再起動段へ進む）。
	VersionArgv []string
	// UpdateArgv は自己更新の引数。nil = 更新口なし（再起動のみ）。
	UpdateArgv []string
	// Timeout は**その agent 単体**の更新予算（全体に 1 本掛けない）。
	Timeout time.Duration
}

// updaterSpecs — 実測で確認できた agent のみ載せる。**推測で書かない**
// （間違った更新コマンドを走らせるのは黙って壊す行為）。
var updaterSpecs = map[string]UpdaterSpec{
	// claude: `claude update`。~250MB の DL があり 5 分では足りない（実測で
	// timeout し `signal: killed` になった）＝15 分。
	"claude": {VersionArgv: []string{"--version"}, UpdateArgv: []string{"update"}, Timeout: 15 * time.Minute},
}

// Updater は agent の UpdaterSpec と、載っているかを返す。
func Updater(agent string) (UpdaterSpec, bool) { s, ok := updaterSpecs[agent]; return s, ok }

// InstallSpec は「本体バイナリの解決」の差分。
type InstallSpec struct {
	// BinNames は実行ファイル名の候補。**herdr の lookup_agent 表の要素に限る**
	// （下記 ⚠を参照）。先頭が既定。
	BinNames []string
	// WellKnownPaths は既定インストール先（$HOME 相対。"~/" 始まり）。
	WellKnownPaths []string
}

// installSpecs — BinNames は herdr `src/detect/mod.rs lookup_agent` の alias 表と
// 突き合わせ済み（ValidateSpecs が起動時に静的検証する）。
//
// ⚠**alias 表に無い basename で起動すると herdr の検出に一切載らない**
// （前景プロセス名基準）。`pane.agent` も `agent_session` も付かず、resume も
// organize の検出系統も silent に無効化される。だから BinNames は必ず表の要素にする。
var installSpecs = map[string]InstallSpec{
	"claude": {BinNames: []string{"claude"}, WellKnownPaths: []string{"~/.local/bin/claude"}},
	"codex":  {BinNames: []string{"codex"}},
	// cursor の実行ファイル名は cursor-agent（canonical label と異なる唯一の例。
	// ResumeSpec.Argv0="cursor-agent" と同源）。公式インストーラは
	// ~/.local/bin/cursor-agent と ~/.local/bin/agent の 2 本の symlink を張るが、
	// `agent` は汎用名で衝突を招く（Grok の /.grok/bin/agent と実際に衝突した）ため
	// 解決先の候補は cursor-agent のみに絞る。
	"cursor": {BinNames: []string{"cursor-agent"}, WellKnownPaths: []string{"~/.local/bin/cursor-agent"}},
}

// Install は agent の InstallSpec と、載っているかを返す。
func Install(agent string) (InstallSpec, bool) { s, ok := installSpecs[agent]; return s, ok }

// herdrExecAliases は herdr `lookup_agent` の alias 表（実ソース抽出）。
// 「この basename で起動すれば herdr がその agent として検出する」の権威。
var herdrExecAliases = map[string][]string{
	"pi": {"pi"}, "claude": {"claude", "claude-code"}, "codex": {"codex"},
	"gemini": {"gemini"}, "cursor": {"cursor", "cursor-agent"},
	"devin": {"devin", "devin-cli", "devin cli"},
	"agy":   {"agy", "antigravity", "antigravity-cli"},
	"cline": {"cline"}, "omp": {"omp"},
	"mastracode": {"mastracode", "mastra-code", "mastra code"},
	"opencode":   {"opencode", "open-code"},
	"copilot":    {"copilot", "github-copilot", "ghcs"},
	"kimi":       {"kimi", "kimi-code", "kimi code"},
	"kiro":       {"kiro", "kiro-cli"}, "droid": {"droid"},
	"amp": {"amp", "amp-local"}, "grok": {"grok", "grok-build"},
	"hermes":   {"hermes", "hermes-agent"},
	"kilo":     {"kilo", "kilo-code", "kilo code"},
	"qodercli": {"qodercli", "qoderclicn", "qoder", "qodercn"},
	"maki":     {"maki"},
}

// HerdrExecAliases は agent を herdr に検出させられる実行ファイル名を返す。
func HerdrExecAliases(agent string) []string { return herdrExecAliases[agent] }

// ValidateSpecs は Spec 群の内部整合を検査する（起動時／テストで呼ぶ）。
// **黙って壊れる種類の誤りを起動時に落とす**のが目的:
//   - canonical でない agent の Spec（誰にも一致しないので永久に無効）
//   - lookup_agent の alias 表に無い BinNames（herdr の検出に載らず、resume も
//     organize の検出系統も silent に無効化される）
func ValidateSpecs() error {
	var errs []string
	for a := range resumeSpecs {
		if !IsCanonical(a) {
			errs = append(errs, fmt.Sprintf("resumeSpecs[%q]: canonical label でない", a))
		}
	}
	for a, s := range installSpecs {
		if !IsCanonical(a) {
			errs = append(errs, fmt.Sprintf("installSpecs[%q]: canonical label でない", a))
			continue
		}
		aliases := herdrExecAliases[a]
		for _, bin := range s.BinNames {
			found := false
			for _, al := range aliases {
				if al == bin {
					found = true
					break
				}
			}
			if !found {
				errs = append(errs, fmt.Sprintf(
					"installSpecs[%q].BinNames=%q が herdr の lookup_agent 表に無い"+
						"（この名前で起動すると herdr の検出に一切載らない）", a, bin))
			}
		}
	}
	for a := range updaterSpecs {
		if !IsCanonical(a) {
			errs = append(errs, fmt.Sprintf("updaterSpecs[%q]: canonical label でない", a))
		}
	}
	// ResumeSpec.Argv0 も alias 表の要素でなければならない（cursor-agent）。
	for a, s := range resumeSpecs {
		if s.Argv0 == "" {
			continue
		}
		ok := false
		for _, al := range herdrExecAliases[a] {
			if al == s.Argv0 {
				ok = true
				break
			}
		}
		if !ok {
			errs = append(errs, fmt.Sprintf("resumeSpecs[%q].Argv0=%q が lookup_agent 表に無い", a, s.Argv0))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("agent spec 不整合:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}
