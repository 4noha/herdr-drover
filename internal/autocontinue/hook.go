package autocontinue

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/4noha/herdr-drover/internal/herdrapi"
)

// Input は Stop フックの stdin JSON（Claude Code / Codex 共通部分。両者とも
// 公式仕様に `last_assistant_message` と `stop_hook_active` がある。2026-10-29
// 実測: Claude Code 2.1.312 で両フィールドを確認、Codex 0.165.0 のバイナリにも同名
// 文字列あり）。未知キーは無視。
type Input struct {
	SessionID            string  `json:"session_id"`
	TranscriptPath       string  `json:"transcript_path"`
	Cwd                  string  `json:"cwd"`
	HookEventName        string  `json:"hook_event_name"`
	StopHookActive       bool    `json:"stop_hook_active"`
	LastAssistantMessage *string `json:"last_assistant_message"`
	// LastUserMessage は transcript が無い経路（opencode プラグイン）が渡す直近の
	// 人間入力（自分の続行文は除いたもの）。transcript があればそちらを優先。
	LastUserMessage *string `json:"last_user_message,omitempty"`
}

// Output は stdout JSON（両エージェント共通: decision=block で続行、reason が
// 次のユーザ入力になる）。出力しない＝停止を許す。
type Output struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

// Hook は 1 回分のフック実行。
type Hook struct {
	Store *Store
	Agent string
	// PaneID は対象 pane（空＝何もしない）。PaneVia はその出どころ（env / cwd:…）＝ログ用。
	PaneID  string
	PaneVia string
	// NewJudge は Effective.JudgeCmd から判定器を作る seam（テストは fake）。
	NewJudge func(cmd []string, dir string) *Judge
	Now      func() time.Time
}

// Result はログ・テスト用の実行結果。
type Result struct {
	Output   *Output
	Verdict  Verdict
	Source   string
	Count    int
	Cap      int
	Grant    bool
	Skipped  string // 判定に入らなかった理由（空=判定した）
	LogLine  string
	Judgment Judgement
}

