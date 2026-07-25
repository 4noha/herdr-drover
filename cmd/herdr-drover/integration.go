package main

// integration — herdr の agent integration hook（`herdr integration install <agent>`）を
// **必要になった時点で自動導入**する。
//
// # なぜ要るか
//
// `agent_session`（会話 ref）は herdr が自力で見つけるのではなく、**各エージェントの
// hook が報告する**。hook が無いと ref が付かず、restart-agent-session は resume 引数を
// 付けられない＝**会話を失ったまま作り直す**。実測 2026-07-25: codex/cursor は
// integration を入れるまで ref がまったく付かなかった。
//
// # ⚠ これは「後から入れても遡らない」
//
// hook は **session 開始時**に発火する。既に走っているセッションに後から
// integration を入れても、そのセッションの ref は**永久に付かない**（次に開始する
// セッションから有効）。だから自動導入だけでは不十分で、restart 側にも
// 「ref が取れない pane は既定で触らない」安全網が要る（restartclaude.go）。
//
// # 規律
//
//   - **silent に設定を書き換えない**（鉄則⑤）。導入したら必ず 1 行出す。
//   - **未導入のときだけ入れる**。導入済み（current/outdated）には触らない
//     — ユーザーが手で直した hook を勝手に上書きしない。
//   - 失敗しても**起動を止めない**（hook が無くても agent 自体は動く）。
//   - herdr CLI をサブプロセスで呼ぶだけ＝プロセス境界のデータ交換（鉄則④）。

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// integrationTimeout は herdr CLI 1 回の上限（status/install とも短時間で終わる）。
const integrationTimeout = 20 * time.Second

// integrationOnce は同一プロセス内での重複実行を防ぐ（シムは 1 回起動 1 回だが、
// daemon 経路から呼ぶ場合に効く）。
var integrationOnce sync.Map // agent -> *sync.Once

// integrationStatus は `herdr integration status` の 1 行を分解した結果。
type integrationStatus struct {
	Known     bool // その agent の行が status に在ったか（herdr が対応する種別か）
	Installed bool // "not installed" 以外＝何らかの版が入っている
	Raw       string
}

// parseIntegrationStatus は status 出力から agent の行を exact に取り出す。
//
// 行の形（herdr 0.7.4 実測）:
//
//	pi: not installed (/Users/…/herdr-agent-state.ts)
//	claude: current (v7) (/Users/…/herdr-agent-state.sh)
//
// **prefix は `<agent>: ` の exact-match**（部分一致にすると codex と qodercli の
// ような紛らわしい組で誤判定しうる）。判定できない形は Known=false にして
// **何もしない**（未知の出力を推測で解釈しない＝鉄則③）。
func parseIntegrationStatus(out, agent string) integrationStatus {
	prefix := agent + ": "
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		rest := strings.TrimPrefix(line, prefix)
		return integrationStatus{
			Known:     true,
			Installed: !strings.HasPrefix(rest, "not installed"),
			Raw:       line,
		}
	}
	return integrationStatus{}
}

// herdrIntegrationCmd はテスト用 seam（既定は実 herdr CLI を叩く）。
var herdrIntegrationCmd = func(args ...string) (string, error) {
	cmd := exec.Command("herdr", append([]string{"integration"}, args...)...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// ensureAgentIntegration は agent の integration hook が入っていなければ導入する。
// 戻り値は「今回導入したか」。エラーは呼び手を止めない（ログのみ）。
//
// ⚠ **既に導入済みなら一切触らない**（版が古くても上書きしない）。ユーザーが
// 手で直した hook を勝手に戻すのは silent な設定変更になるため、報告に留める。
func ensureAgentIntegration(agent string, out io.Writer) bool {
	if os.Getenv("DROVER_AUTO_INTEGRATION") == "off" {
		return false
	}
	statusOut, err := herdrIntegrationCmd("status")
	if err != nil {
		fmt.Fprintf(out, "integration: status 取得に失敗（hook 自動導入を見送り）: %v\n", err)
		return false
	}
	st := parseIntegrationStatus(statusOut, agent)
	if !st.Known {
		// herdr がその種別の integration を持たない（例: gemini 等）＝何もしない。
		return false
	}
	if st.Installed {
		return false
	}
	fmt.Fprintf(out, "integration: %s の hook が未導入＝会話 ref（agent_session）が"+
		"報告されず resume できません。`herdr integration install %s` を実行します\n", agent, agent)
	installOut, err := herdrIntegrationCmd("install", agent)
	if err != nil {
		fmt.Fprintf(out, "integration: %s の導入に失敗（このまま起動します。"+
			"このセッションは resume 不可）: %v %s\n", agent, err, strings.TrimSpace(installOut))
		return false
	}
	for _, line := range strings.Split(strings.TrimSpace(installOut), "\n") {
		if line != "" {
			fmt.Fprintf(out, "integration: %s\n", line)
		}
	}
	fmt.Fprintf(out, "integration: %s の hook を導入しました"+
		"（⚠**既に走っているセッションには遡りません**。次に開始するものから有効）\n", agent)
	return true
}

// ensureAgentIntegrationOnce はプロセス内で agent ごとに 1 回だけ実行する。
func ensureAgentIntegrationOnce(agent string, out io.Writer) {
	v, _ := integrationOnce.LoadOrStore(agent, &sync.Once{})
	v.(*sync.Once).Do(func() { ensureAgentIntegration(agent, out) })
}
