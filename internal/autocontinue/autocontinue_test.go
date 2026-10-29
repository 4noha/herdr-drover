package autocontinue

// 判定ルール・transcript 読取・フック設置・フック本体（回数・上限・裁量・
// 抑止マーク）を実ファイルで検証する。transcript の行は 2026-10-29 に実 Claude Code
// 2.1.312 / Codex 0.165.0 が書いたものと同じ形（Stop フック実測の行を含む）。
// 判定器（claude -p）は exec しない＝Run seam に fake を挿す。実 Claude Code で
// Stop フック契約を通す live テストは cmd 側（TestLiveClaudeStopHook・opt-in）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/4noha/herdr-drover/internal/herdrapi"
)

func fakeJudge(reply string, err error) *Judge {
	return &Judge{Cmd: []string{"fake"}, Timeout: time.Second, Run: func(context.Context, []string, string) (string, error) {
		return reply, err
	}}
}

func TestJudgeRules(t *testing.T) {
	cases := []struct {
		name    string
		msg     string
		reply   string
		err     error
		want    Verdict
		wantSrc string
	}{
		{"空発話は停止", "   ", "CONTINUE", nil, VerdictHuman, "rule:empty"},
		{"未完＋非疑問は続行（判定器を呼ばない）", "744 テスト成功。まだ未完です。残る課題は支持と下降です。次は検証です。", "HUMAN", nil, VerdictContinue, "rule:unfinished"},
		{"人にしかできない確認の疑問文は停止", "実装は整いました。**Cloud Run へデプロイしてよいですか？**", "CONTINUE", nil, VerdictHuman, "rule:asks-irreversible"},
		{"パスワードを聞く疑問文は停止", "SSH 先の認証情報（パスワード）を教えてもらえますか？", "CONTINUE", nil, VerdictHuman, "rule:asks-credential"},
		{"実機操作を頼む疑問文は停止", "書き込み準備ができました。本体の電源を切って USB ケーブルを差し直してから教えてもらえますか？", "CONTINUE", nil, VerdictHuman, "rule:asks-physical"},
		{"本文に secret があっても最後の一文が『続けますか？』なら続行（gts 誤検知の再現）", "VM の secret key 導出を確認しました。\n次の手順\n1. トレースを整理\n2. 鍵更新規則を確定\nいま「VM の実体」を掴んだので、ここからはオフライン解析で進められます。続けますか？", "HUMAN", nil, VerdictContinue, "rule:asks-to-continue"},
		{"進めてよいですか？ も続行", "設計をまとめました。この方針で実装を進めてよいですか？", "HUMAN", nil, VerdictContinue, "rule:asks-to-continue"},
		{"push してよいですか？ は不可逆＝停止", "コミットしました。origin に push してよいですか？", "CONTINUE", nil, VerdictHuman, "rule:asks-irreversible"},
		{"区切りの宣言でも再開に電源再投入が要るなら停止（ユーザー判断 v7）", "状態を整理しました。\n次回は「電源再投入 → 状態確認」から再開します。", "CONTINUE", nil, VerdictHuman, "rule:deferral-needs-physical"},
		{"区切りの宣言（物理操作なし）は続行", "状態を整理しました。\n次回は静的解析の続きから再開します。", "HUMAN", nil, VerdictContinue, "rule:wrapup-deferral"},
		{"ボタン操作の手順を頼む報告は待機中の説明が付いても停止（ユーザー判断 v9）", "セッションは待機中です。受付が開けば自動で進みます。\n中央○ 2秒 → 5秒後に ＋ ＋ − ＋ − − ＋ をお願いします。\n押せない場合は、このままタイムアウトしても影響ありません。", "CONTINUE", nil, VerdictHuman, "rule:asks-physical"},
		{"長押しの依頼", "準備ができました。電源ボタンを 5 秒長押ししてください。", "CONTINUE", nil, VerdictHuman, "rule:asks-physical"},
		{"技術用語の token を含む普通の問いは判定器へ", "token の有効期限は 1h でした。どの環境で検証しますか？", "CONTINUE", nil, VerdictContinue, "llm:3/3"},
		{"普通の選択肢提示は判定器へ→CONTINUE", "方針は 1. A 2. B 3. C です。おすすめは 2。どれにしますか？", "CONTINUE", nil, VerdictContinue, "llm:3/3"},
		{"判定器 DONE", "すべて完了しました。", "DONE\n", nil, VerdictDone, "llm:3/3"},
		{"判定器 HUMAN", "どの端末に接続しますか。", "The answer is HUMAN.", nil, VerdictHuman, "llm:3/3"},
		{"説明付き回答は最後の語を採る", "完了しました。", "Not CONTINUE because nothing remains. Final: DONE", nil, VerdictDone, "llm:3/3"},
		{"実機操作の依頼は疑問文でなくても停止（PioneerNP1 誤続行の再現）", "✅ dump 完了（BatchLogSave result=true）。POSEIDONDBG OFF は送信したが応答未確認です（後で確認）。\nSD を回収します：\n1. 電源を切る\n2. SD を抜いて Mac に挿す\n3. 「挿した」と教えてください", "CONTINUE", nil, VerdictHuman, "rule:asks-physical"},
		{"実機操作の依頼＋別案の問いも停止", "SD が Mac にマウントされていません（カードリーダーも検出されず）。SD を Mac に挿してください。\n（NP1 の電源を切る → SD を抜く → Mac に挿す → 「挿した」）\nもし今すぐ難しければ、その間に別の静的解析を進めます。どうしますか？", "CONTINUE", nil, VerdictHuman, "rule:asks-physical"},
		{"判定器エラーは停止側", "完了しました。", "", errors.New("boom"), VerdictHuman, "fallback:judge-error"},
		{"判定器出力が読めないのは停止側", "完了しました。", "maybe", nil, VerdictHuman, "fallback:unparsed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := fakeJudge(c.reply, c.err).Judge(context.Background(), c.msg, "", false, false)
			if got.Verdict != c.want || got.Source != c.wantSrc {
				t.Fatalf("got %+v want verdict=%s src=%s", got, c.want, c.wantSrc)
			}
		})
	}
	// 判定器が無い（Run=nil）構成は停止側。
	j := &Judge{}
	if got := j.Judge(context.Background(), "完了しました。", "", false, false); got.Verdict != VerdictHuman || got.Source != "fallback:no-judge" {
		t.Fatalf("no-judge: %+v", got)
	}
}