// Run はフック本体。エラーは「状態が読めない」等に限り、判定結果は Result。
// 鉄則: どんな失敗でもエージェントを止めない（呼び手は err をログに出して
// 出力無し＝停止許可にする）。
func (h *Hook) Run(ctx context.Context, in Input) (Result, error) {
	now := h.Now()
	res := Result{}
	cfg, err := h.Store.LoadConfig()
	if err != nil {
		return res, fmt.Errorf("config 読取: %w", err)
	}
	if cfg.Debug {
		n := 0
		if in.LastAssistantMessage != nil {
			n = len(*in.LastAssistantMessage)
		}
		_ = h.Store.AppendLog(now, fmt.Sprintf("debug: %s invoked pane=%q via=%s sid=%s active=%t event=%s msg_len=%d cwd=%s", h.Agent, h.PaneID, h.PaneVia, in.SessionID, in.StopHookActive, in.HookEventName, n, in.Cwd))
	}
	if h.PaneID == "" {
		res.Skipped = "no-pane"
		if h.PaneVia != "" && h.PaneVia != "env" {
			// cwd 解決に失敗した codex 等は silent にしない（env 無しの素の claude -p は
			// 対象外なので書かない）。
			_ = h.Store.AppendLog(now, fmt.Sprintf("%s pane 未解決（%s）cwd=%s sid=%s→何もしない", h.Agent, h.PaneVia, in.Cwd, in.SessionID))
		}
		return res, nil
	}
	pane, found, err := h.Store.LoadPane(h.PaneID)
	if err != nil {
		return res, fmt.Errorf("pane 設定読取: %w", err)
	}
	eff := Resolve(cfg, pane, found)
	if !eff.Enabled {
		res.Skipped = "disabled"
		return res, nil
	}
	// ここから先は「セミオートが処理中」＝通知を抑止（判定器が走る数秒の間に
	// daemon が idle を観測しても通知しない。停止側に倒れたら ClearMark）。
	_ = h.Store.SetMark(h.PaneID, Mark{At: now, Pending: true})
	stop := func(r Result, line string) (Result, error) {
		_ = h.Store.ClearMark(h.PaneID)
		r.LogLine = line
		_ = h.Store.AppendLog(now, line)
		return r, nil
	}

	cnt, err := h.Store.LoadCounter(h.Agent, in.SessionID)
	if err != nil {
		return stop(res, fmt.Sprintf("%s pane=%s sid=%s counter 読取失敗→停止: %v", h.Agent, h.PaneID, in.SessionID, err))
	}
	if !in.StopHookActive {
		// 人間の入力から始まったターンの最初の停止＝回数リセット。
		cnt.Count = 0
	}
	lastPrompt, ok := LastHumanPrompt(h.Agent, in.TranscriptPath)
	if !ok && in.LastUserMessage != nil && isHumanText(*in.LastUserMessage) {
		lastPrompt = *in.LastUserMessage
	}
	grant := eff.Grant || GrantIn(lastPrompt)
	continueAsked := ContinueLike(lastPrompt)
	// 人間の新しい入力で始まった停止（active=false）なら、同じセッションの直前の
	// 判定が「停止」だった場合に、その後の人の反応（最初の指示）を事例へ追記する
	//（『つづけて』が来ていれば、その停止は誤りだった示唆＝コーパス候補）。
	if !in.StopHookActive && lastPrompt != "" {
		if prev, found := h.Store.LastCaseOf(h.Agent, in.SessionID); found && prev.Decision != "continue" && prev.FollowupPrompt == "" {
			_ = h.Store.AppendFollowup(prev.ID, lastPrompt, now)
		}
	}
	rec := Case{ID: NewCaseID(h.Agent, in.SessionID, now), At: now.Format(time.RFC3339), Rules: RulesVersion, Agent: h.Agent,
		Pane: h.PaneID, SID: in.SessionID, Active: in.StopHookActive, Grant: grant, Asked: continueAsked, Prompt: lastPrompt}
	cap := eff.Cap
	if grant {
		cap = eff.GrantCap
	}
	res.Grant, res.Cap = grant, cap
	msg := ""
	if in.LastAssistantMessage != nil {
		msg = *in.LastAssistantMessage
	}
	rec.Count, rec.Cap = cnt.Count, cap
	base := fmt.Sprintf("%s pane=%s sid=%s active=%t count=%d/%d grant=%t rules=v%d", h.Agent, h.PaneID, in.SessionID, in.StopHookActive, cnt.Count, cap, grant, RulesVersion)
	if continueAsked {
		base += " asked=continue"
	}
	if h.PaneVia != "" && h.PaneVia != "env" {
		base += " via=" + h.PaneVia
	}
	if in.LastAssistantMessage != nil {
		rec.Message = *in.LastAssistantMessage
	}
	if cnt.Count >= cap {
		res.Count = cnt.Count
		res.Skipped = "cap"
		rec.Decision, rec.Skipped = "cap", "cap"
		_ = h.Store.AppendCase(rec)
		return stop(res, base+" 上限到達→停止（人の確認待ち）")
	}
	judge := h.NewJudge(eff.JudgeCmd, h.Store.Dir)
	if judge != nil && judge.Votes == 0 {
		judge.Votes = eff.JudgeVotes
	}
	jd := judge.Judge(ctx, msg, lastPrompt, grant, continueAsked)
	res.Judgment, res.Verdict, res.Source = jd, jd.Verdict, jd.Source
	detail := ""
	if jd.Detail != "" {
		detail = " (" + strings.ReplaceAll(tail(jd.Detail, 60), "\n", " ") + ")"
	}
	rec.Verdict, rec.Source, rec.Detail = string(jd.Verdict), jd.Source, jd.Detail
	if jd.Verdict != VerdictContinue {
		res.Count = cnt.Count
		rec.Decision = "stop"
		_ = h.Store.AppendCase(rec)
		return stop(res, fmt.Sprintf("%s verdict=%s src=%s%s→停止", base, jd.Verdict, jd.Source, detail))
	}
	cnt.Count++
	cnt.At = now.Format(time.RFC3339)
	if err := h.Store.SaveCounter(h.Agent, in.SessionID, cnt); err != nil {
		return stop(res, fmt.Sprintf("%s counter 保存失敗→停止: %v", base, err))
	}
	phrase := eff.Phrase
	if continueAsked && pane.Phrase == "" {
		// 直近の指示が続行系なら、pane 側に文言の明示指定が無い限りその言い回しを
		// そのまま使う（「改善を続けてください」「いけるところまでつづけて」等）。
		phrase = strings.TrimSpace(lastPrompt)
	}
	reason := fmt.Sprintf("%s %d/%d] %s", ReasonPrefix, cnt.Count, cap, phrase)
	_ = h.Store.SetMark(h.PaneID, Mark{At: now, Continue: true, Reason: reason})
	res.Count = cnt.Count
	res.Output = &Output{Decision: "block", Reason: reason}
	res.LogLine = fmt.Sprintf("%s verdict=continue src=%s%s→続行 %q", base, jd.Source, detail, reason)
	_ = h.Store.AppendLog(now, res.LogLine)
	rec.Decision, rec.Reason = "continue", reason
	_ = h.Store.AppendCase(rec)
	return res, nil
}

