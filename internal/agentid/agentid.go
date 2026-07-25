// Package agentid はコーディングエージェントの **identity 判定を 1 箇所に集約**する
// （DESIGN_MULTI_AGENT.md の P1）。
//
// ⚠**cmd 配下ではなく internal に置く理由**: pane を列挙する経路は cmd/herdr-drover
// だけでなく internal/session（producer）にもある。cmd 内に置くと producer から
// 呼べず判定が再び分裂する＝P1 で潰した非対称がぶり返す。SPEC.md §7-2 の
// 「新しい pane 列挙経路を足すときは Resolve を通すこと」を構造的に守るための配置。
//
// 従来は同じ「この pane はどのエージェントか」の判定が 3 系統に散っていた:
//
//	claudeshim.claudeCandidates   … シム命名のみ
//	restartclaude.selectRestartTargets … シム命名のみ（organize と非対称＝穴）
//	organize.classifyClaudePane   … シム命名 OR 検出値 ＋ 矛盾判定
//
// 判定規則がずれると「organize は拾うのに restart は拾わない」のような静かな
// 非対称になる（実際になっていた）。本ファイルの Resolve を唯一の
// 入口にする。
//
// # 権威の優先順位（実測に基づく）
//
//	0. 注入 pane（identity token あり）は **常に対象外**
//	1. agent_session の (source, agent)         … 最も強い
//	2. シム命名 <agent> / <agent>-N              … 慣習的に drover が書く
//	3. herdr の検出値 agent（canonical のみ）    … 最も弱い
//
// **0 が最優先である理由（実測 2026-07-25）**: ↗窓 注入 pane は reconcile の
// mirror_agents がリモート側の agent 名を herdr の検出値へ転記するため、実際に
// `agent == "claude"` になっている pane が存在する。token 除外を後ろに置くと
// リモートの鏡をローカルセッションと誤認する（organize が実際にこれを踏んだ）。
//
// **1 が最も強い理由**: herdr は agent_session の `(source, agent)` を 14 組の
// exact 許可制でしか受け付けない（`src/agent_resume.rs is_official_agent_source`）。
// 未知の組は保存されないので、**値域が canonical に閉じている**のが強みである。
//
// ⚠**「偽装が構造的に不可能」ではない**: `pane.report_agent_session` は認証の無い
// 公開 ndjson メソッドで、socket に届く任意のローカルプロセスが `herdr:claude` を
// 名乗れる（本リポジトリのテスト自身がそうしている）。許可制は「どの組を受けるか」
// であって「誰が名乗れるか」ではない。herdr socket に触れる相手は既に何でもできる
// 立場なので実害は増えないが、**根拠を過大評価しないこと**。
//
// ⚠**2 も「自分で書いた値」ではない**: agent 名は herdr の `agent rename` /
// `agent start <name>` でユーザーが任意に付けられる同一フィールドで、drover 由来かを
// 示す印は無い（herdr は空でない・一意しか要求しない）。よって 2 は
// 「drover の書き込みが**慣習的に**支配的なフィールド」程度の強さしかない。
//
// **3 が最も弱い理由**: herdr の `effective_agent_label()` は hook 由来の申告値を
// 最優先するため、この値は「検出値 ∪ 外部申告値」である（drover 自身の
// mirror_agents もその書き手）。よって **canonical label への exact-match を必須**
// にし、未知の文字列は採用しない（ヒューリスティック分類禁止の鉄則③）。
//
// 矛盾（複数の権威が食い違う）は **機械確定不能として報告**し、推測で動かさない。
//
// # kind ≠ 破壊してよい対象
//
// Resolve が返すのは「**何のエージェントか**」だけである。restart /
// update のように pane を作り直す操作は、それに加えて
// **IsDirectInvocation で「その argv がエージェントの直接起動か」**を必ず
// 確認すること。identity を検出値まで広げた結果、`zsh -lc '… claude'` のような
// wrapper 起動 pane も kind=claude になるが、その argv に `--resume` を足しても
// claude には届かない（＝引き継げていないのに成功と報告してしまう）。

package agentid

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/4noha/herdr-drover/internal/herdrapi"
)