func TestBuildPromptCarriesContext(t *testing.T) {
	p := buildPrompt("最終発話", "いけるところまでつづけて", true, true)
	for _, want := range []string{"CONTINUE, HUMAN, or DONE", "granted autonomy", "いけるところまでつづけて", "最終発話"} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt に %q が無い:\n%s", want, p)
		}
	}
	if strings.Contains(buildPrompt("x", "", false, false), "granted autonomy") {
		t.Fatal("裁量なしで granted の文言が出ている")
	}
	// 指示そのものが「つづけて」: 裁量は無いが続行側に寄せる文言が入る。
	p = buildPrompt("x", "改善を続けてください", false, true)
	if !strings.Contains(p, "keep going") || strings.Contains(p, "granted autonomy") {
		t.Fatalf("asked=continue の文言:\n%s", p)
	}
}

func TestGrantAndContinueLike(t *testing.T) {
	for _, s := range []string{"いけるところまでつづけて", "最後まで自律的にすすめて", "4を完成まで進めて", "できることをつづけて", "聞かずに全部やって"} {
		if !GrantIn(s) {
			t.Errorf("GrantIn(%q)=false", s)
		}
	}
	for _, s := range []string{"つづけて", "改善を続けてください", "すすめて"} {
		if GrantIn(s) {
			t.Errorf("GrantIn(%q)=true（裁量ではない）", s)
		}
		if !ContinueLike(s) {
			t.Errorf("ContinueLike(%q)=false", s)
		}
	}
	long := strings.Repeat("あ", 41) + "つづけて"
	if ContinueLike(long) {
		t.Error("41 字超は ContinueLike=false のはず")
	}
}

// 実 transcript の行形（2026-10-29 実測）。
const claudeTranscript = `{"type":"user","message":{"role":"user","content":"単語 PART1 だけを答えて止まってください。"},"uuid":"u1"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"PART1"}],"stop_reason":"end_turn"}}
{"type":"user","isMeta":true,"message":{"role":"user","content":"Stop hook feedback:\n[semi-auto 1/3] つづけて"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{}}],"stop_reason":"tool_use"}}
{"type":"user","message":{"role":"user","content":[{"tool_use_id":"t1","type":"tool_result","content":"ok"}]}}
{"type":"user","message":{"role":"user","content":"<system-reminder>注入</system-reminder>"}}
{"type":"permission-mode","permissionMode":"default"}
`

const codexTranscript = `{"type":"session_meta","payload":{"id":"x"}}
{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>…</environment_context>"}]}}
{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"最後まで自律的にすすめて"}]}}
{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"進めます"}]}}
{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":"進めます"}}
{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"[semi-auto 1/10] 最後まで自律的にすすめて"}]}}
`

