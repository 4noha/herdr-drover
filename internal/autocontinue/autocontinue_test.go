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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
		{"人にしかできない確認の疑問文は停止", "実装は整いました。**Cloud Run へデプロイしてよいですか？**", "CONTINUE", nil, VerdictHuman, "rule:human-only-question"},
		{"パスワードを聞く疑問文は停止", "SSH 先の認証情報（パスワード）を教えてもらえますか？", "CONTINUE", nil, VerdictHuman, "rule:human-only-question"},
		{"普通の選択肢提示は判定器へ→CONTINUE", "方針は 1. A 2. B 3. C です。おすすめは 2。どれにしますか？", "CONTINUE", nil, VerdictContinue, "llm"},
		{"判定器 DONE", "すべて完了しました。", "DONE\n", nil, VerdictDone, "llm"},
		{"判定器 HUMAN", "どの端末に接続しますか。", "The answer is HUMAN.", nil, VerdictHuman, "llm"},
		{"判定器エラーは停止側", "完了しました。", "", errors.New("boom"), VerdictHuman, "fallback:judge-error"},
		{"判定器出力が読めないのは停止側", "完了しました。", "maybe", nil, VerdictHuman, "fallback:unparsed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := fakeJudge(c.reply, c.err).Judge(context.Background(), c.msg, "", false)
			if got.Verdict != c.want || got.Source != c.wantSrc {
				t.Fatalf("got %+v want verdict=%s src=%s", got, c.want, c.wantSrc)
			}
		})
	}
	// 判定器が無い（Run=nil）構成は停止側。
	j := &Judge{}
	if got := j.Judge(context.Background(), "完了しました。", "", false); got.Verdict != VerdictHuman || got.Source != "fallback:no-judge" {
		t.Fatalf("no-judge: %+v", got)
	}
}

func TestBuildPromptCarriesContext(t *testing.T) {
	p := buildPrompt("最終発話", "いけるところまでつづけて", true)
	for _, want := range []string{"CONTINUE, HUMAN, or DONE", "granted autonomy", "いけるところまでつづけて", "最終発話"} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt に %q が無い:\n%s", want, p)
		}
	}
	if strings.Contains(buildPrompt("x", "", false), "granted autonomy") {
		t.Fatal("裁量なしで granted の文言が出ている")
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
	// 1〜3 回目: 続行（reason に n/cap と文言・mark=continue・counter 増加）。
	for i := 1; i <= 3; i++ {
		in.StopHookActive = i > 1
		res, err := h.Run(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		want := "[semi-auto " + string(rune('0'+i)) + "/3] つづけて"
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
	// 4 回目: 上限＝停止・マーク消滅（通知は通常どおり飛ぶ）。
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
	if res.Output == nil || res.Output.Reason != "[semi-auto 1/3] つづけて" {
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
	if n := strings.Count(string(b), "\n"); n != 6 {
		t.Fatalf("log 行数=%d want 6:\n%s", n, b)
	}
	if !strings.Contains(string(b), "verdict=human src=llm→停止") || !strings.Contains(string(b), "上限到達→停止") {
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
	if res.Output == nil || res.Output.Reason != "[semi-auto 1/3] 改善を続けてください" {
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
	if res.Output == nil || res.Output.Reason != "[semi-auto 1/10] 最後まで自律的にすすめて" || !res.Grant || res.Cap != 10 {
		t.Fatalf("grant: %+v out=%+v", res, res.Output)
	}
	// pane 設定があっても phrase 未指定なら、ユーザの言い回しを使う。
	st.SavePane("w1:p4F", PaneSetting{Enabled: boolp(true)})
	in.StopHookActive = true
	res, _ = h.Run(context.Background(), in)
	if res.Output == nil || res.Output.Reason != "[semi-auto 2/10] 最後まで自律的にすすめて" {
		t.Fatalf("pane without phrase: %+v", res.Output)
	}
	// pane 側に文言の明示指定があればそちらが優先。
	st.SavePane("w1:p4F", PaneSetting{Phrase: "つづけて"})
	res, _ = h.Run(context.Background(), in)
	if res.Output == nil || res.Output.Reason != "[semi-auto 3/10] つづけて" {
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
	if err := json.Unmarshal([]byte(out.String()), &o); err != nil || o.Decision != "block" || o.Reason != "[semi-auto 1/3] つづけて" {
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
