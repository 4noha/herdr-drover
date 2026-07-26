package main

// claude 本体の更新＋セッション反映を **1 コマンド**で行う
// （ローカル CLI `update-claude` ／遠隔命令 update-claude）。
//
// `claude update` はバイナリ（symlink `~/.local/bin/claude` → `versions/<ver>`）を
// 差し替えるだけで、**すでに走っているセッションには効かない**（exec 済みプロセスは
// 旧 inode に貼り付く）。逆に restart-claude だけではディスク上が古いままなら
// 何も新しくならない。実運用で欲しいのは常に「更新して、セッションへ反映」なので
// この 2 段を 1 コマンドに閉じる。
//
// ⚠**claude バイナリを PATH 頼みで決めない**（daemon の PATH には ~/.local/bin が
// 無い）。権威は restart-claude と同じ「稼働中のローカル claude pane が実際に
// 起動している argv[0]」＝更新すべき実体そのもの。どの経路で決めたかは必ず出力する
// （silent な選択をしない・鉄則⑤）。
//
// 更新の有無に関わらず**再起動まで行う**: 「ディスクは最新だがセッションは旧版」が
// まさに直したい状態（実測 2026-07-25: disk 2.1.219 / セッション 2.1.214）。
// 「更新が無かったから何もしない」は目的を達成しない。作業中 pane の skip 等の
// 安全弁は restart-claude の芯（restartClaudePanes）をそのまま共有する。

import (
	"context"
	"errors"
	"fmt"
	"github.com/4noha/herdr-drover/internal/agentid"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"flag"

	"github.com/4noha/drover-cloud/selfupdate"
	"github.com/4noha/herdr-drover/internal/herdrapi"
)

// claudeUpdateTimeout は `claude update`（ダウンロード込み）の上限。遠隔命令は
// この呼び出しの間ブロックするので無限には待たない。
//
// ⚠実測 2026-07-25 で 5 分は**短すぎた**: claude 本体は ~250MB あり、ノート PC の
// Wi-Fi では 5 分を超えて `signal: killed`（＝この上限による中断）になった。
// 有線/高速回線の 2 台は 4 分以内に完了しており、回線差が素直に出る。上限は
// 「暴走を止める backstop」であって「普通は間に合う値」であるべきなので 15 分。
// updateTimeoutFor はそのエージェント単体の更新予算（UpdaterSpec.Timeout）。
// **全体に 1 本掛けない** — 複数エージェントを更新するとき、遅い 1 つが他の
// 予算を食い潰さないようにする。Spec が無い場合の既定は claude 実測由来の 15 分。
func updateTimeoutFor(agent string) time.Duration {
	if sp, ok := agentid.Updater(agent); ok && sp.Timeout > 0 {
		return sp.Timeout
	}
	return claudeUpdateTimeout
}

const claudeUpdateTimeout = 15 * time.Minute