func TestLastHumanPrompt(t *testing.T) {
	dir := t.TempDir()
	cp := filepath.Join(dir, "c.jsonl")
	if err := os.WriteFile(cp, []byte(claudeTranscript), 0o600); err != nil {
		t.Fatal(err)
	}
	got, ok := LastHumanPrompt("claude", cp)
	if !ok || got != "単語 PART1 だけを答えて止まってください。" {
		t.Fatalf("claude: got %q ok=%t（Stop hook feedback・tool_result・<注入 を除いた最後の人間入力のはず）", got, ok)
	}
	xp := filepath.Join(dir, "x.jsonl")
	if err := os.WriteFile(xp, []byte(codexTranscript), 0o600); err != nil {
		t.Fatal(err)
	}
	got, ok = LastHumanPrompt("codex", xp)
	if !ok || got != "最後まで自律的にすすめて" {
		t.Fatalf("codex: got %q ok=%t", got, ok)
	}
	if _, ok := LastHumanPrompt("claude", filepath.Join(dir, "missing.jsonl")); ok {
		t.Fatal("無いファイルで ok=true")
	}
	if _, ok := LastHumanPrompt("opencode", cp); ok {
		t.Fatal("未対応 agent で ok=true")
	}
	// 末尾 2MB だけ読む経路: 先頭に巨大なゴミ行を置いても最後の入力が取れる。
	big := filepath.Join(dir, "big.jsonl")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	pad := strings.Repeat("x", 1<<20)
	for i := 0; i < 3; i++ {
		f.WriteString(`{"type":"user","message":{"role":"user","content":"` + pad + `"}}` + "\n")
	}
	f.WriteString(claudeTranscript)
	f.Close()
	got, ok = LastHumanPrompt("claude", big)
	if !ok || got != "単語 PART1 だけを答えて止まってください。" {
		t.Fatalf("big: got %.40q ok=%t", got, ok)
	}
}

// ユーザー実機の settings.json と同じ形（SessionStart＋PreToolUse を持つ）に
// 追記しても他が温存される・冪等・bin 変更で差し替え・除去で元に戻る。
func TestInstallUninstallPreservesOtherHooks(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "settings.json")
	orig := `{
  "permissions": {"allow": ["Bash(ls:*)"]},
  "hooks": {
    "SessionStart": [{"matcher": "^(startup|resume)$", "hooks": [{"type": "command", "command": "bash '/x/herdr-agent-state.sh' session"}]}],
    "PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "jq ."}]}]
  },
  "model": "opus"
}`
	if err := os.WriteFile(p, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := Install(p, "claude", "/Users/x/.herdr-drover/bin/herdr-drover")
	if err != nil || !changed {
		t.Fatalf("install: changed=%t err=%v", changed, err)
	}
	var doc map[string]any
	b, _ := os.ReadFile(p)
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["model"] != "opus" || doc["permissions"] == nil {
		t.Fatalf("他キーが消えた: %s", b)
	}
	hooks := doc["hooks"].(map[string]any)
	if len(hooks["SessionStart"].([]any)) != 1 || len(hooks["PreToolUse"].([]any)) != 1 {
		t.Fatalf("既存フックが変わった: %s", b)
	}
	stop := hooks["Stop"].([]any)
	if len(stop) != 1 {
		t.Fatalf("Stop が 1 group のはず: %s", b)
	}
	h := stop[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)
	if h["command"] != "'/Users/x/.herdr-drover/bin/herdr-drover' stop-hook claude" || h["type"] != "command" || h["timeout"].(float64) != HookTimeoutSec {
		t.Fatalf("handler: %v", h)
	}
	if c, ok, _ := Installed(p, "claude"); !ok || c != h["command"] {
		t.Fatalf("Installed: %q %t", c, ok)
	}
	// 冪等。
	if changed, err := Install(p, "claude", "/Users/x/.herdr-drover/bin/herdr-drover"); err != nil || changed {
		t.Fatalf("2 回目 install: changed=%t err=%v", changed, err)
	}
	// bin 変更＝差し替え（group は増えない）。
	if changed, err := Install(p, "claude", "/opt/herdr-drover"); err != nil || !changed {
		t.Fatalf("bin 変更 install: changed=%t err=%v", changed, err)
	}
	b, _ = os.ReadFile(p)
	if strings.Count(string(b), "stop-hook claude") != 1 || !strings.Contains(string(b), "'/opt/herdr-drover' stop-hook claude") {
		t.Fatalf("差し替えになっていない: %s", b)
	}
	// 除去＝Stop キーごと消え、他は残る。
	if changed, err := Uninstall(p, "claude"); err != nil || !changed {
		t.Fatalf("uninstall: changed=%t err=%v", changed, err)
	}
	b, _ = os.ReadFile(p)
	json.Unmarshal(b, &doc)
	hooks = doc["hooks"].(map[string]any)
	if _, ok := hooks["Stop"]; ok {
		t.Fatalf("Stop が残っている: %s", b)
	}
	if len(hooks["SessionStart"].([]any)) != 1 || doc["model"] != "opus" {
		t.Fatalf("除去で他が壊れた: %s", b)
	}
	if changed, _ := Uninstall(p, "claude"); changed {
		t.Fatal("未設置の uninstall で changed=true")
	}
	// 設定ファイルが無い（codex 初回）→ 作られる。
	cp := filepath.Join(dir, "sub", "hooks.json")
	if changed, err := Install(cp, "codex", "/opt/herdr-drover"); err != nil || !changed {
		t.Fatalf("codex 新規: changed=%t err=%v", changed, err)
	}
	if _, ok, _ := Installed(cp, "codex"); !ok {
		t.Fatal("codex 新規設置が Installed=false")
	}
	if _, ok, _ := Installed(cp, "claude"); ok {
		t.Fatal("codex ファイルに claude 用が在るはずがない")
	}
}

