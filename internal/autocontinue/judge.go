package autocontinue

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Verdict は判定結果。行動は 2 値（continue=続行／それ以外=停止）だが、
// ログで「完了」と「人待ち」を区別できるよう 3 値で持つ。
type Verdict string

const (
	VerdictContinue Verdict = "continue"
	VerdictHuman    Verdict = "human"
	VerdictDone     Verdict = "done"
)

// Judgement は判定とその根拠（ログ用）。
type Judgement struct {
	Verdict Verdict
	Source  string // rule:<name> / llm / fallback:<理由>
	Detail  string
}

// JudgeTimeout は判定器 1 回の上限（Claude Code の Stop フック既定 60s の内側）。
const JudgeTimeout = 40 * time.Second

// DefaultJudgeCmd は既定の判定器（小モデル）。最後の引数にプロンプトが付く。
// `--max-turns 1`＝ツールを使わせない・`--output-format text`＝1 語だけ読む。
func DefaultJudgeCmd() []string {
	return []string{"claude", "-p", "--model", "haiku", "--max-turns", "1", "--output-format", "text"}
}

var (
	// 人待ちの判定は**最後の一文**（疑問文）だけで行う。実測 2026-10-29: 直前 400 文字
	// 全体で見ると、本文の技術用語（VM の "secret" 等）に反応して「続けますか？」を
	// 人待ちと誤判定した（gts セッション）。
	//
	// reAskContinue: 「続けますか／進めてよいですか」型＝エージェント自身が続行の許可を
	// 求めている。セミオートの趣旨そのもの＝続行（ただし同じ文に不可逆操作があれば人待ち）。
	reAskContinue = regexp.MustCompile(`(続け|つづけ|進め|すすめ|着手|始め|やり|実行|実装)(ます|て(も)?(いい|よい|よろしい|大丈夫)|ましょう|てよろしい)?(です)?か|よろしいでしょうか|よろしいですか|どうしますか|続行しますか|進行しますか|次に進みますか|(続け|進め)ても|shall I (continue|proceed)|should I (continue|proceed)|continue\?`)
	// reCredAsk: 認証情報・秘密の提示を**人に求める**文（名詞＋依頼動詞の両方）。
	reCredAsk = regexp.MustCompile(`(?i)(パスワード|パスフレーズ|認証情報|credential|password|secret|api ?key|トークン|token|OTP|認証コード|2FA|二段階|ログイン|サインイン|アカウント).*(教えて|ください|下さい|入力|貼|渡し|提供|もらえ|いただけ|頂け|お願い)`)
	// reIrreversibleAsk: 不可逆・対外操作の**承認を求める**文。
	reIrreversibleAsk = regexp.MustCompile(`(?i)(本番|デプロイ|公開|push|リリース|削除|消し|force|上書き|送信|送って|投稿|支払|購入|課金|決済|カード|権限|許可|承認).*(してよ|していい|してもいい|しますか|してよろしい|して大丈夫|しても大丈夫|よろしい|していただ|してもらえ|可能ですか|should I|shall I|ok to|okay to)`)
	// rePhysicalAsk: 実機・物理操作を**人に求める**文。
	rePhysicalAsk = regexp.MustCompile(`(実機|物理|電源|ケーブル|差し込|差し直|接続して|押して|置いて|つないで|繋いで).*(ください|下さい|もらえ|いただけ|頂け|してから|お願い)`)
	// reUnfinished: 未完を明示する語。疑問文で終わらなければ続行。
	reUnfinished = regexp.MustCompile(`(未完|未対応|未実装|未着手|残り|残る|残課題|次は|次に|続けます|続行します|進めます|引き続き|次のステップ|途中です|まだ.*(ません|ない))`)
	// reVerdict: 判定器出力から 1 語を拾う。
	reVerdict = regexp.MustCompile(`(?i)\b(CONTINUE|HUMAN|DONE)\b`)
)

