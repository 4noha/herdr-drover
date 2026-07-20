// Package session は herdr の pane/agent 一覧を cm STATUS スキーマ互換の
// session map へ変換し、クラウド（internal/cloud/state）へ push する
// producer を提供する（DESIGN「一覧同期」節の実装）。
//
// 設計上の不変条件（cm から継承）:
//   - scan エラー tick は skip＝前回状態維持。cm で「scan 失敗→空 STATUS
//     全置換→全窓消滅 flap」を起こした実事故（STATUS flap）の教訓。
//     エラーと「正常に 0 件」は峻別する（herdr は dial 失敗が明確に error
//     になるので、成功した空 list は本当に pane ゼロと信頼できる）。
//     この信頼はサーバ再起動窓についても実測で確定済み（レビュー指摘
//     「復元完了前に成功した空/部分 list が返ると削除→再作成 churn」の
//     検証結果＝窓は不在）: herdr v0.7.4 は session.json の pane 復元が
//     完了するまで API socket への dial 自体を受け付けず、graceful/
//     SIGKILL 再起動 各 5 回・pane 10 個・3ms poll の観測は常に
//     「dial 失敗 → 完全な list」の直接遷移だった。よって遅延削除
//     （1 tick confirm）は導入しない。前提の見張りは
//     TestTickServerRestartNoFlap（実サーバ再起動＋Tick 連打）が常設。
//   - ヒューリスティック分類はしない。is_active は agent_status の
//     exact-match（"working" のみ true）だけで決める。
//   - 揮発/毎操作変動フィールド（terminal_id・revision・scroll・focused）
//     は session map に載せない。載せると state.PushStatus の content_hash
//     ゲートが毎 tick 破れて near-$0 が壊れる（terminal_id はサーバ再起動
//     でも変わる揮発ハンドル＝実測）。
package session

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/4noha/herdr-drover/internal/herdrapi"
)

// StateClient は producer が使うクラウド側の契約（*state.Client が満たす）。
// インターフェース注入なのはテストで fake を挟み「scan エラー tick では
// Push/Delete が一切呼ばれない」を機械確認するため（統合テストは実物）。
type StateClient interface {
	// PushStatus は sessions を upsert（content_hash 不変なら非書込＝
	// near-$0 ゲートは state 側に実在することをコードで確認済み。
	// producer 側での前回 hash 比較は不要＝二重化しない）。
	PushStatus(ctx context.Context, sessions []map[string]any) (changed int, err error)
	// DeleteSession は自 PC の session doc を削除（終了の push 同期）。
	DeleteSession(ctx context.Context, key string) error
	// OwnSessionKeys は起動時 seed 用（再起動中に終了した pane の
	// 取りこぼし防止。cm producer ループと同じ規律）。
	OwnSessionKeys(ctx context.Context) ([]string, error)
}

// HerdrClient は producer が使う herdr 側の契約（*herdrapi.Client が満たす）。
// v0.5.x で WorkspaceList を削除（判定の権威が workspace label / workspace_id
// から token + injectindex へ移った＝cmd/herdr-drover/reconcile.go 参照）。
type HerdrClient interface {
	PaneList() ([]herdrapi.PaneInfo, error)
	AgentList() ([]herdrapi.AgentInfo, error)
}

// Producer は herdr pane/agent → session map → state push/delete の 1 PC
// 分を担う。Tick を周期＋events nudge で呼ぶのは agent 側（DESIGN:
// events は差分の権威でなく nudge・poll backstop 常設）。
// 並行 Tick は想定しない（単一 goroutine から呼ぶこと）。
type Producer struct {
	Herdr HerdrClient
	State StateClient

	// isInjected は「注入 pane 判定」の権威関数（v0.5.x〜）。injectindex から
	// 注入した関数を呼び、pane_id が Pending / Live どちらでも true を返す。
	// nil の場合は全 pane を非注入扱い（テスト・過渡期のフォールバック）。
	// producer は token 判定と OR で除外する（token の 2 穴：create race / herdr
	// 再起動での token 消失、を injectindex が塞ぐ）。
	isInjected func(paneID string) bool

	// prev は前 tick の生存キー集合。今 tick に居ないキーを
	// DeleteSession で消す（in-memory 差分＝追加 Firestore 読み無し）。
	prev map[string]bool
	// seeded は prev を Firestore 実態（OwnSessionKeys）で初期化済みか。
	// 初回 tick で seed し、失敗したら tick ごと skip して次回再試行する
	// （seed 無しで進むと agent 停止中に終了した pane の doc が永久残留）。
	seeded bool
}

