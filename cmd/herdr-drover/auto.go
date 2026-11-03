package main

// auto — セミオート続行（「つづけて」の自動判定）の CLI。
//
//	herdr-drover auto status
//	herdr-drover auto on  [--pane ID|--all] [--phrase 文言] [--grant] [--cap N] [--grant-cap N]
//	herdr-drover auto off [--pane ID|--all]
//	herdr-drover auto install   [--agent claude|codex|all] [--bin PATH]
//	herdr-drover auto uninstall [--agent claude|codex|all]
//	herdr-drover auto judge [--prompt 直近の指示] [--grant] < 最終発話   （判定器の動作確認）
//	herdr-drover auto toggle [--pane ID]   （今いる／フォーカス pane の ON/OFF 反転＋herdr 通知。plugin action auto-toggle＝キー割当用）
//	herdr-drover auto cases [--last N] [--decision continue|stop|cap] [--sid ID] [--full]   （判定事例の一覧）
//	herdr-drover auto cases label <id> <continue|human|done> [note]       （正解ラベル）
//	herdr-drover auto cases export <id> [origin]                          （コーパス用 JSON を出力）
//	herdr-drover stop-hook <agent>      （内部: エージェントの Stop フックから呼ばれる）
//
// 仕組みと根拠は internal/autocontinue の package コメントと SPEC §2.6。

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/4noha/herdr-drover/internal/autocontinue"
	"github.com/4noha/herdr-drover/internal/herdrapi"
)

// autoHookAgents は Stop フックを設置できるエージェント。
var autoHookAgents = []string{"claude", "codex", "opencode"}

func cmdAuto(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("%w: サブコマンドが要る（status|on|off|toggle|install|uninstall|judge|cases）", errUsage)
	}
	st, err := autocontinue.Open()
	if err != nil {
		return err
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "status":
		return autoStatus(st, stdout)
	case "on", "off":
		return autoSet(st, sub == "on", rest, stdout)
	case "install", "uninstall":
		return autoInstall(sub == "install", rest, stdout)
	case "judge":
		return autoJudge(st, rest, os.Stdin, stdout)
	case "toggle":
		return autoToggle(st, rest, stdout)
	case "cases":
		return autoCases(st, rest, stdout)
	}
	return fmt.Errorf("%w: 不明なサブコマンド %q（status|on|off|toggle|install|uninstall|judge|cases）", errUsage, sub)
}