// RunStopHook は CLI 入口（`herdr-drover stop-hook <agent>`）。stdin→判定→stdout。
// 戻り値は exit code。**常に 0**（非 0 や stderr はエージェント側で「フック失敗」
// として扱われ得る＝エージェントの動作を邪魔しない）。失敗はログのみ。
func RunStopHook(agent string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	if getenv(JudgeEnv) == "1" {
		return 0 // 判定器サブプロセス内の再帰ガード
	}
	st, err := Open()
	if err != nil {
		fmt.Fprintf(stderr, "stop-hook: %v\n", err)
		return 0
	}
	b, err := io.ReadAll(io.LimitReader(stdin, 4<<20))
	if err != nil {
		fmt.Fprintf(stderr, "stop-hook: stdin 読取: %v\n", err)
		return 0
	}
	var in Input
	if len(strings.TrimSpace(string(b))) > 0 {
		if err := json.Unmarshal(b, &in); err != nil {
			_ = st.AppendLog(time.Now(), fmt.Sprintf("%s pane=%s stdin JSON 解釈失敗→停止: %v", agent, getenv("HERDR_PANE_ID"), err))
			return 0
		}
	}
	pane, via := getenv("HERDR_PANE_ID"), "env"
	if agent == "codex" || agent == "opencode" {
		// ⚠ Codex 0.165 はフックを**共有の app-server デーモン**内で実行する（実測
		// 2026-10-29: 別 pane の codex の Stop で HERDR_PANE_ID が最初に app-server を
		// 起動した pane のまま届いた）。opencode の server プラグインも共有 background
		// service 内。どちらも env は信用せず cwd で herdr の agent 一覧から引く。
		pane, via = ResolvePaneByCwd(agent, in.Cwd, herdrAgentList)
	}
	h := &Hook{Store: st, Agent: agent, PaneID: pane, PaneVia: via, NewJudge: NewJudge, Now: time.Now}
	res, err := h.Run(context.Background(), in)
	if err != nil {
		_ = st.AppendLog(time.Now(), fmt.Sprintf("%s pane=%s 実行失敗→停止: %v", agent, h.PaneID, err))
		return 0
	}
	if res.Output != nil {
		enc := json.NewEncoder(stdout)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(res.Output)
	}
	return 0
}

// osGetenv は RunStopHook の既定 getenv。
func OSGetenv(k string) string { return os.Getenv(k) }

// herdrAgentList は実 herdr の agent.list。socket は env HERDR_SOCKET_PATH > 既定の順で
// 試し、env の先に繋がらなければ既定パスへ**フォールバック**する（実測 2026-10-29:
// Codex の共有 app-server デーモンの env には隔離テスト用の /tmp socket が残っていた）。
func herdrAgentList() ([]herdrapi.AgentInfo, error) {
	agents, err := herdrapi.New("").AgentList()
	if err == nil {
		return agents, nil
	}
	def := herdrapi.DefaultSocketPath()
	if herdrapi.ResolveSocketPath("") == def {
		return nil, err
	}
	agents, err2 := herdrapi.New(def).AgentList()
	if err2 != nil {
		return nil, fmt.Errorf("%v; 既定 socket も失敗: %w", err, err2)
	}
	return agents, nil
}

// ResolvePaneByCwd は「種別 agent で cwd が一致する pane」を herdr の agent 一覧から
// **一意に**引く。↗窓（注入 pane・tokens に drover_inj_sid）は除外。0 件・複数件は
// 空を返し、via にその理由を入れる（推測で選ばない＝鉄則 3）。
func ResolvePaneByCwd(agent, cwd string, list func() ([]herdrapi.AgentInfo, error)) (pane, via string) {
	if cwd == "" {
		return "", "cwd:empty"
	}
	agents, err := list()
	if err != nil {
		return "", "cwd:agent.list失敗(" + err.Error() + ")"
	}
	var hits []string
	for _, a := range agents {
		if a.Agent != agent || a.PaneID == "" {
			continue
		}
		if a.Tokens["drover_inj_sid"] != "" {
			continue
		}
		if a.ForegroundCwd == cwd || (a.ForegroundCwd == "" && a.Cwd == cwd) {
			hits = append(hits, a.PaneID)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], "cwd"
	case 0:
		return "", "cwd:該当なし"
	}
	return "", fmt.Sprintf("cwd:曖昧(%d件 %s)", len(hits), strings.Join(hits, ","))
}