// NewProducer は Producer を作る（cmd/herdr-drover/agent.go が期待する
// 契約名。引数はインターフェースなので *herdrapi.Client / *state.Client を
// そのまま渡せる＝テストでは fake も注入できる）。
// 注入 pane 判定は WithIsInjected で後付け注入する（cmd 側で injectindex.Index
// を持ち回す＝session パッケージから直接依存させないため）。
func NewProducer(h HerdrClient, st StateClient) *Producer {
	return &Producer{Herdr: h, State: st}
}

// WithIsInjected は注入 pane 判定関数を注入する（返り値は self＝method chain 可）。
// idx.IsInjected をそのまま渡す想定。テストでは任意の func を渡せる。
func (p *Producer) WithIsInjected(fn func(paneID string) bool) *Producer {
	p.isInjected = fn
	return p
}

// ShortDir は cwd 末尾のディレクトリ名（cm scanner.ShortDir と同一規則）。
// "/" は ASCII 1 バイトで、UTF-8 の多バイト文字は 0x2F を含まない設計
// なので、U+2010 ハイフン等を含むパス成分もバイト境界を壊さず安全に
// 分割できる（cm で lsof の C ロケール \xNN 化けに苦しんだ層は herdr が
// JSON/UTF-8 で返すため存在しない）。
func ShortDir(cwd string) string {
	d := strings.TrimRight(cwd, "/")
	if d == "" {
		return "unknown"
	}
	parts := strings.Split(d, "/")
	last := parts[len(parts)-1]
	if last == "" {
		return "unknown"
	}
	return last
}

// isActive は agent_status → is_active の exact-match 写像。
// "working" のみ true。idle/blocked/done/unknown・未知の値は全て false
// （状態不明を active に倒すと Web 側が偽の活動表示になる＝安全側）。
func isActive(agentStatus string) bool {
	return agentStatus == "working"
}

// BuildSessions は pane.list/agent.list の実データから cm STATUS スキーマ
// 互換の session map 群を組む（純関数・順序は key 昇順で決定的）。
//
// スキーマ（cm monitor.sessionDict 互換の部分集合）:
//   - key / session_id: pane_id（例 "w1:p1"。herdr server 再起動を跨いで
//     安定＝実測・Firestore doc id 制約適合。terminal_id は揮発なので不可）
//   - cwd / short_dir: pane の cwd とその末尾（ForegroundCwd は意図的に
//     使わない: fg プロセスの cd で毎回 content_hash が動き書込が増える上、
//     DESIGN の指定は cwd）
//   - window_name: pane に agent が居ればその name、無ければ pane label、
//     どちらも無ければ pane_id（exact な優先順位・推測しない）
//   - is_active: agent_status=="working" の exact-match
//   - agent_status: 生の値（Web/診断用。enum なので content_hash は安定）
//
// updated_at/version/content_hash は state.PushStatus が付与する契約
// （contentHash はこの 3 キーを除外して計算する＝state.go で確認）なので
// producer は載せない。pid は herdr pane に存在しないため載せない
// （偽値の捏造はしない）。
// isInjected は「この pane_id は注入 pane か」を返す関数（injectindex の権威）。
// nil 可（テスト・過渡期のフォールバック）。producer は token OR isInjected で
// 除外する（token 権威化の 2 穴 (a) create race / (b) herdr 再起動での token
// 消失、を index の Pending 予約と起動時 self-heal で塞ぐ）。
func BuildSessions(panes []herdrapi.PaneInfo, agents []herdrapi.AgentInfo, isInjected func(paneID string) bool) []map[string]any {
	// pane_id → agent の対応（agent の name/status を優先採用するため）。
	agentByPane := make(map[string]herdrapi.AgentInfo, len(agents))
	for _, a := range agents {
		agentByPane[a.PaneID] = a
	}
	out := make([]map[string]any, 0, len(panes))
	for _, p := range panes {
		if p.PaneID == "" {
			// pane_id 無しは同期キーを作れない（doc id 不能）。捏造せず落とす。
			continue
		}
		// リモート pane 注入（reconcile）が作った注入 pane＝他 PC のセッションの
		// viewer であって自 PC のセッションではない。Firestore へ push すると peer PC が
		// ListSessions で拾って再注入し、その注入 pane を自分の producer がまた push…と
		// cross-PC で無限増殖する（DESIGN の不変条件・敵対的レビューで確認済みの
		// critical 経路）。**判定の権威は token + injectindex**（v0.5.x〜。旧 workspace
		// 所属判定は完全廃止＝ユーザーの mv-tab / workspace rename に耐性）。
		// token race 窓は index の Pending 予約が、herdr 再起動での token 消失は起動時
		// self-heal（reconcile.go selfHealOnStartup）が塞ぐ。
		if p.Tokens[herdrapi.InjTokenPC] != "" {
			continue
		}
		if isInjected != nil && isInjected(p.PaneID) {
			continue
		}
		status := p.AgentStatus
		name := p.Label
		if a, ok := agentByPane[p.PaneID]; ok {
			// agent pane は agent レコードを権威にする（name は agent.start
			// の指定名＝reconcile の表示名経路と同じ源）。
			status = a.AgentStatus
			if a.Name != "" {
				name = a.Name
			}
		}
		if name == "" {
			name = p.PaneID
		}
		out = append(out, map[string]any{
			"key":          p.PaneID,
			"session_id":   p.PaneID,
			"cwd":          p.Cwd,
			"short_dir":    ShortDir(p.Cwd),
			"window_name":  name,
			"is_active":    isActive(status),
			"agent_status": status,
		})
	}
	// 決定的順序（テスト・ログ比較のため。Firestore 書込は doc 単位なので
	// 意味論には影響しない）。
	sort.Slice(out, func(i, j int) bool {
		return out[i]["key"].(string) < out[j]["key"].(string)
	})
	return out
}