func autoStatus(st *autocontinue.Store, w io.Writer) error {
	cfg, err := st.LoadConfig()
	if err != nil {
		return err
	}
	eff := autocontinue.Resolve(cfg, autocontinue.PaneSetting{}, false)
	fmt.Fprintf(w, "状態ディレクトリ: %s\n", st.Dir)
	fmt.Fprintf(w, "全体既定: enabled=%t phrase=%q cap=%d grant_cap=%d\n", eff.Enabled, eff.Phrase, eff.Cap, eff.GrantCap)
	fmt.Fprintf(w, "判定器: %s\n", strings.Join(eff.JudgeCmd, " "))
	home, err := os.UserHomeDir()
	if err == nil {
		for _, a := range autoHookAgents {
			p, _ := autocontinue.SettingsPath(home, a)
			c, ok, err := autocontinue.Installed(p, a)
			switch {
			case err != nil:
				fmt.Fprintf(w, "フック %-6s: 読取失敗 %s: %v\n", a, p, err)
			case ok:
				fmt.Fprintf(w, "フック %-6s: 設置済 %s (%s)\n", a, p, c)
			default:
				fmt.Fprintf(w, "フック %-6s: 未設置 %s（`herdr-drover auto install` で設置）\n", a, p)
			}
		}
	}
	panes, err := st.ListPanes()
	if err != nil {
		return err
	}
	if len(panes) == 0 {
		fmt.Fprintf(w, "pane 設定: なし\n")
	} else {
		keys := make([]string, 0, len(panes))
		for k := range panes {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Fprintf(w, "pane 設定:\n")
		for _, k := range keys {
			p := panes[k]
			en := "(未指定)"
			if p.Enabled != nil {
				en = fmt.Sprintf("%t", *p.Enabled)
			}
			fmt.Fprintf(w, "  %-10s enabled=%s grant=%t phrase=%q at=%s\n", strings.ReplaceAll(k, "_", ":"), en, p.Grant, p.Phrase, p.At)
		}
	}
	if pane := os.Getenv("HERDR_PANE_ID"); pane != "" {
		p, found, _ := st.LoadPane(pane)
		e := autocontinue.Resolve(cfg, p, found)
		fmt.Fprintf(w, "この pane (%s): enabled=%t grant=%t phrase=%q\n", pane, e.Enabled, e.Grant, e.Phrase)
	}
	if b, err := os.ReadFile(st.LogPath()); err == nil {
		lines := strings.Split(strings.TrimSpace(string(b)), "\n")
		if len(lines) > 5 {
			lines = lines[len(lines)-5:]
		}
		fmt.Fprintf(w, "直近の判定（%s）:\n", st.LogPath())
		for _, l := range lines {
			fmt.Fprintf(w, "  %s\n", l)
		}
	}
	return nil
}

func autoSet(st *autocontinue.Store, on bool, args []string, w io.Writer) error {
	fs := flag.NewFlagSet("auto on/off", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	pane := fs.String("pane", "", "対象 pane id（既定: 環境変数 HERDR_PANE_ID＝今いる pane）")
	all := fs.Bool("all", false, "全体既定を変える（pane 設定が無い pane に適用）")
	phrase := fs.String("phrase", "", "続行時に送る文言（既定「つづけて」）")
	grant := fs.Bool("grant", false, "裁量あり（上限 grant_cap=20。「いけるところまで」相当）")
	capN := fs.Int("cap", 0, "裁量なしの上限回数（--all 時）")
	gcap := fs.Int("grant-cap", 0, "裁量ありの上限回数（--all 時）")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("%w: 余分な引数 %v", errUsage, fs.Args())
	}
	if *all {
		cfg, err := st.LoadConfig()
		if err != nil {
			return err
		}
		cfg.Enabled = &on
		if *phrase != "" {
			cfg.Phrase = *phrase
		}
		if *capN > 0 {
			cfg.Cap = *capN
		}
		if *gcap > 0 {
			cfg.GrantCap = *gcap
		}
		if err := st.SaveConfig(cfg); err != nil {
			return err
		}
		fmt.Fprintf(w, "auto: 全体既定を enabled=%t にしました（%s）\n", on, st.Dir)
		return nil
	}
	if *pane == "" {
		*pane = os.Getenv("HERDR_PANE_ID")
	}
	if *pane == "" {
		return fmt.Errorf("%w: pane が分からない（herdr pane 内で実行するか --pane ID か --all）", errUsage)
	}
	if _, err := autoApply(st, herdrapi.New(""), *pane, on, *phrase, *grant, w); err != nil {
		return err
	}
	if on {
		home, err := os.UserHomeDir()
		if err == nil {
			missing := []string{}
			for _, a := range autoHookAgents {
				sp, _ := autocontinue.SettingsPath(home, a)
				if _, ok, _ := autocontinue.Installed(sp, a); !ok {
					missing = append(missing, a)
				}
			}
			if len(missing) > 0 {
				fmt.Fprintf(w, "auto: ⚠ Stop フック未設置: %s（`herdr-drover auto install` を実行。"+
					"設置後に**新しく始めた**セッションから有効）\n", strings.Join(missing, ", "))
			}
		}
	}
	return nil
}

// autoDefaultBin は設置する command に焼くバイナリの絶対パス。常駐版
// （~/.herdr-drover/bin/herdr-drover）が在ればそれ（更新で中身が入れ替わっても
// パスは不変）、無ければ自分自身。
func autoDefaultBin() (string, error) {
	home, err := os.UserHomeDir()
	if err == nil {
		p := filepath.Join(home, ".herdr-drover", "bin", "herdr-drover")
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(self); err == nil {
		self = r
	}
	return self, nil
}

func autoInstall(install bool, args []string, w io.Writer) error {
	name := "auto uninstall"
	if install {
		name = "auto install"
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	agent := fs.String("agent", "all", "claude|codex|opencode|all")
	bin := fs.String("bin", "", "フックに焼く herdr-drover のパス（既定: ~/.herdr-drover/bin/herdr-drover か自分）")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	var agents []string
	switch *agent {
	case "all":
		agents = autoHookAgents
	case "claude", "codex", "opencode":
		agents = []string{*agent}
	default:
		return fmt.Errorf("%w: --agent は claude|codex|opencode|all", errUsage)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	if install && *bin == "" {
		b, err := autoDefaultBin()
		if err != nil {
			return err
		}
		*bin = b
	}
	for _, a := range agents {
		p, err := autocontinue.SettingsPath(home, a)
		if err != nil {
			return err
		}
		var changed bool
		if install {
			changed, err = autocontinue.Install(p, a, *bin)
		} else {
			changed, err = autocontinue.Uninstall(p, a)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", a, err)
		}
		switch {
		case install && changed:
			fmt.Fprintf(w, "auto: %s の Stop フックを %s に設置しました（command: %s）\n", a, p, autocontinue.HookCommand(*bin, a))
			if a == "codex" {
				fmt.Fprintf(w, "auto: ⚠ codex は非管理フックを各 PC で一度 trust する必要があります。"+
					"codex を起動して `/hooks` を開き、herdr-drover の Stop フックを trust してください"+
					"（未 trust の間はスキップされる＝起動時に警告が出ます）\n")
			}
			if a == "opencode" {
				fmt.Fprintf(w, "auto: ⚠ opencode のプラグインはサーバ起動時に読み込まれます。"+
					"background service を使っている場合は `opencode reload` か service の再起動後、"+
					"`--standalone` のセッションは次に始めたものから有効です\n")
			}
		case install:
			fmt.Fprintf(w, "auto: %s の Stop フックは設置済み（%s）\n", a, p)
		case changed:
			fmt.Fprintf(w, "auto: %s の Stop フックを %s から除去しました\n", a, p)
		default:
			fmt.Fprintf(w, "auto: %s の Stop フックは未設置（%s）\n", a, p)
		}
	}
	if install {
		fmt.Fprintf(w, "auto: フックは**新しく始めた**セッションから有効です。有効化は "+
			"`herdr-drover auto on`（今いる pane）か `--all`\n")
		if _, err := exec.LookPath("claude"); err != nil {
			fmt.Fprintf(w, "auto: ⚠ 判定器の既定コマンド `claude` が PATH に無い＝判定は常に停止側"+
				"（autocontinue.log に fallback が残る）。config.json の judge_cmd で差し替え可\n")
		}
	}
	return nil
}

// autoJudge は判定器をその場で走らせる（設定確認・プロンプト調整用）。
// stdin にエージェントの最終発話を流す。実行には判定器コマンド（既定 claude）が要る。
func autoJudge(st *autocontinue.Store, args []string, stdin io.Reader, w io.Writer) error {
	fs := flag.NewFlagSet("auto judge", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	prompt := fs.String("prompt", "", "直近のユーザ指示（裁量判定・文脈用）")
	grant := fs.Bool("grant", false, "裁量ありとして判定")
	raw := fs.Bool("raw", false, "判定器の生出力も表示")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	msg, err := io.ReadAll(io.LimitReader(stdin, 1<<20))
	if err != nil {
		return err
	}
	cfg, err := st.LoadConfig()
	if err != nil {
		return err
	}
	eff := autocontinue.Resolve(cfg, autocontinue.PaneSetting{}, false)
	g := *grant || autocontinue.GrantIn(*prompt)
	start := time.Now()
	asked := autocontinue.ContinueLike(*prompt)
	jd := autocontinue.NewJudge(eff.JudgeCmd, st.Dir).Judge(context.Background(), string(msg), *prompt, g, asked)
	fmt.Fprintf(w, "verdict=%s source=%s grant=%t asked=%t elapsed=%s", jd.Verdict, jd.Source, g, asked, time.Since(start).Round(time.Millisecond))
	if jd.Detail != "" {
		fmt.Fprintf(w, " detail=%q", jd.Detail)
	}
	fmt.Fprintln(w)
	if *raw && jd.Raw != "" {
		fmt.Fprintf(w, "--- raw ---\n%s\n", strings.TrimSpace(jd.Raw))
	}
	return nil
}

// autoToggle は pane の有効/無効を反転する（キー一発用）。pane は --pane >
// HERDR_PANE_ID（herdr が plugin action にフォーカス pane を注入）> pane.current。
// 結果は herdr の `notification.show`（トースト）と stdout に出す。通知失敗は
// 切替を止めない（stdout と autocontinue.log に残す）。
func autoToggle(st *autocontinue.Store, args []string, w io.Writer) error {
	fs := flag.NewFlagSet("auto toggle", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	pane := fs.String("pane", "", "対象 pane id（既定: HERDR_PANE_ID → pane.current）")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("%w: 余分な引数 %v", errUsage, fs.Args())
	}
	api := herdrapi.New("")
	if *pane == "" {
		*pane = os.Getenv("HERDR_PANE_ID")
	}
	if *pane == "" {
		cur, err := paneCurrent(api)
		if err != nil {
			return fmt.Errorf("%w: pane が分からない（HERDR_PANE_ID 無し・pane.current 失敗: %v）", errUsage, err)
		}
		*pane = cur.PaneID
	}
	on, err := autoEnabledFor(st, *pane)
	if err != nil {
		return err
	}
	_, err = autoApply(st, api, *pane, !on, "", false, w)
	return err
}

// autoEnabledFor は pane の有効/無効（pane 設定 > 全体既定 > 既定 OFF）。
func autoEnabledFor(st *autocontinue.Store, pane string) (bool, error) {
	cfg, err := st.LoadConfig()
	if err != nil {
		return false, err
	}
	p, found, err := st.LoadPane(pane)
	if err != nil {
		return false, err
	}
	return autocontinue.Resolve(cfg, p, found).Enabled, nil
}

// autoApply は pane の ON/OFF を設定ファイルへ書き、Tab 印（⟳ ）とトーストを出す。
// CLI（auto on/off/toggle）と遠隔命令（semi-auto）の共通実体＝どの経路でも
// 見え方が同じになる。phrase/grant は空/false なら触らない。
func autoApply(st *autocontinue.Store, api *herdrapi.Client, pane string, on bool, phrase string, grant bool, w io.Writer) (autocontinue.PaneSetting, error) {
	cfg, err := st.LoadConfig()
	if err != nil {
		return autocontinue.PaneSetting{}, err
	}
	p, _, err := st.LoadPane(pane)
	if err != nil {
		return p, err
	}
	p.Enabled = &on
	if phrase != "" {
		p.Phrase = phrase
	}
	if grant {
		p.Grant = true
	}
	p.At = time.Now().Format(time.RFC3339)
	if err := st.SavePane(pane, p); err != nil {
		return p, err
	}
	eff := autocontinue.Resolve(cfg, p, true)
	state := "OFF"
	if on {
		state = "ON"
	}
	title := fmt.Sprintf("semi-auto %s: %s", state, pane)
	body := "つづけて の自動判定を停止"
	if on {
		body = fmt.Sprintf("つづけて を自動入力（上限 %d 回・裁量あり %d 回）", eff.Cap, eff.GrantCap)
		if p.Grant {
			body = fmt.Sprintf("つづけて を自動入力（裁量あり・上限 %d 回）", eff.GrantCap)
		}
		if home, err := os.UserHomeDir(); err == nil {
			missing := []string{}
			for _, a := range autoHookAgents {
				sp, _ := autocontinue.SettingsPath(home, a)
				if _, ok, _ := autocontinue.Installed(sp, a); !ok {
					missing = append(missing, a)
				}
			}
			if len(missing) > 0 {
				body += "。⚠ Stop フック未設置: " + strings.Join(missing, ",") + "（herdr-drover auto install）"
			}
		}
	}
	fmt.Fprintf(w, "auto: %s — %s\n", title, body)
	_ = st.AppendLog(time.Now(), fmt.Sprintf("set pane=%s → %s (grant=%t phrase=%q)", pane, state, p.Grant, p.Phrase))
	autoTabMark(api, pane, on, w)
	if _, err := api.Call("notification.show", map[string]any{"title": title, "body": body}); err != nil {
		fmt.Fprintf(w, "auto: herdr 通知に失敗（切替自体は完了）: %v\n", err)
	}
	return p, nil
}

// autoSemiAutoEnabled は producer に渡す「pane が ON か」（session doc の semi_auto）。
// 読めない時は false（載せない）＝daemon を止めない。
func autoSemiAutoEnabled(pane string) bool {
	st, err := autocontinue.Open()
	if err != nil {
		return false
	}
	on, err := autoEnabledFor(st, pane)
	return err == nil && on
}

// newDoSemiAuto は遠隔命令 semi-auto の実体。args["on"]="1"/"0"、無ければ反転。
// 切替後は自プロセスへ nudge（SIGUSR1）して session doc の semi_auto を即反映する。
func newDoSemiAuto(api *herdrapi.Client, lg *log.Logger) func(ctx context.Context, sid string, args map[string]string) (string, error) {
	return func(_ context.Context, sid string, args map[string]string) (string, error) {
		st, err := autocontinue.Open()
		if err != nil {
			return "", err
		}
		var on bool
		switch args["on"] {
		case "1", "true":
			on = true
		case "0", "false":
			on = false
		case "":
			cur, err := autoEnabledFor(st, sid)
			if err != nil {
				return "", err
			}
			on = !cur
		default:
			return "", fmt.Errorf("on は 1/0（%q）", args["on"])
		}
		p, err := autoApply(st, api, sid, on, "", false, lg.Writer())
		if err != nil {
			return "", err
		}
		if err := nudgeSelf(); err != nil {
			lg.Printf("[auto] 即時 re-scan の合図に失敗（次の周期 tick で反映）: %v", err)
		}
		state := "OFF"
		if on {
			state = "ON"
		}
		return fmt.Sprintf("semi-auto %s pane=%s grant=%t phrase=%q", state, sid, p.Grant, p.Phrase), nil
	}
}

// autoTabMark は Tab ラベルに ON 印「⟳ 」を付け外しする（トーストは消えるので
// 「いま auto か」が見えるようにする恒久表示）。pane.get→tab_id→tab.get→tab.rename。
// herdr に届かない時は警告のみ（切替自体は成立）。
const autoTabPrefix = "⟳ "

func autoTabMark(api *herdrapi.Client, pane string, on bool, w io.Writer) {
	raw, err := api.Call("pane.get", map[string]any{"pane_id": pane})
	if err != nil {
		fmt.Fprintf(w, "auto: Tab 印の更新に失敗（pane.get）: %v\n", err)
		return
	}
	var pg struct {
		Pane struct {
			TabID string `json:"tab_id"`
		} `json:"pane"`
	}
	if err := json.Unmarshal(raw, &pg); err != nil || pg.Pane.TabID == "" {
		fmt.Fprintf(w, "auto: Tab 印の更新に失敗（pane.get 応答に tab_id が無い）\n")
		return
	}
	raw, err = api.Call("tab.get", map[string]any{"tab_id": pg.Pane.TabID})
	if err != nil {
		fmt.Fprintf(w, "auto: Tab 印の更新に失敗（tab.get）: %v\n", err)
		return
	}
	var tg struct {
		Tab struct {
			Label string `json:"label"`
		} `json:"tab"`
	}
	if err := json.Unmarshal(raw, &tg); err != nil {
		fmt.Fprintf(w, "auto: Tab 印の更新に失敗（tab.get 応答）: %v\n", err)
		return
	}
	label := strings.TrimPrefix(tg.Tab.Label, autoTabPrefix)
	if on {
		label = autoTabPrefix + label
	}
	if label == tg.Tab.Label {
		return
	}
	if _, err := api.Call("tab.rename", map[string]any{"tab_id": pg.Pane.TabID, "label": label}); err != nil {
		fmt.Fprintf(w, "auto: Tab 印の更新に失敗（tab.rename）: %v\n", err)
		return
	}
	fmt.Fprintf(w, "auto: Tab ラベル %q → %q\n", tg.Tab.Label, label)
}

// autoCases は判定事例の閲覧・ラベル・export。
//
//	auto cases [--last N] [--decision continue|stop|cap] [--sid ID] [--full]
//	auto cases label <id> <continue|human|done> [note…]
//	auto cases export <id> [origin…]   … testdata/judge_cases.json に貼る JSON
//
// 「続行した事例の直前の文脈」も全部残るので、続行だけを眺めて時々ある誤続行を
// 見つける（--decision continue）。停止が誤りだった事例は停止後に打った指示が
// followup に付く（『つづけて』なら ↩ 印）。
func autoCases(st *autocontinue.Store, args []string, w io.Writer) error {
	if len(args) > 0 && (args[0] == "label" || args[0] == "export") {
		sub := args[0]
		if len(args) < 2 {
			return fmt.Errorf("%w: auto cases %s <id> …", errUsage, sub)
		}
		id := args[1]
		if sub == "label" {
			if len(args) < 3 {
				return fmt.Errorf("%w: auto cases label <id> <continue|human|done> [note]", errUsage)
			}
			if err := st.LabelCase(id, args[2], strings.Join(args[3:], " "), time.Now()); err != nil {
				return err
			}
			fmt.Fprintf(w, "auto: 事例 %s にラベル %s を付けました\n", id, args[2])
			return nil
		}
		cases, err := st.ReadCases(0)
		if err != nil {
			return err
		}
		for _, c := range cases {
			if c.ID == id {
				cc := autocontinue.ExportCorpus(c, strings.Join(args[2:], " "), time.Now())
				b, _ := json.MarshalIndent(cc, "", "  ")
				fmt.Fprintln(w, string(b))
				fmt.Fprintf(w, "# ↑ internal/autocontinue/testdata/judge_cases.json に追記し、expect / expect_source を確認して TestJudgeCorpus を回す\n")
				return nil
			}
		}
		return fmt.Errorf("事例 %s が見つからない（%s）", id, st.CasesPath())
	}
	fs := flag.NewFlagSet("auto cases", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	last := fs.Int("last", 30, "末尾 N 件")
	decision := fs.String("decision", "", "continue|stop|cap で絞る")
	sid := fs.String("sid", "", "セッション id で絞る")
	full := fs.Bool("full", false, "最終発話を切らずに出す")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	cases, err := st.ReadCases(0)
	if err != nil {
		return err
	}
	var sel []autocontinue.Case
	for _, c := range cases {
		if *decision != "" && c.Decision != *decision {
			continue
		}
		if *sid != "" && c.SID != *sid {
			continue
		}
		sel = append(sel, c)
	}
	if *last > 0 && len(sel) > *last {
		sel = sel[len(sel)-*last:]
	}
	fmt.Fprintf(w, "事例 %d 件（全 %d 件・%s）\n", len(sel), len(cases), st.CasesPath())
	for _, c := range sel {
		mark := ""
		if c.FollowupContinue != nil && *c.FollowupContinue {
			mark = " ↩人が『つづけて』"
		} else if c.FollowupPrompt != "" {
			mark = " ↩人: " + strings.ReplaceAll(tailRunes(c.FollowupPrompt, 40), "\n", " ")
		}
		if c.Label != "" {
			mark += " [label=" + c.Label + "]"
		}
		fmt.Fprintf(w, "— %s  %s %s pane=%s %s/%d rules=v%d\n   %s%s\n", c.At, c.ID, c.Agent, c.Pane, c.Decision, c.Cap, c.Rules,
			strings.TrimSpace(fmt.Sprintf("verdict=%s src=%s %s", c.Verdict, c.Source, c.Detail)), mark)
		if c.Prompt != "" {
			fmt.Fprintf(w, "   指示: %s\n", strings.ReplaceAll(tailRunes(c.Prompt, 80), "\n", " "))
		}
		msg := c.Message
		if !*full {
			msg = tailRunes(msg, 240)
		}
		fmt.Fprintf(w, "   発話: %s\n", strings.ReplaceAll(strings.TrimSpace(msg), "\n", " ⏎ "))
	}
	return nil
}

func tailRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return "…" + string(r[len(r)-n:])
}