func strp(s string) *string { return &s }
func boolp(b bool) *bool    { return &b }

func newTestHook(t *testing.T, judge *Judge) (*Hook, *Store) {
	t.Helper()
	st := &Store{Dir: t.TempDir()}
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	h := &Hook{Store: st, Agent: "claude", PaneID: "w1:p4F", Now: func() time.Time { return now },
		NewJudge: func([]string, string) *Judge { return judge }}
	return h, st
}

func TestHookFlowCountsCapsAndMarks(t *testing.T) {
	h, st := newTestHook(t, fakeJudge("CONTINUE", nil))
	if err := st.SaveConfig(Config{Enabled: boolp(true)}); err != nil {
		t.Fatal(err)
	}
	in := Input{SessionID: "s1", HookEventName: "Stop", LastAssistantMessage: strp("途中経過です。")}
	ctx := context.Background()
	// 1〜10 回目: 続行（reason に n/cap と文言・mark=continue・counter 増加）。
	for i := 1; i <= 10; i++ {
		in.StopHookActive = i > 1
		res, err := h.Run(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		want := fmt.Sprintf("[semi-auto %d/10] つづけて", i)
		if res.Output == nil || res.Output.Decision != "block" || res.Output.Reason != want {
			t.Fatalf("%d 回目: output=%+v want reason %q", i, res.Output, want)
		}
		if ok, m := st.Suppressed("w1:p4F", h.Now()); !ok || !m.Continue {
			t.Fatalf("%d 回目: 抑止マークが continue でない: %+v", i, m)
		}
		if c, _ := st.LoadCounter("claude", "s1"); c.Count != i {
			t.Fatalf("%d 回目: counter=%d", i, c.Count)
		}
	}
	// 11 回目: 上限＝停止・マーク消滅（通知は通常どおり飛ぶ）。
	in.StopHookActive = true
	res, err := h.Run(ctx, in)
	if err != nil || res.Output != nil || res.Skipped != "cap" {
		t.Fatalf("上限: res=%+v err=%v", res, err)
	}
	if ok, _ := st.Suppressed("w1:p4F", h.Now()); ok {
		t.Fatal("上限停止後にマークが残っている")
	}
	// 人間の新しい入力（active=false）で回数リセット。
	in.StopHookActive = false
	res, _ = h.Run(ctx, in)
	if res.Output == nil || res.Output.Reason != "[semi-auto 1/10] つづけて" {
		t.Fatalf("リセット後: %+v", res.Output)
	}
	// 判定 HUMAN → 出力なし・マーク消滅・counter 据え置き。
	h.NewJudge = func([]string, string) *Judge { return fakeJudge("HUMAN", nil) }
	in.StopHookActive = true
	res, _ = h.Run(ctx, in)
	if res.Output != nil || res.Verdict != VerdictHuman {
		t.Fatalf("HUMAN: %+v", res)
	}
	if ok, _ := st.Suppressed("w1:p4F", h.Now()); ok {
		t.Fatal("HUMAN 停止後にマークが残っている")
	}
	if c, _ := st.LoadCounter("claude", "s1"); c.Count != 1 {
		t.Fatalf("HUMAN で counter が動いた: %d", c.Count)
	}
	// ログは 1 実行 1 行（6 行）。
	b, _ := os.ReadFile(st.LogPath())
	if n := strings.Count(string(b), "\n"); n != 13 {
		t.Fatalf("log 行数=%d want 13:\n%s", n, b)
	}
	if !strings.Contains(string(b), "verdict=human src=llm:3/3→停止") || !strings.Contains(string(b), "上限到達→停止") {
		t.Fatalf("log 内容:\n%s", b)
	}
}

func TestHookSkipsWhenDisabledOrNoPane(t *testing.T) {
	h, st := newTestHook(t, fakeJudge("CONTINUE", nil))
	in := Input{SessionID: "s1", LastAssistantMessage: strp("x")}
	res, err := h.Run(context.Background(), in)
	if err != nil || res.Output != nil || res.Skipped != "disabled" {
		t.Fatalf("既定 OFF: %+v %v", res, err)
	}
	if _, err := os.Stat(st.LogPath()); err == nil {
		t.Fatal("OFF なのにログが書かれた")
	}
	// pane 単位 on（全体 off のまま）→ 続行。
	st.SavePane("w1:p4F", PaneSetting{Enabled: boolp(true), Phrase: "改善を続けてください"})
	res, _ = h.Run(context.Background(), in)
	if res.Output == nil || res.Output.Reason != "[semi-auto 1/10] 改善を続けてください" {
		t.Fatalf("pane on: %+v", res.Output)
	}
	// pane 単位 off（全体 on）→ skip。
	st.SaveConfig(Config{Enabled: boolp(true)})
	st.SavePane("w1:p4F", PaneSetting{Enabled: boolp(false)})
	res, _ = h.Run(context.Background(), in)
	if res.Output != nil || res.Skipped != "disabled" {
		t.Fatalf("pane off: %+v", res)
	}
	// pane 外（HERDR_PANE_ID 無し）→ 何もしない。
	h.PaneID = ""
	res, _ = h.Run(context.Background(), in)
	if res.Output != nil || res.Skipped != "no-pane" {
		t.Fatalf("no pane: %+v", res)
	}
}

func TestHookGrantFromTranscript(t *testing.T) {
	h, st := newTestHook(t, fakeJudge("CONTINUE", nil))
	st.SaveConfig(Config{Enabled: boolp(true)})
	tp := filepath.Join(t.TempDir(), "x.jsonl")
	os.WriteFile(tp, []byte(codexTranscript), 0o600)
	h.Agent = "codex"
	in := Input{SessionID: "r1", TranscriptPath: tp, LastAssistantMessage: strp("進めます")}
	res, err := h.Run(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	// 裁量あり＝上限 10・文言はユーザの言い回しをそのまま。
	if res.Output == nil || res.Output.Reason != "[semi-auto 1/20] 最後まで自律的にすすめて" || !res.Grant || res.Cap != 20 {
		t.Fatalf("grant: %+v out=%+v", res, res.Output)
	}
	// pane 設定があっても phrase 未指定なら、ユーザの言い回しを使う。
	st.SavePane("w1:p4F", PaneSetting{Enabled: boolp(true)})
	in.StopHookActive = true
	res, _ = h.Run(context.Background(), in)
	if res.Output == nil || res.Output.Reason != "[semi-auto 2/20] 最後まで自律的にすすめて" {
		t.Fatalf("pane without phrase: %+v", res.Output)
	}
	// pane 側に文言の明示指定があればそちらが優先。
	st.SavePane("w1:p4F", PaneSetting{Phrase: "つづけて"})
	res, _ = h.Run(context.Background(), in)
	if res.Output == nil || res.Output.Reason != "[semi-auto 3/20] つづけて" {
		t.Fatalf("pane phrase: %+v", res.Output)
	}
}

func TestSuppressedTTL(t *testing.T) {
	st := &Store{Dir: t.TempDir()}
	now := time.Now()
	if ok, _ := st.Suppressed("w1:p1", now); ok {
		t.Fatal("マーク無しで抑止")
	}
	st.SetMark("w1:p1", Mark{At: now.Add(-10 * time.Second), Pending: true})
	if ok, _ := st.Suppressed("w1:p1", now); !ok {
		t.Fatal("pending マークで抑止されない")
	}
	st.SetMark("w1:p1", Mark{At: now.Add(-MarkTTL - time.Second), Continue: true})
	if ok, _ := st.Suppressed("w1:p1", now); ok {
		t.Fatal("TTL 超過マークで抑止")
	}
	st.SetMark("w1:p1", Mark{At: now, Continue: true})
	st.ClearMark("w1:p1")
	if ok, _ := st.Suppressed("w1:p1", now); ok {
		t.Fatal("ClearMark 後に抑止")
	}
	// SuppressFunc は dir 空で常に false・pane id の ':' を安全化して同じファイルを見る。
	if SuppressFunc("", time.Now)("w1:p1") {
		t.Fatal("dir 空で true")
	}
	st.SetMark("w1:p1", Mark{At: now, Continue: true})
	if !SuppressFunc(st.Dir, func() time.Time { return now })("w1:p1") {
		t.Fatal("SuppressFunc が実ファイルを見ていない")
	}
}

func TestRunStopHookIO(t *testing.T) {
	dir := t.TempDir()
	st := &Store{Dir: dir}
	st.SaveConfig(Config{Enabled: boolp(true), JudgeCmd: []string{"sh", "-c", "echo CONTINUE"}})
	env := map[string]string{DirEnv: dir, "HERDR_PANE_ID": "w2:p3"}
	getenv := func(k string) string { return env[k] }
	t.Setenv(DirEnv, dir) // Open() は os.Getenv を見る
	var out, errb strings.Builder
	code := RunStopHook("claude", strings.NewReader(`{"session_id":"abc","stop_hook_active":false,"last_assistant_message":"途中です"}`), &out, &errb, getenv)
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	var o Output
	if err := json.Unmarshal([]byte(out.String()), &o); err != nil || o.Decision != "block" || o.Reason != "[semi-auto 1/10] つづけて" {
		t.Fatalf("stdout=%q err=%v", out.String(), err)
	}
	// 判定器サブプロセス内（再帰ガード）は即 0・無出力。
	out.Reset()
	env[JudgeEnv] = "1"
	if code := RunStopHook("claude", strings.NewReader(`{}`), &out, &errb, getenv); code != 0 || out.Len() != 0 {
		t.Fatalf("judge env: code=%d out=%q", code, out.String())
	}
	delete(env, JudgeEnv)
	// 壊れた stdin でも exit 0・無出力（ログに残る）。
	out.Reset()
	if code := RunStopHook("claude", strings.NewReader(`{bad`), &out, &errb, getenv); code != 0 || out.Len() != 0 {
		t.Fatalf("bad json: code=%d out=%q", code, out.String())
	}
	b, _ := os.ReadFile(st.LogPath())
	if !strings.Contains(string(b), "stdin JSON 解釈失敗") {
		t.Fatalf("log:\n%s", b)
	}
}

// 実 exec 経路: 判定器サブプロセスから HERDR_* / CLAUDECODE が消え、再帰ガード env が立つ。
func TestRunExecStripsHerdrEnv(t *testing.T) {
	t.Setenv("HERDR_PANE_ID", "w9:p9")
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_ENTRYPOINT", "cli")
	out, err := runExec(context.Background(), []string{"sh", "-c", `echo "pane=${HERDR_PANE_ID:-none} env=${HERDR_ENV:-none} cc=${CLAUDECODE:-none} ep=${CLAUDE_CODE_ENTRYPOINT:-none} judge=${HERDR_DROVER_AUTO_JUDGE:-none}"`}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "pane=none env=none cc=none ep=none judge=1" {
		t.Fatalf("env が漏れている: %q", out)
	}
	if _, err := runExec(context.Background(), []string{"sh", "-c", "echo oops >&2; exit 3"}, t.TempDir()); err == nil || !strings.Contains(err.Error(), "oops") {
		t.Fatalf("stderr が error に載らない: %v", err)
	}
}

// Codex はフックが共有 app-server 内で走り HERDR_PANE_ID が信用できない（実測）ため、
// cwd で herdr の agent 一覧から一意に引く。↗窓は除外・曖昧と該当なしは空＋理由。
func TestResolvePaneByCwd(t *testing.T) {
	list := func() ([]herdrapi.AgentInfo, error) {
		return []herdrapi.AgentInfo{
			{Agent: "codex", PaneID: "w1:p5B", Cwd: "/u/semi-auto-test", ForegroundCwd: "/u/semi-auto-test"},
			{Agent: "claude", PaneID: "w1:p5A", Cwd: "/u/semi-auto-test", ForegroundCwd: "/u/semi-auto-test"},
			{Agent: "codex", PaneID: "w22:p23", Cwd: "/u/hr01", ForegroundCwd: "/u/hr01"},
			{Agent: "codex", PaneID: "w22:p24", Cwd: "/u/hr01", ForegroundCwd: "/u/hr01"},
			{Agent: "codex", PaneID: "w31:p19", Cwd: "/Users/x/.herdr-drover", ForegroundCwd: "/Users/x/.herdr-drover",
				Tokens: map[string]string{"drover_inj_sid": "w29:pN", "drover_inj_cwd": "/u/semi-auto-test"}},
			{Agent: "codex", PaneID: "w9:p1", Cwd: "/u/sub", ForegroundCwd: ""},
		}, nil
	}
	cases := []struct{ cwd, pane, via string }{
		{"/u/semi-auto-test", "w1:p5B", "cwd"},
		{"/u/sub", "w9:p1", "cwd"}, // foreground_cwd 無しは cwd で
		{"/u/hr01", "", "cwd:曖昧(2件 w22:p23,w22:p24)"},
		{"/u/none", "", "cwd:該当なし"},
		{"", "", "cwd:empty"},
	}
	for _, c := range cases {
		pane, via := ResolvePaneByCwd("codex", c.cwd, list)
		if pane != c.pane || via != c.via {
			t.Errorf("cwd=%q: got (%q,%q) want (%q,%q)", c.cwd, pane, via, c.pane, c.via)
		}
	}
	if pane, via := ResolvePaneByCwd("codex", "/u/x", func() ([]herdrapi.AgentInfo, error) { return nil, errors.New("down") }); pane != "" || !strings.HasPrefix(via, "cwd:agent.list失敗") {
		t.Errorf("list error: (%q,%q)", pane, via)
	}
}

func TestHookLogsUnresolvedPane(t *testing.T) {
	h, st := newTestHook(t, fakeJudge("CONTINUE", nil))
	st.SaveConfig(Config{Enabled: boolp(true)})
	h.Agent, h.PaneID, h.PaneVia = "codex", "", "cwd:曖昧(2件 a,b)"
	res, err := h.Run(context.Background(), Input{SessionID: "s", Cwd: "/u/hr01", LastAssistantMessage: strp("次はステップ 2")})
	if err != nil || res.Output != nil || res.Skipped != "no-pane" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	b, _ := os.ReadFile(st.LogPath())
	if !strings.Contains(string(b), "codex pane 未解決（cwd:曖昧(2件 a,b)）cwd=/u/hr01") {
		t.Fatalf("log:\n%s", b)
	}
}

// 直近の指示が素の「つづけて」系（裁量語なし）: 上限は 3 のまま、文言はその言い回し、
// 判定器には asked=continue が渡る。
func TestHookReusesPlainContinueInstruction(t *testing.T) {
	var gotPrompt string
	judge := &Judge{Cmd: []string{"fake"}, Timeout: time.Second, Run: func(_ context.Context, argv []string, _ string) (string, error) {
		gotPrompt = argv[len(argv)-1]
		return "CONTINUE", nil
	}}
	h, st := newTestHook(t, judge)
	st.SaveConfig(Config{Enabled: boolp(true)})
	tp := filepath.Join(t.TempDir(), "c.jsonl")
	os.WriteFile(tp, []byte(`{"type":"user","message":{"role":"user","content":"改善を続けてください"}}`+"\n"), 0o600)
	in := Input{SessionID: "s2", TranscriptPath: tp, LastAssistantMessage: strp("方針は A か B です。どちらにしますか？")}
	res, err := h.Run(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Output == nil || res.Output.Reason != "[semi-auto 1/10] 改善を続けてください" || res.Grant || res.Cap != 10 {
		t.Fatalf("res=%+v out=%+v", res, res.Output)
	}
	if !strings.Contains(gotPrompt, "keep going") {
		t.Fatalf("判定器に asked=continue が渡っていない:\n%s", gotPrompt)
	}
	if !strings.Contains(res.LogLine, "asked=continue") {
		t.Fatalf("log: %s", res.LogLine)
	}
}

// ---- 事例の蓄積とコーパス ------------------------------------------------------

// 判定のたびに cases.jsonl に 1 行残り、停止後に人が打った最初の指示が followup として
// 合成される（『つづけて』なら「その停止は誤りだった示唆」）。ラベルは labels.jsonl。
func TestHookRecordsCasesAndFollowup(t *testing.T) {
	h, st := newTestHook(t, fakeJudge("HUMAN", nil))
	st.SaveConfig(Config{Enabled: boolp(true)})
	tp := filepath.Join(t.TempDir(), "c.jsonl")
	os.WriteFile(tp, []byte(`{"type":"user","message":{"role":"user","content":"全部やって"}}`+"\n"), 0o600)
	in := Input{SessionID: "s9", TranscriptPath: tp, LastAssistantMessage: strp("方針 A と B があります。どちらにしますか？")}
	if _, err := h.Run(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	cases, err := st.ReadCases(0)
	if err != nil || len(cases) != 1 {
		t.Fatalf("cases=%d err=%v", len(cases), err)
	}
	c := cases[0]
	if c.Decision != "stop" || c.Verdict != "human" || !strings.HasPrefix(c.Source, "llm") || c.Rules != RulesVersion || c.Prompt != "全部やって" || !c.Grant ||
		c.Message != "方針 A と B があります。どちらにしますか？" || c.Cap != DefaultGrantCap {
		t.Fatalf("case: %+v", c)
	}
	// 次の人間入力が『つづけて』→ 直前の停止事例に followup が付く。
	os.WriteFile(tp, []byte(`{"type":"user","message":{"role":"user","content":"全部やって"}}`+"\n"+
		`{"type":"user","message":{"role":"user","content":"つづけて"}}`+"\n"), 0o600)
	h.NewJudge = func([]string, string) *Judge { return fakeJudge("CONTINUE", nil) }
	in.LastAssistantMessage = strp("続けます。")
	if _, err := h.Run(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	cases, _ = st.ReadCases(0)
	if len(cases) != 2 {
		t.Fatalf("cases=%d", len(cases))
	}
	if cases[0].FollowupPrompt != "つづけて" || cases[0].FollowupContinue == nil || !*cases[0].FollowupContinue {
		t.Fatalf("followup が付いていない: %+v", cases[0])
	}
	if cases[1].Decision != "continue" || cases[1].Reason != "[semi-auto 1/10] つづけて" || !cases[1].Asked {
		t.Fatalf("2 件目: %+v", cases[1])
	}
	// ラベル付与と合成、末尾 n 件、export の形。
	if err := st.LabelCase(cases[0].ID, "continue", "選択肢は自分で選んでよい", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := st.LabelCase(cases[0].ID, "maybe", "", time.Now()); err == nil {
		t.Fatal("不正ラベルが通った")
	}
	last, _ := st.ReadCases(1)
	if len(last) != 1 || last[0].ID != cases[1].ID {
		t.Fatalf("末尾 1 件: %+v", last)
	}
	all, _ := st.ReadCases(0)
	if all[0].Label != "continue" || all[0].LabelNote != "選択肢は自分で選んでよい" {
		t.Fatalf("label 合成: %+v", all[0])
	}
	cc := ExportCorpus(all[0], "テスト", time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC))
	if cc.Expect != "continue" || cc.ExpectSource != "llm" || cc.Rules != RulesVersion || cc.Message != all[0].Message || cc.Added != "2026-10-29" {
		t.Fatalf("export: %+v", cc)
	}
	// 上限到達も事例として残る（decision=cap）。
	st.SaveConfig(Config{Enabled: boolp(true), Cap: 1, GrantCap: 1})
	in.StopHookActive = true
	if _, err := h.Run(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	all, _ = st.ReadCases(0)
	if all[len(all)-1].Decision != "cap" {
		t.Fatalf("cap 事例: %+v", all[len(all)-1])
	}
}

// TestJudgeCorpus はリポジトリ内コーパス（testdata/judge_cases.json）に対するルール層の
// 全体整合性を固定する。expect_source が rule:… の事例はその規則で決まること、llm の
// 事例はルールで決めずに判定器へ渡ること（＝判定器の答えがそのまま結果になること）を
// 検証する。HERDR_DROVER_LIVE_JUDGE=1 なら llm の事例を実判定器（claude -p）で評価する。
func TestJudgeCorpus(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "judge_cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var corpus []CorpusCase
	if err := json.Unmarshal(b, &corpus); err != nil {
		t.Fatal(err)
	}
	if len(corpus) == 0 {
		t.Fatal("コーパスが空")
	}
	seen := map[string]bool{}
	live := os.Getenv("HERDR_DROVER_LIVE_JUDGE") == "1"
	var liveJudge *Judge
	if live {
		liveJudge = NewJudge(DefaultJudgeCmd(), t.TempDir())
	}
	for _, c := range corpus {
		if c.ID == "" || seen[c.ID] {
			t.Fatalf("id が空か重複: %q", c.ID)
		}
		seen[c.ID] = true
		if c.Origin == "" || c.Added == "" || c.Message == "" {
			t.Fatalf("%s: origin/added/message は必須", c.ID)
		}
		switch c.Expect {
		case "continue", "human", "done":
		default:
			t.Fatalf("%s: expect は continue|human|done（%q）", c.ID, c.Expect)
		}
		asked := c.Asked || ContinueLike(c.Prompt)
		grant := c.Grant || GrantIn(c.Prompt)
		// ルール層のみ: 判定器は「呼ばれたら期待どおり答える」fake にする。
		called := false
		fake := &Judge{Cmd: []string{"fake"}, Timeout: time.Second, Run: func(context.Context, []string, string) (string, error) {
			called = true
			return strings.ToUpper(c.Expect), nil
		}}
		got := fake.Judge(context.Background(), c.Message, c.Prompt, grant, asked)
		if string(got.Verdict) != c.Expect {
			t.Errorf("%s: verdict=%s source=%s want %s（origin: %s）", c.ID, got.Verdict, got.Source, c.Expect, c.Origin)
			continue
		}
		if c.ExpectSource == "llm" {
			if !called || !strings.HasPrefix(got.Source, "llm") {
				t.Errorf("%s: 判定器へ渡るべき事例がルールで決まった: source=%s detail=%q", c.ID, got.Source, got.Detail)
			}
		} else if got.Source != c.ExpectSource {
			t.Errorf("%s: source=%s want %s", c.ID, got.Source, c.ExpectSource)
		}
		if live && c.ExpectSource == "llm" {
			lg := liveJudge.Judge(context.Background(), c.Message, c.Prompt, grant, asked)
			if string(lg.Verdict) != c.Expect {
				t.Errorf("%s: 実判定器=%s（src=%s）want %s", c.ID, lg.Verdict, lg.Source, c.Expect)
			} else {
				t.Logf("%s: 実判定器=%s ok", c.ID, lg.Verdict)
			}
		}
	}
}

// 判定器の多数決: 3 票の過半数を採り、割れたら停止側（human）。並列実行。
func TestJudgeMajorityVote(t *testing.T) {
	var n atomic.Int32
	answers := []string{"DONE", "CONTINUE", "DONE"}
	j := &Judge{Cmd: []string{"fake"}, Timeout: time.Second, Votes: 3, Run: func(context.Context, []string, string) (string, error) {
		i := int(n.Add(1)) - 1
		return answers[i%3], nil
	}}
	got := j.Judge(context.Background(), "完了しました。", "", false, false)
	if got.Verdict != VerdictDone || got.Source != "llm:2/3" || n.Load() != 3 {
		t.Fatalf("majority: %+v calls=%d", got, n.Load())
	}
	n.Store(0)
	answers = []string{"DONE", "CONTINUE", "HUMAN"}
	got = j.Judge(context.Background(), "完了しました。", "", false, false)
	if got.Verdict != VerdictHuman || got.Source != "llm:no-majority" {
		t.Fatalf("split: %+v", got)
	}
	// 1 票だけ失敗しても残りで決める。
	n.Store(0)
	j.Run = func(context.Context, []string, string) (string, error) {
		i := int(n.Add(1)) - 1
		if i == 1 {
			return "", errors.New("boom")
		}
		return "CONTINUE", nil
	}
	got = j.Judge(context.Background(), "完了しました。", "", false, false)
	if got.Verdict != VerdictContinue || got.Source != "llm:2/3" {
		t.Fatalf("one error: %+v", got)
	}
}