// CanonicalLabels は herdr が `pane.agent` / `agent.agent` に出す canonical な
// エージェント種別の全集合。herdr 0.7.4 の `src/detect/mod.rs agent_label()` から
// 実ソース抽出した 21 値（改変禁止のソースを読むだけ＝vendor ではない）。
//
// ⚠ここに**無い**文字列は identity に採用しない。herdr は未知ラベルを trim して
// 素通しする（`normalize_reported_agent_label`）ので、素通し値を信じると
// 外部申告をそのまま identity にしてしまう。
//
// ⚠**canonical label は `-` を含まない**（実ソースで確認済み）。これが
// `<agent>-N` の decode が一意である根拠。`claude-code` / `cursor-agent` /
// `kilo-code` は herdr の `lookup_agent` が**プロセス名を照合する入力 alias**で
// あって、この canonical 集合の値ではない。
var CanonicalLabels = map[string]bool{
	"agy": true, "amp": true, "claude": true, "cline": true, "codex": true,
	"copilot": true, "cursor": true, "devin": true, "droid": true, "gemini": true,
	"grok": true, "hermes": true, "kilo": true, "kimi": true, "kiro": true,
	"maki": true, "mastracode": true, "omp": true, "opencode": true, "pi": true,
	"qodercli": true,
}

func IsCanonical(s string) bool { return CanonicalLabels[s] }

// Encode は pane に付ける agent 名を作る（n=1 → "<agent>"、
// n>=2 → "<agent>-N"）。Decode と**厳密に往復**すること。
func Encode(agent string, n int) string {
	if n <= 1 {
		return agent
	}
	return fmt.Sprintf("%s-%d", agent, n)
}

