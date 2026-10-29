package autocontinue

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
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
	// PaneID は HERDR_PANE_ID（herdr pane 外なら空＝何もしない）。
	PaneID string
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
	if h.PaneID == "" {
		res.Skipped = "no-pane"
		return res, nil
	}
	cfg, err := h.Store.LoadConfig()
	if err != nil {
		return res, fmt.Errorf("config 読取: %w", err)
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
	lastPrompt, _ := LastHumanPrompt(h.Agent, in.TranscriptPath)
	grant := eff.Grant || GrantIn(lastPrompt)
	cap := eff.Cap
	if grant {
		cap = eff.GrantCap
	}
	res.Grant, res.Cap = grant, cap
	msg := ""
	if in.LastAssistantMessage != nil {
		msg = *in.LastAssistantMessage
	}
	base := fmt.Sprintf("%s pane=%s sid=%s active=%t count=%d/%d grant=%t", h.Agent, h.PaneID, in.SessionID, in.StopHookActive, cnt.Count, cap, grant)
	if cnt.Count >= cap {
		res.Count = cnt.Count
		res.Skipped = "cap"
		return stop(res, base+" 上限到達→停止（人の確認待ち）")
	}
	judge := h.NewJudge(eff.JudgeCmd, h.Store.Dir)
	jd := judge.Judge(ctx, msg, lastPrompt, grant)
	res.Judgment, res.Verdict, res.Source = jd, jd.Verdict, jd.Source
	detail := ""
	if jd.Detail != "" {
		detail = " (" + strings.ReplaceAll(tail(jd.Detail, 60), "\n", " ") + ")"
	}
	if jd.Verdict != VerdictContinue {
		res.Count = cnt.Count
		return stop(res, fmt.Sprintf("%s verdict=%s src=%s%s→停止", base, jd.Verdict, jd.Source, detail))
	}
	cnt.Count++
	cnt.At = now.Format(time.RFC3339)
	if err := h.Store.SaveCounter(h.Agent, in.SessionID, cnt); err != nil {
		return stop(res, fmt.Sprintf("%s counter 保存失敗→停止: %v", base, err))
	}
	phrase := eff.Phrase
	if grant && pane.Phrase == "" && ContinueLike(lastPrompt) {
		// 裁量ありで pane 側に文言の明示指定が無ければ、ユーザの言い回しをそのまま使う
		//（「いけるところまでつづけて」「最後まで自律的にすすめて」等）。
		phrase = strings.TrimSpace(lastPrompt)
	}
	reason := fmt.Sprintf("%s %d/%d] %s", ReasonPrefix, cnt.Count, cap, phrase)
	_ = h.Store.SetMark(h.PaneID, Mark{At: now, Continue: true, Reason: reason})
	res.Count = cnt.Count
	res.Output = &Output{Decision: "block", Reason: reason}
	res.LogLine = fmt.Sprintf("%s verdict=continue src=%s%s→続行 %q", base, jd.Source, detail, reason)
	_ = h.Store.AppendLog(now, res.LogLine)
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
	h := &Hook{Store: st, Agent: agent, PaneID: getenv("HERDR_PANE_ID"), NewJudge: NewJudge, Now: time.Now}
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
