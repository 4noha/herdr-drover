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
	"sort"
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
	"claude": {Supported: true, Flag: "--resume", Aliases: []string{"-r"}, Form: FormSpace, Kinds: []string{"id"}},
	// devin は `-r, --resume [<SESSION_ID>]`（値は任意）。⚠clap の「値が任意」形は
	// スペース区切りだと値を拾わないことがあるので実測した（3000.2.17）:
	// `devin --resume <id>` はパースを通り（対照の裸位置引数は
	// `error: unexpected argument` で弾かれる）＝**スペース形で値が付く**。
	"devin":    {Supported: true, Flag: "--resume", Aliases: []string{"-r"}, Form: FormSpace, Kinds: []string{"id"}},
	"droid":    {Supported: true, Flag: "--resume", Form: FormSpace, Kinds: []string{"id"}},
	"hermes":   {Supported: true, Flag: "--resume", Form: FormSpace, Kinds: []string{"id"}},
	"qodercli": {Supported: true, Flag: "--resume", Form: FormSpace, Kinds: []string{"id"}},
	// argv[0] が agent 名と違う唯一の例。
	"cursor": {Supported: true, Flag: "--resume", Form: FormSpace, Argv0: "cursor-agent", Kinds: []string{"id"}},
	// `<bin> --session <v>`
	"kimi": {Supported: true, Flag: "--session", Form: FormSpace, Kinds: []string{"id"}},
	// opencode は `-s, --session <id>`（実測 1.18.10）。⚠`-c, --continue` は「直前の
	// セッション」で **id を取らない**別物なので alias に入れない（入れると値のある
	// --session を落とし損ねる／付け直しで意味が変わる）。
	"opencode": {Supported: true, Flag: "--session", Aliases: []string{"-s"}, Form: FormSpace, Kinds: []string{"id"}},
	"kilo":     {Supported: true, Flag: "--session", Form: FormSpace, Kinds: []string{"id"}},
	"pi":       {Supported: true, Flag: "--session", Form: FormSpace, Kinds: []string{"id", "path"}},
	// `<bin> --thread <v>`
	"mastracode": {Supported: true, Flag: "--thread", Form: FormSpace, Kinds: []string{"id"}},
	// `<bin> --resume=<v>` — copilot は `-r, --resume[=value]`（実測 1.0.75。
	// --help の例も `copilot --resume=<session-id>` 形）。値は session ID / task ID /
	// ID prefix / 名前を受ける。
	"copilot": {Supported: true, Flag: "--resume", Aliases: []string{"-r"}, Form: FormEquals, Kinds: []string{"id"}},
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
	// 版の出力: "2.1.220 (Claude Code)"
	"claude": {VersionArgv: []string{"--version"}, UpdateArgv: []string{"update"}, Timeout: 15 * time.Minute},
	// codex: `codex update`（--help に "Update Codex to the latest version"）。
	// 版の出力: "codex-cli 0.145.0"
	"codex": {VersionArgv: []string{"--version"}, UpdateArgv: []string{"update"}, Timeout: 15 * time.Minute},
	// cursor: `cursor-agent update`（--help に "Update Cursor Agent to the latest version"）。
	// 版の出力: "2026.07.23-e383d2b"
	"cursor": {VersionArgv: []string{"--version"}, UpdateArgv: []string{"update"}, Timeout: 15 * time.Minute},
	// copilot: `copilot update [channel]`（--help "Download the latest version"・
	// channel は stable/prerelease）。**非対話で完走することを実測**（1.0.75・
	// stdin を閉じて rc=0 / "No update needed, current version is 1.0.75, fetched
	// latest release is v1.0.75"）。版の出力は 2 行:
	//   "GitHub Copilot CLI 1.0.75."
	//   "Run 'copilot update' to check for updates."
	// DL 規模は claude の ~250MB とは桁違いに小さい（実測の版チェックは数秒）が、
	// 遅回線を見込んで他と同じ上限に揃える。
	"copilot": {VersionArgv: []string{"--version"}, UpdateArgv: []string{"update"}, Timeout: 15 * time.Minute},
	// opencode: `opencode upgrade`（--help "upgrade opencode to the latest or a
	// specific version"）。**導入経路を自分で検知する**のが devin との決定的な違いで、
	// 実測（1.18.10・brew 導入）では `Using method: brew` と自己申告し、最新なら
	// `upgrade skipped: 1.18.10 is already installed` で **非対話 rc=0 完走**した。
	// ＝brew 管理と食い違わないので自己更新を載せてよい。
	// 版の出力は素の "1.18.10"（製品名を含まない）。
	"opencode": {VersionArgv: []string{"--version"}, UpdateArgv: []string{"upgrade"}, Timeout: 15 * time.Minute},
	// devin: **`UpdateArgv` を意図的に nil にする**（＝自己更新の口なし・再起動のみ）。
	// `devin update` サブコマンド自体は存在するが（--help "Check for updates and
	// optionally install them"）、**非対話では完走しない**ことを実測した
	// （3000.2.17・stdin を閉じて rc=130・出力 9 バイトのみ・版も変わらず）。
	// 自動化から呼ぶと固まるか黙って失敗するので載せない。加えて本 CLI は
	// Homebrew cask（`brew install --cask devin-cli`）で入るため、自己更新させると
	// brew の管理と食い違う。更新は `brew upgrade --cask devin-cli` を人が行い、
	// drover は**セッション再起動だけ**を担当するのが正しい分担。
	// 版の出力: "devin 3000.2.17 (2c489dfc)"
	"devin": {VersionArgv: []string{"--version"}, Timeout: 15 * time.Minute},
}

// Updater は agent の UpdaterSpec と、載っているかを返す。
func Updater(agent string) (UpdaterSpec, bool) { s, ok := updaterSpecs[agent]; return s, ok }