// claudeBinsFromPanes は稼働中のローカル claude pane が実際に起動している argv[0]
// を重複なく集める（restart-claude と同じ exact な権威＝PATH を引かない）。
// claudeBinsFromPanes は稼働中の claude pane の launch_argv[0] から**更新対象
// バイナリの候補**を集める。
//
// ⚠identity を herdr の検出値まで広げた（P1）結果、drover が起動していない pane も
// ここに来る。そのまま argv[0] を候補に入れると:
//   - `herdr agent start w -- claude` の相対名 "claude" が混ざり「2 種類＝曖昧」
//     error が恒常発火して update が丸ごと止まる
//   - それが単独だと相対名が採用され、launchd daemon（PATH に ~/.local/bin が
//     無い）で exec 失敗＝このファイル冒頭で禁じた PATH 依存の再導入になる
//
// よって **絶対パスかつ claude の直接起動**の argv[0] だけを候補にする。
// 落としたものは必ず 1 行出す（silent skip 禁止）。
func claudeBinsFromPanes(api *herdrapi.Client, agent string, out io.Writer) ([]string, error) {
	agents, err := api.AgentList()
	if err != nil {
		return nil, fmt.Errorf("agent.list: %w", err)
	}
	targets, conflicts, err := selectRestartTargets(agents, "", agent)
	// 失敗経路（バイナリ特定に失敗して restartClaudePanes へ到達しない場合）でも
	// 「なぜこの pane が対象外なのか」が残るよう、ここでも矛盾を報告する。
	for _, c := range conflicts {
		fmt.Fprintf(out, "update-agent-cli: skip  %s\n", c)
	}
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	var bins []string
	for _, t := range targets {
		root, err := exportTabLayout(api, t.TabID)
		if err != nil {
			continue // 1 枚読めなくても他から決まる（決まらなければ下で loud に error）
		}
		for _, l := range root.leaves() {
			if l.PaneID != t.PaneID || len(l.Command) == 0 {
				continue
			}
			bin := l.Command[0]
			if !agentid.IsDirectInvocation(agent, l.Command) {
				fmt.Fprintf(out, "update-agent-cli: note: pane=%s は %s の直接起動でない"+
					"（argv[0]=%q）＝バイナリ特定の根拠にしない\n", t.PaneID, agent, bin)
				continue
			}
			if !filepath.IsAbs(bin) {
				fmt.Fprintf(out, "update-agent-cli: note: pane=%s の argv[0] が相対名 %q "+
					"＝daemon の PATH では解決できないのでバイナリ特定の根拠にしない\n", t.PaneID, bin)
				continue
			}
			if !seen[bin] {
				seen[bin] = true
				bins = append(bins, bin)
			}
		}
	}
	return bins, nil
}

// resolveClaudeBin は更新対象の claude 実バイナリを決め、根拠（source）も返す。
// 優先順:
//  1. 稼働中のローカル claude pane の argv[0]（exact。**食い違う複数は曖昧＝error**
//     ＝どれを更新すべきか推測しない）
//  2. PATH（CLI 実行時に有効。daemon では通常引けない）
//  3. `~/.local/bin/claude`（native installer の既定配置＝推測ではなく既知の規約）
func resolveClaudeBin(api *herdrapi.Client, agent string, out io.Writer) (bin, source string, err error) {
	bins, berr := claudeBinsFromPanes(api, agent, out)
	if berr == nil && len(bins) == 1 {
		return bins[0], fmt.Sprintf("稼働中の %s pane の argv[0]", agent), nil
	}
	if berr == nil && len(bins) > 1 {
		return "", "", fmt.Errorf("稼働中の %s pane が %d 種類のバイナリを使っていて"+
			"更新対象を一意に決められない（推測しない）: %s",
			agent, len(bins), strings.Join(bins, " / "))
	}
	sp, ok := agentid.Install(agent)
	if !ok {
		return "", "", fmt.Errorf("%s の導入情報（InstallSpec）が無く、稼働 pane からも"+
			"バイナリを特定できない＝更新対象を推測しない", agent)
	}
	for _, name := range sp.BinNames {
		if p, lerr := exec.LookPath(name); lerr == nil {
			if abs, aerr := filepath.Abs(p); aerr == nil {
				return abs, "PATH の " + name, nil
			}
		}
	}
	if home, herr := os.UserHomeDir(); herr == nil {
		for _, wk := range sp.WellKnownPaths {
			p := filepath.Join(home, strings.TrimPrefix(wk, "~/"))
			if fi, serr := os.Stat(p); serr == nil && !fi.IsDir() {
				return p, "既定の導入先 " + wk, nil
			}
		}
	}
	return "", "", fmt.Errorf("%s の実バイナリを特定できない"+
		"（セッションが 1 つも無く、PATH %v にも既定の導入先 %v にも見つからない）",
		agent, sp.BinNames, sp.WellKnownPaths)
}