// endsWithQuestion は末尾（装飾・空白を除く）が疑問符か。
func endsWithQuestion(s string) bool {
	t := strings.TrimRight(strings.TrimSpace(s), " \t\r\n*_`)）」』]")
	return strings.HasSuffix(t, "?") || strings.HasSuffix(t, "？")
}

// lastSentence は末尾の一文（最後の文末記号より後ろ。改行も区切り）。人待ちの
// 判定はこれだけで行う＝本文中の技術用語に引きずられない。
func lastSentence(s string) string {
	t := strings.TrimRight(strings.TrimSpace(s), " \t\r\n*_`)）」』]")
	t = strings.TrimRight(t, "?？")
	i := strings.LastIndexAny(t, "。！!？?\n")
	if i >= 0 {
		t = t[i+1:]
	}
	return strings.TrimSpace(t)
}

// tail は末尾 n 文字（rune）。
func tail(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

// Judge は判定器。Run はテスト用 seam（既定は exec）。
type Judge struct {
	Cmd     []string
	Timeout time.Duration
	Run     func(ctx context.Context, argv []string, dir string) (string, error)
	// Dir は判定器サブプロセスの cwd（claude -p の transcript がここに紐づく＝
	// 実プロジェクトの transcript 一覧を汚さない）。
	Dir string
}

// NewJudge は既定の exec 実装。
func NewJudge(cmd []string, dir string) *Judge {
	return &Judge{Cmd: cmd, Timeout: JudgeTimeout, Run: runExec, Dir: dir}
}

// runExec は argv を実行して stdout を返す。
//
// ⚠ 環境変数: HERDR_* と CLAUDECODE 系を**必ず外す**。外さないと
//   - herdr の SessionStart hook が判定器の claude を pane の agent_session として
//     報告し、本物のセッション ref を上書きする（resume が壊れる）
//   - Claude Code の「入れ子起動」ガードに当たる
//   - 判定器自身の Stop フックが再帰的に判定器を呼ぶ
//
// ついでに HERDR_DROVER_AUTO_JUDGE=1 を立て、フック側の再帰ガードにする。
func runExec(ctx context.Context, argv []string, dir string) (string, error) {
	if len(argv) == 0 {
		return "", fmt.Errorf("judge_cmd が空")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	env := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		k := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			k = kv[:i]
		}
		if strings.HasPrefix(k, "HERDR_") || strings.HasPrefix(k, "CLAUDECODE") || strings.HasPrefix(k, "CLAUDE_CODE_") {
			continue
		}
		env = append(env, kv)
	}
	cmd.Env = append(env, JudgeEnv+"=1")
	cmd.Stdin = nil
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if asExit(err, &ee) {
			return string(out), fmt.Errorf("%w: %s", err, strings.TrimSpace(string(ee.Stderr)))
		}
		return string(out), err
	}
	return string(out), nil
}

// JudgeEnv は判定器サブプロセスに立てる再帰ガード env。
const JudgeEnv = "HERDR_DROVER_AUTO_JUDGE"

func asExit(err error, target **exec.ExitError) bool {
	ee, ok := err.(*exec.ExitError)
	if ok {
		*target = ee
	}
	return ok
}