// Decode は Encode が実際に生成する形だけを exact に受ける。
// decode は encode の**真部分集合**でなければならない（片側だけ広げると
// 「対象 0 件」や「他エージェントの pane を横取り」が静かに起きる）。
//
//   - "<agent>"        → (agent, true)   agent は canonical のみ
//   - "<agent>-N"      → (agent, true)   N>=2・先頭ゼロ無し（%d は生成しない）
//   - それ以外          → ("", false)
func Decode(name string) (string, bool) {
	if name == "" {
		return "", false
	}
	if IsCanonical(name) {
		return name, true
	}
	i := strings.LastIndex(name, "-")
	if i <= 0 || i == len(name)-1 {
		return "", false
	}
	agent, rest := name[:i], name[i+1:]
	if !IsCanonical(agent) || rest[0] == '0' {
		return "", false
	}
	// ⚠数字だけを許す。strconv.Atoi は符号を受理する（`+2` が通る）ため、
	// Atoi に任せると encode が生成しない `claude-+2` を decode が受けてしまい、
	// 「decode ⊆ encode」の契約が破れる＝無関係な pane を claude と誤認する。
	for i := 0; i < len(rest); i++ {
		if rest[i] < '0' || rest[i] > '9' {
			return "", false
		}
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n < 2 {
		return "", false
	}
	return agent, true
}

// execNames は「その argv が当該エージェントの**直接起動**か」を判定する
// 実行ファイル名（basename）の許容集合。DESIGN_MULTI_AGENT.md の ResumeSpec の芽。
//
// identity（何のエージェントか）と actionability（drover が argv を組み立て直せるか）
// は別物。restart/update は後者も満たす pane にだけ触れる。
var execNames = map[string][]string{
	"claude": {"claude"},
}

// IsDirectInvocation は launch_argv がエージェント本体の直接起動かを返す。
// `zsh -lc '… claude'` のような wrapper 経由では false（`--resume` を足しても
// エージェントに届かないため、再起動すると会話を失う）。
//
// 表に無い kind は false＝**知らないものは触らない**（推測禁止の鉄則①③）。
func IsDirectInvocation(kind string, argv []string) bool {
	if len(argv) == 0 {
		return false
	}
	base := filepath.Base(argv[0])
	for _, want := range execNames[kind] {
		if base == want {
			return true
		}
	}
	return false
}

// Identity は identity 判定に要る最小情報。PaneInfo / AgentInfo の
// どちらからでも作れる（herdr 側で両者はほぼ同一スキーマ＝実測で全フィールド
// 一致を確認済み）。
type Identity struct {
	ShimName string                // drover が agent.rename で付けた名前（未命名は ""）
	Detected string                // herdr の検出値（canonical とは限らない）
	Session  herdrapi.AgentSession // herdr が exact 許可制で受けた会話 session
	Tokens   map[string]string     // metadata token（↗窓 注入 pane の identity）
}

func OfAgent(a herdrapi.AgentInfo) Identity {
	return Identity{ShimName: a.Name, Detected: a.Agent, Session: a.AgentSession, Tokens: a.Tokens}
}

// OfPane は pane.list 由来。shimName は agent.list の Name を渡す
// （PaneInfo.Label は実測で同値だが、命名の権威は agent 側なので明示的に受ける）。
func OfPane(p herdrapi.PaneInfo, shimName string) Identity {
	return Identity{ShimName: shimName, Detected: p.Agent, Session: p.AgentSession, Tokens: p.Tokens}
}

// Resolve は pane のエージェント種別を exact-match で 1 つに決める。
// 戻り値:
//
//	kind     … canonical なエージェント種別。判定できなければ ""
//	conflict … 非空なら**機械確定不能**（呼び手は対象外にして必ず報告する）
//
// 上位の comment の優先順位どおり。矛盾は握り潰さず conflict で返す。
// injectTokenKeys は ↗窓 注入 pane に reconcile が付ける metadata token。
var injectTokenKeys = []string{herdrapi.InjTokenPC, herdrapi.InjTokenSID}

// HasInjectToken は identity token の有無で ↗窓 注入 pane を判定する純関数。
// **どちらか一方でも在れば注入 pane**（安全側＝取り違えて破壊しない）。
func HasInjectToken(tokens map[string]string) bool {
	for _, k := range injectTokenKeys {
		if _, ok := tokens[k]; ok {
			return true
		}
	}
	return false
}

func Resolve(id Identity) (kind, conflict string) {
	// 0. ↗窓 注入 pane は常に対象外（reconcile の領分。mirror された検出値が
	//    canonical に一致しうるので**必ず最初に**弾く）。
	if HasInjectToken(id.Tokens) {
		return "", ""
	}

	// 1. agent_session（値域が 14 組の exact 許可制に閉じている。ただし
	//    「誰が名乗れるか」の認証ではない＝上部 comment の警告を参照）
	fromSession := ""
	if a := id.Session.Agent; a != "" && id.Session.Source == "herdr:"+a && IsCanonical(a) {
		fromSession = a
	}
	// 2. シム命名（ユーザーも書ける同一フィールド。慣習的に drover が書く）
	fromName, _ := Decode(id.ShimName)
	// 3. herdr の検出値（canonical への exact-match のみ採用）
	fromDetect := ""
	if IsCanonical(id.Detected) {
		fromDetect = id.Detected
	}

	// 矛盾検出: 非空の権威同士が食い違ったら機械確定不能。
	// 強い順に見て、後続が食い違ったら報告する（推測で片方を採らない）。
	switch {
	case fromSession != "" && fromName != "" && fromSession != fromName:
		return "", fmt.Sprintf("会話 session の種別 %q とシム命名 %q が矛盾（機械確定不能）",
			fromSession, id.ShimName)
	case fromSession != "" && fromDetect != "" && fromSession != fromDetect:
		return "", fmt.Sprintf("会話 session の種別 %q と herdr 検出種別 %q が矛盾（機械確定不能）",
			fromSession, fromDetect)
	case fromName != "" && fromDetect != "" && fromName != fromDetect:
		return "", fmt.Sprintf("シム命名 %q（=%s）と herdr 検出種別 %q が矛盾（機械確定不能）",
			id.ShimName, fromName, fromDetect)
	}

	// ⚠検出値が**非空だが canonical でない**のに命名だけ一致する場合も矛盾扱い。
	// herdr が別物を検出しているのに drover の命名で上書きするのは推測になる
	// （旧 classifyClaudePane の `named && p.Agent != "" && !detected` を一般化）。
	if fromName != "" && fromDetect == "" && id.Detected != "" {
		return "", fmt.Sprintf("シム命名 %q（=%s）と herdr 検出種別 %q（canonical 外）が矛盾（機械確定不能）",
			id.ShimName, fromName, id.Detected)
	}

	switch {
	case fromSession != "":
		return fromSession, ""
	case fromName != "":
		return fromName, ""
	default:
		return fromDetect, ""
	}
}