// claudeVersion は `<bin> --version` の 1 行（例 "2.1.219 (Claude Code)"）。
// 版取得の引数はエージェントごとに違いうる（UpdaterSpec.VersionArgv）。
// nil = 版を問い合わせる口が無い＝呼び手は「版比較 skip・更新有無不明」と
// 明示ログして再起動段へ進む（更新前に return しない）。
func claudeVersion(ctx context.Context, agent, bin string) (string, error) {
	sp, ok := agentid.Updater(agent)
	if !ok || sp.VersionArgv == nil {
		return "", errNoVersionProbe
	}
	out, err := exec.CommandContext(ctx, bin, sp.VersionArgv...).Output()
	if err != nil {
		return "", fmt.Errorf("%s %v: %w", bin, sp.VersionArgv, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// errNoVersionProbe は「版を問い合わせる口が無い」ことを示す番兵。
// **失敗ではない**ので呼び手は中断せず、版比較を skip して再起動段へ進む。
var errNoVersionProbe = errors.New("このエージェントは版を問い合わせる口を持たない")

// errNoUpdater は「自己更新の口が無い」ことを示す番兵（再起動のみ行う）。
var errNoUpdater = errors.New("このエージェントは自己更新の口を持たない")

// runClaudeUpdate は `<bin> update` を実行して結合出力を返す。実測 2.1.219: 最新でも
// exit 0 で "Claude Code is up to date (x.y.z)" を出す＝exit code だけでは
// 「更新された/されていない」は判定できないので、版の前後比較を権威にする。
func runClaudeUpdate(ctx context.Context, agent, bin string) (string, error) {
	sp, ok := agentid.Updater(agent)
	if !ok || sp.UpdateArgv == nil {
		return "", errNoUpdater
	}
	out, err := exec.CommandContext(ctx, bin, sp.UpdateArgv...).CombinedOutput()
	text := strings.TrimSpace(string(out))
	// 上限で kill したときの生エラーは "signal: killed" で**原因が読み取れない**
	// （実測 2026-07-25: ユーザーからは回線が遅いのか claude が壊れたのか区別が
	// つかなかった）。ctx の期限切れなら理由と対処を明示して返す。
	if err != nil && ctx.Err() != nil {
		return text, fmt.Errorf("%v の上限内に終わらず中断した"+
			"（回線が遅い/更新が大きい。エージェント本体は数百 MB ある）: %w", sp.Timeout, ctx.Err())
	}
	return text, err
}

// updateClaudeAndRestart は「claude 更新 → セッションへ反映」の芯。CLI と遠隔命令が
// 共有する（経路ごとに別ロジックを持たない）。戻り値は監査用の 1 行要約。
func updateClaudeAndRestart(ctx context.Context, api *herdrapi.Client, opt restartOptions,
	out io.Writer) (string, error) {
	// opt.Agent 空は「全エージェント」だが、更新は**種別ごと**に本体が違う。
	// 空なら claude を既定にする（どの種別を更新したかは必ず 1 行出す）。
	// ⚠選べる種別は **UpdaterSpec の表から導出**する。以前は「更新口を持つのは
	// 現状これのみ」と文言に焼いてあり、codex/cursor を足した後も直っておらず
	// 利用者に嘘を表示していた（agentid.UpdaterAgents の doc 参照）。
	agent := opt.Agent
	if agent == "" {
		agent = "claude"
		fmt.Fprintf(out, "update-agent-cli: note: --agent 未指定＝%s を対象にする"+
			"（更新口を持つのは %s。--agent で選べる）\n",
			agent, strings.Join(agentid.UpdaterAgents(), " / "))
	}
	if _, ok := agentid.Updater(agent); !ok {
		return "", fmt.Errorf("%s は更新口（UpdaterSpec）を持たない"+
			"＝どうやって更新するか推測しない（手動更新後に restart-agent-session を使う）", agent)
	}

	bin, source, err := resolveClaudeBin(api, agent, out)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(out, "update-agent-cli: 対象バイナリ %s（根拠: %s）\n", bin, source)

	// 版が取れないのは**失敗ではない**（口を持たないエージェントがある）。
	// 更新有無が不明になるだけなので、明示ログして先へ進む。
	before, err := claudeVersion(ctx, agent, bin)
	if errors.Is(err, errNoVersionProbe) {
		fmt.Fprintf(out, "update-agent-cli: note: %s は版を問い合わせる口が無い"+
			"＝更新有無は判定できない（版比較 skip）\n", agent)
		before, err = "", nil
	}
	if err != nil {
		return "", err
	}

	if opt.DryRun {
		usp, _ := agentid.Updater(agent)
		fmt.Fprintf(out, "update-agent-cli: [dry-run] 現在 %s → `%s %s` を実行し"+
			"セッションを再起動します\n", before, bin, strings.Join(usp.UpdateArgv, " "))
		dry := opt
		dry.Agent = agent
		dry.DryRun = true
		if _, rerr := restartClaudePanes(api, dry, out); rerr != nil {
			return "", rerr
		}
		return fmt.Sprintf("[dry-run] %s（更新も再起動も未実行）", before), nil
	}

	updOut, uerr := runClaudeUpdate(ctx, agent, bin)
	if errors.Is(uerr, errNoUpdater) {
		// 自己更新の口が無いエージェントは更新段を skip して再起動だけ行う。
		fmt.Fprintf(out, "update-agent-cli: note: %s は自己更新の口が無い＝更新段を skip し"+
			"セッション再起動のみ行う\n", agent)
		updOut, uerr = "", nil
	}
	if updOut != "" {
		for _, line := range strings.Split(updOut, "\n") {
			fmt.Fprintf(out, "update-agent-cli: claude> %s\n", line)
		}
	}
	if uerr != nil {
		// 更新に失敗しても**セッション再起動へは進まない**（古いままで作り直しても
		// 目的を達さず、pane を無駄に作り直すだけ）。
		return "", fmt.Errorf("`%s` の更新失敗: %w", bin, uerr)
	}

	after, err := claudeVersion(ctx, agent, bin)
	if errors.Is(err, errNoVersionProbe) {
		after, err = "", nil
	}
	if err != nil {
		return "", err
	}
	verMsg := "既に最新 " + after
	switch {
	case before == "" && after == "":
		verMsg = "版不明（版を問い合わせる口が無い＝更新有無は判定できない）"
	case before != after:
		verMsg = fmt.Sprintf("更新 %s → %s", before, after)
	}
	fmt.Fprintf(out, "update-agent-cli: %s\n", verMsg)

	// 版が変わらなくても再起動する: ディスクが最新でもセッションが旧版のまま、
	// というのがまさに直したい状態（本コマンドの存在理由）。
	run := opt
	run.Agent = agent
	run.DryRun = false
	results, rerr := restartClaudePanes(api, run, out)
	if rerr != nil {
		return "", fmt.Errorf("%s／セッション再起動 失敗: %w", verMsg, rerr)
	}
	return verMsg + " / " + summarizeRestart(results), nil
}

// cmdUpdateClaude は CLI 入口。
func cmdUpdateClaude(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("update-claude", flag.ContinueOnError)
	fs.SetOutput(stderr)
	force := fs.Bool("force", false, "既定の安全網を外す（作業中 pane・会話 ref が取れない pane も再起動する）")
	dryRun := fs.Bool("dry-run", false, "対象バイナリと再起動予定を表示するだけで何もしない")
	model := fs.String("model", "", "再起動時に claude へ渡すモデル（例 opus）。空なら既存指定に触らない")
	agentFlag := fs.String("agent", "", "更新するエージェント種別（空なら更新口を持つ既定＝claude）")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	rest := fs.Args()
	if len(rest) > 1 {
		return fmt.Errorf("%w: 余分な引数 %v（sid は 1 つまで）", errUsage, rest[1:])
	}
	sid := ""
	if len(rest) == 1 {
		sid = rest[0]
	}

	api := herdrapi.New("")
	if _, err := api.Ping(); err != nil {
		return fmt.Errorf("herdr server に繋がらない（socket=%s）: %w", api.SocketPath, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), updateTimeoutFor(*agentFlag))
	defer cancel()
	summary, err := updateClaudeAndRestart(ctx, api,
		restartOptions{SID: sid, Agent: *agentFlag, Force: *force, DryRun: *dryRun, Model: *model}, stdout)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "update-agent-cli: %s\n", summary)
	return nil
}

// ===== update-all（Web のワンボタン: claude 更新 → 自己更新 → 再起動） =====

// updateAllRunning は update-all の同時実行ガード。遠隔命令の watcher は逐次
// 呼び出しだが、CLI 併用や将来の並列化で二重に走ると「claude 更新中に自分を
// 置換して exit」のような順序崩れが起きる。**逐次実行は本機能の正しさの前提**
// なので、重なりは黙って直列化せず loud に拒否する。
var updateAllRunning atomic.Bool

// updateAllResult は各段の結果（監査 detail の組み立て用）。
type updateAllResult struct {
	Claude string // エージェント本体＋セッション反映の要約（複数なら連結）
	Self   string // herdr-drover 自身の更新結果
}

// runUpdateAll は「claude 本体更新＋セッション反映 → herdr-drover 自身の更新」を
// **この順で逐次**実行する。CLI と遠隔命令が共有する芯。
//
// ⚠**順序は入れ替えられない**: 自分自身の更新を反映するにはプロセスを終了して
// launchd に再起動させるしかなく、exit した時点でハンドラは終わる＝それ以降の段は
// 実行されない。よって「自分の再起動」は必ず最後に置き、claude 側を先に完了させる。
// （自己更新を先にしても、走っているプロセスは旧 inode のままなので新コードでは
// 動かない＝先にやる利点が無い。実行順を単純に保つ方が正しい。）
//
// 戻り値の restart は「呼び手が exit すべきか」。selfupdate が更新した/しないに
// 関わらず true を返す＝Web の「再起動」ボタンを本コマンドが**包含**する
// （3 ボタンを 1 つに集約するという要件）。err 時は false（原因調査のため状態を保つ）。
// updatableAgents は「このマシンで更新できるエージェント」を canonical 順で返す。
// **UpdaterSpec を持つ** かつ **実バイナリが解決できる**もののみ。
// 除外は黙って落とさず 1 行出す（silent skip 禁止）。
func updatableAgents(out io.Writer) []string {
	var all []string
	for label := range agentid.CanonicalLabels {
		all = append(all, label)
	}
	sort.Strings(all) // 実行順を決定的にする（ログ比較・再現性）
	var got []string
	for _, label := range all {
		if _, ok := agentid.Updater(label); !ok {
			continue // 更新口が未登録＝そもそも候補でない（毎回出すと煩いので黙る）
		}
		if _, err := lookupAgentBin(label); err != nil {
			fmt.Fprintf(out, "update-all: note: %s は更新口を持つがこのマシンに未導入＝skip\n", label)
			continue
		}
		got = append(got, label)
	}
	return got
}

func runUpdateAll(ctx context.Context, api *herdrapi.Client, opt restartOptions,
	selfUpdate func() (string, bool, error), out io.Writer) (updateAllResult, bool, error) {
	if !updateAllRunning.CompareAndSwap(false, true) {
		return updateAllResult{}, false, fmt.Errorf("update-all が既に実行中（逐次実行が前提のため二重起動は拒否）")
	}
	defer updateAllRunning.Store(false)

	var res updateAllResult

	// 段 1: **導入済み × 更新口を持つ**エージェントを順に更新＋セッション反映。
	//
	// 対象の決め方（推測しない）:
	//   - UpdaterSpec を持つ（更新方法が実 CLI で確認済み）
	//   - このマシンに実際に導入されている（InstallSpec でバイナリが解決できる）
	// 未導入や更新口不明のエージェントは**黙って飛ばさず** 1 行出す。
	//
	// ⚠**設計判断（v0.5.27 で確定）**: 1 つのエージェントの失敗で他や自己更新を
	// 止めない。失敗は集約して Ack に残す。理由は、例えば cursor の更新失敗で
	// **herdr-drover 自身の更新まで止まる**のが実運用で最も困るから
	// （自己更新は不具合修正の唯一の配布経路）。ただし**エージェント単位**では
	// 従来どおり「更新に失敗したらそのセッションは触らない」を維持する
	//（古いまま作り直しても目的を達さず pane を無駄に作り直すだけ）。
	targets := updatableAgents(out)
	if len(targets) == 0 {
		res.Claude = "更新対象のエージェントなし"
		fmt.Fprintf(out, "update-all: [1/2] 更新対象のエージェントが無い（導入済みかつ更新口を持つものが 0）\n")
	}
	var summaries, failures []string
	for i, agent := range targets {
		fmt.Fprintf(out, "update-all: [1/2] %s の更新とセッション反映（%d/%d）\n",
			agent, i+1, len(targets))
		aopt := opt
		aopt.Agent = agent
		sum, aerr := updateClaudeAndRestart(ctx, api, aopt, out)
		if aerr != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", agent, aerr))
			fmt.Fprintf(out, "update-all: %s の更新に失敗（他のエージェントと自己更新は続行）: %v\n",
				agent, aerr)
			continue
		}
		summaries = append(summaries, agent+": "+sum)
	}
	if len(summaries) > 0 {
		res.Claude = strings.Join(summaries, " ／ ")
	}
	if len(failures) > 0 {
		if res.Claude != "" {
			res.Claude += " ／ "
		}
		res.Claude += "失敗 " + strings.Join(failures, " ／ ")
	}

	// 段 2: herdr-drover 自身。バイナリはディスク上で置換されるだけで、走っている
	// プロセスは旧 inode のまま＝反映は次段の再起動で行う。
	fmt.Fprintf(out, "update-all: [2/2] herdr-drover 自身の更新\n")
	tag, updated, serr := selfUpdate()
	if serr != nil {
		res.Self = "失敗"
		return res, false, fmt.Errorf("herdr-drover 更新: %w", serr)
	}
	if updated {
		res.Self = "更新 " + tag
	} else {
		res.Self = "既に最新 " + tag
	}
	fmt.Fprintf(out, "update-all: herdr-drover %s\n", res.Self)
	return res, true, nil
}

// summarizeUpdateAll は監査 detail の 1 行要約（純関数）。
func summarizeUpdateAll(res updateAllResult) string {
	parts := make([]string, 0, 2)
	if res.Claude != "" {
		// Claude フィールドは複数エージェントの結果を連結した文字列（v0.5.27〜）。
		// 各要素が既に "<agent>: …" 形なので、ここで種別名を前置しない。
		parts = append(parts, res.Claude)
	}
	if res.Self != "" {
		parts = append(parts, "drover: "+res.Self)
	}
	if len(parts) == 0 {
		return "実行結果なし"
	}
	return strings.Join(parts, " / ")
}

// cmdUpdateAll は CLI 入口。CLI では自プロセスを exit しても意味が無いので、
// 常駐 daemon への反映手段（kickstart）を案内して終わる。
func cmdUpdateAll(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("update-all", flag.ContinueOnError)
	fs.SetOutput(stderr)
	force := fs.Bool("force", false, "既定の安全網を外す（作業中 pane・会話 ref が取れない pane も再起動する）")
	model := fs.String("model", "", "再起動時に claude へ渡すモデル（例 opus）")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	if rest := fs.Args(); len(rest) > 0 {
		return fmt.Errorf("%w: 余分な引数 %v（update-all は sid を取らない＝PC 全体が対象）", errUsage, rest)
	}

	api := herdrapi.New("")
	if _, err := api.Ping(); err != nil {
		return fmt.Errorf("herdr server に繋がらない（socket=%s）: %w", api.SocketPath, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), updateTimeoutFor(""))
	defer cancel()
	res, _, err := runUpdateAll(ctx, api, restartOptions{Force: *force, Model: *model},
		func() (string, bool, error) { return selfupdate.Update(version) }, stdout)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "update-all: %s\n", summarizeUpdateAll(res))
	fmt.Fprintf(stdout, "常駐 agent への反映は再起動後: launchctl kickstart -k gui/$UID/%s\n", launchdLabel)
	return nil
}