// Judge は「続行してよいか」を判定する。msg はエージェントの最終発話、
// lastPrompt は直近のユーザ指示（裁量判定・判定器の文脈用。無ければ空）。
//
// continueAsked は「直近の人間の指示そのものが『つづけて』系」だったこと＝選択肢や
// 計画の提示で迷うケースを続行側へ寄せる（上限は変えない）。
func (j *Judge) Judge(ctx context.Context, msg, lastPrompt string, grant, continueAsked bool) Judgement {
	if strings.TrimSpace(msg) == "" {
		// 発話が無い停止（エラー終了等）は判定材料が無い＝止める。
		return Judgement{Verdict: VerdictHuman, Source: "rule:empty"}
	}
	t := tail(msg, 400)
	q := endsWithQuestion(t)
	if q {
		ls := lastSentence(t)
		switch {
		case reCredAsk.MatchString(ls):
			return Judgement{Verdict: VerdictHuman, Source: "rule:asks-credential", Detail: ls}
		case reIrreversibleAsk.MatchString(ls):
			return Judgement{Verdict: VerdictHuman, Source: "rule:asks-irreversible", Detail: ls}
		case rePhysicalAsk.MatchString(ls):
			return Judgement{Verdict: VerdictHuman, Source: "rule:asks-physical", Detail: ls}
		case reAskContinue.MatchString(ls):
			// 「続けますか？」はセミオートの趣旨そのもの＝続行。
			return Judgement{Verdict: VerdictContinue, Source: "rule:asks-to-continue", Detail: ls}
		}
	}
	if !q && reUnfinished.MatchString(t) {
		return Judgement{Verdict: VerdictContinue, Source: "rule:unfinished", Detail: reUnfinished.FindString(t)}
	}
	if j == nil || j.Run == nil {
		return Judgement{Verdict: VerdictHuman, Source: "fallback:no-judge"}
	}
	timeout := j.Timeout
	if timeout <= 0 {
		timeout = JudgeTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	argv := append(append([]string{}, j.Cmd...), buildPrompt(msg, lastPrompt, grant, continueAsked))
	out, err := j.Run(ctx, argv, j.Dir)
	if err != nil {
		return Judgement{Verdict: VerdictHuman, Source: "fallback:judge-error", Detail: err.Error()}
	}
	m := reVerdict.FindString(out)
	switch strings.ToUpper(m) {
	case "CONTINUE":
		return Judgement{Verdict: VerdictContinue, Source: "llm"}
	case "DONE":
		return Judgement{Verdict: VerdictDone, Source: "llm"}
	case "HUMAN":
		return Judgement{Verdict: VerdictHuman, Source: "llm"}
	}
	return Judgement{Verdict: VerdictHuman, Source: "fallback:unparsed", Detail: tail(strings.TrimSpace(out), 80)}
}

// buildPrompt は判定器へのプロンプト。出力は 1 語に固定する。
func buildPrompt(msg, lastPrompt string, grant, continueAsked bool) string {
	var b strings.Builder
	b.WriteString("You are a gate deciding whether a coding agent that just ended its turn should be told to keep working.\n")
	b.WriteString("Answer with exactly one word: CONTINUE, HUMAN, or DONE.\n\n")
	b.WriteString("CONTINUE = the agent stopped although work remains, or it is offering choices / a plan / a recommendation that it can decide itself (pick its own recommendation and proceed).\n")
	b.WriteString("HUMAN = finishing needs something only the human can give: credentials or login, a physical action on a device, approval for an irreversible or external operation (deploy, publish, payment, deleting data, pushing to a shared repo), or genuinely missing requirements the agent cannot infer.\n")
	b.WriteString("DONE = the requested task is fully complete and nothing is left to do.\n\n")
	if grant {
		b.WriteString("The human explicitly granted autonomy (\"go as far as you can\"). Treat ordinary choices and plan approvals as CONTINUE.\n\n")
	} else if continueAsked {
		b.WriteString("The human's latest instruction was itself just \"keep going\" (つづけて). They want the agent to proceed on its own judgement: treat ordinary choices and plan approvals as CONTINUE; answer HUMAN only for the strict HUMAN conditions above.\n\n")
	}
	if lp := strings.TrimSpace(lastPrompt); lp != "" {
		b.WriteString("Latest human instruction:\n<<<\n")
		b.WriteString(tail(lp, 800))
		b.WriteString("\n>>>\n\n")
	}
	b.WriteString("Agent's final message of the turn:\n<<<\n")
	b.WriteString(tail(msg, 3000))
	b.WriteString("\n>>>\n\nOne word:")
	return b.String()
}