// UpdaterAgents は UpdaterSpec を持つ canonical label を昇順で返す。
//
// ⚠**呼び手のメッセージに種別名をハードコードしないため**にある。以前は
// 「更新口を持つエージェントは現状 claude のみ」と出力に焼いてあり、codex/cursor を
// 足した後も直っていなかった（利用者に嘘を表示していた）。表から導出すれば
// Spec を足すだけで文言が追随する。
func UpdaterAgents() []string {
	out := make([]string, 0, len(updaterSpecs))
	for a := range updaterSpecs {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

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
	// copilot: `npm install -g @github/copilot` が `copilot` を PATH に置く
	// （実測 /opt/homebrew/bin/copilot・要 Node 22+）。herdr の alias 表は
	// copilot / github-copilot / ghcs だが、**実行ファイル名として確認できたのは
	// copilot のみ**なので候補もそれだけに絞る（cursor で `agent` を外したのと同じ理由
	// ＝未確認の名前を候補に混ぜると別物を掴みうる）。
	"copilot": {BinNames: []string{"copilot"}},
	// opencode: `brew install anomalyco/tap/opencode`（公式 tap・要 ripgrep）が
	// `opencode` を PATH に置く（実測 /opt/homebrew/bin/opencode → Cellar への symlink）。
	// 公式は curl インストーラと `npm i -g opencode-ai` も提供する。
	// alias 表の open-code は**実行ファイル名としては未確認**なので載せない。
	"opencode": {BinNames: []string{"opencode"}},
	// devin: `brew install --cask devin-cli` が `devin` を PATH に置く
	// （実測 /opt/homebrew/bin/devin。公式は curl インストーラも提供）。
	// alias 表の devin-cli は**実行ファイル名としては未確認**なので載せない。
	"devin": {BinNames: []string{"devin"}},
}

// Install は agent の InstallSpec と、載っているかを返す。
func Install(agent string) (InstallSpec, bool) { s, ok := installSpecs[agent]; return s, ok }

// ModelSpec は「起動時のモデル指定」の差分。
//
// ⚠**モデル名は agent 固有**（claude=`opus`/`sonnet`、codex=`gpt-5`、
// cursor=`sonnet-4-thinking`）。フラグ名がたまたま同じでも**値は互換でない**ので、
// 種別を跨いで同じ値を渡してはいけない。呼び手は `--model` を使うとき
// **必ず対象種別を 1 つに絞る**こと。
type ModelSpec struct {
	Flag string // "--model"
	// Aliases は strip 対象の別表記。**落とすときだけ広く、付けるときは Flag に
	// 統一する**（残すと二重指定になる。codex の `-m` で実際に起きうる）。
	Aliases []string
}

// modelSpecs — 実 CLI の --help で確認した agent のみ載せる（推測で書かない）。
// ここに無い agent に `--model` を渡してはいけない（呼び手が loud に撥ねる）。
var modelSpecs = map[string]ModelSpec{
	"claude": {Flag: "--model"},                          // 短縮形なし（実測 2.1.220）
	"codex":  {Flag: "--model", Aliases: []string{"-m"}}, // `-m, --model <MODEL>`
	"cursor": {Flag: "--model"},                          // `--model <model>`
	// copilot: `--model <model>`（"use 'auto' to let Copilot pick automatically"）。
	// 短縮形なし（実測 1.0.75）。
	"copilot": {Flag: "--model"},
	// opencode: `-m, --model <provider/model>`（実測 1.18.10。値は "provider/model" 形＝
	// 他エージェントと語彙がまったく違う good example）。短縮形 -m あり。
	"opencode": {Flag: "--model", Aliases: []string{"-m"}},
	// devin: `--model <MODEL>`（例 "claude-sonnet-4" / "opus" / "codex"・env DEVIN_MODEL）。
	// 短縮形なし（実測 3000.2.17）。⚠値の語彙は agent 固有＝claude の `opus` と
	// 文字列が被っていても**同じ意味とは限らない**（型 doc の警告どおり種別を跨がない）。
	"devin": {Flag: "--model"},
}

// Model は agent の ModelSpec と、載っているかを返す。
func Model(agent string) (ModelSpec, bool) { s, ok := modelSpecs[agent]; return s, ok }

// StripModel は argv からモデル指定を取り除く（Flag と Aliases の両方・
// `--model=<v>` 形も含む）。Spec が無い agent では argv を触らない。
func StripModel(agent string, argv []string) []string {
	if len(argv) == 0 {
		return nil
	}
	sp, ok := modelSpecs[agent]
	if !ok {
		return append([]string(nil), argv...)
	}
	flags := append([]string{sp.Flag}, sp.Aliases...)
	out := make([]string, 0, len(argv))
	out = append(out, argv[0])
	for i := 1; i < len(argv); i++ {
		a := argv[i]
		hit := false
		for _, f := range flags {
			if a == f {
				// 値を取るフラグ。次がフラグ形でなければ値も落とす。
				if i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "-") {
					i++
				}
				hit = true
				break
			}
			if strings.HasPrefix(a, f+"=") {
				hit = true
				break
			}
		}
		if !hit {
			out = append(out, a)
		}
	}
	return out
}

// BuildModel は argv のモデル指定を model へ張り替える。
// model が空、または agent が ModelSpec を持たなければ **argv を一切いじらない**
// （知らないエージェントに勝手なフラグを足さない）。
func BuildModel(agent string, argv []string, model string) []string {
	if len(argv) == 0 {
		return nil
	}
	sp, ok := modelSpecs[agent]
	if !ok || model == "" {
		return append([]string(nil), argv...)
	}
	return append(StripModel(agent, argv), sp.Flag, model)
}

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