// Tick は 1 回分の scan→push→消滅 delete を行う。
//
// エラー方針:
//   - scan（PaneList/AgentList）失敗 → **何もせず** error を返す＝skip。
//     prev も触らない（前回状態維持）。空 push で Web の一覧が flap する
//     cm 実事故の再発防止がこの関数の最重要不変条件。
//   - seed（OwnSessionKeys）失敗 → 同じく skip（次 tick で再試行）。
//   - push/delete 失敗 → 部分失敗として error は返すが tick は前進する。
//     push 失敗分は次 tick の content_hash 判定（doc 不在→changed）で
//     自然に再送される。delete 失敗キーは prev に持ち越して次 tick で
//     再試行する（cm は握り潰して doc が幽霊化し得た＝ここで改善）。
func (p *Producer) Tick(ctx context.Context) error {
	// scan は両方成功して初めて着手（部分結果で push しない。agent.list
	// だけ失敗した状態で進むと agent pane の name/status が全て pane 素値
	// に退行した「偽の差分」を書いてしまう）。
	panes, err := p.Herdr.PaneList()
	if err != nil {
		return fmt.Errorf("session: pane.list 失敗（tick skip・前回状態維持）: %w", err)
	}
	agents, err := p.Herdr.AgentList()
	if err != nil {
		return fmt.Errorf("session: agent.list 失敗（tick skip・前回状態維持）: %w", err)
	}
	// v0.5.x で workspace.list scan を撤去（判定の権威は token + injectindex に
	// 移った）。producer は Herdr.WorkspaceList を呼ばない＝1 tick の I/O が 1 本
	// 減る（実測ベンチには乗らないが、workspace.list 失敗による tick skip も無くなる）。
	// 注入 pane 判定は p.isInjected + token OR で BuildSessions が行う。

	if !p.seeded {
		keys, serr := p.State.OwnSessionKeys(ctx)
		if serr != nil {
			return fmt.Errorf("session: OwnSessionKeys seed 失敗（tick skip）: %w", serr)
		}
		p.prev = make(map[string]bool, len(keys))
		for _, k := range keys {
			p.prev[k] = true
		}
		p.seeded = true
	}

	ss := BuildSessions(panes, agents, p.isInjected)
	cur := make(map[string]bool, len(ss))
	for _, s := range ss {
		cur[s["key"].(string)] = true
	}

	var errs []error
	if len(ss) > 0 {
		if _, perr := p.State.PushStatus(ctx, ss); perr != nil {
			errs = append(errs, fmt.Errorf("session: PushStatus: %w", perr))
		}
	}

	// 消滅キーの delete。次 tick 用の集合は cur を基礎に、delete 失敗分
	// だけを持ち越す（成功分・今回生存分以外は忘れる）。
	next := make(map[string]bool, len(cur))
	for k := range cur {
		next[k] = true
	}
	for k := range p.prev {
		if cur[k] {
			continue
		}
		if derr := p.State.DeleteSession(ctx, k); derr != nil {
			next[k] = true // 再試行のため持ち越し
			errs = append(errs, fmt.Errorf("session: DeleteSession(%s): %w", k, derr))
		}
	}
	p.prev = next
	return errors.Join(errs...)
}
