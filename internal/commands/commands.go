// Package commands は遠隔命令（Firestore commands/{pc}/q）の購読・実行・
// 監査。cm internal/cloud/agent/commands.go のコピー適応（同一作者＝
// コピー自由・cm リポジトリは無改変）。
//
// owner 限定は web 側（cm relay/web を無改変共用）＝ここは多層防御として
// revocation を dispatch 前に再検査し、allowlist（state.ValidCommands 焼込み
// 3 種）の exact-match 分岐のみ実行する。実行系は全て seam 注入＝テストで
// 実 launchctl / 自己バイナリ置換 / os.Exit をしない。
//
// drover での写像（DESIGN「遠隔命令」）:
//
//	restart-agent  → launchctl kickstart -k（自己。DoRestart seam）
//	self-update    → selfupdate.Update → DoExit（os.Exit(0)。launchd
//	                 KeepAlive が新バイナリで再起動）
//	restart-proxy  → 当該 sid の bridge respawn（webterm。DoProxy seam）
//	restart-claude → claude セッションを会話ごと作り直して新バイナリを掴ませる
//	                 （DoRestartClaude seam。sid 空＝その PC のローカル claude
//	                 pane 全部）。破壊的だが**対象プロセスのみ**＝agent 自身は
//	                 死なないので Ack は実行**後**（結果の要約を detail に残す）
//	update-claude  → `claude update`＋上記の再起動を 1 コマンドで
//	                 （DoUpdateClaude seam）。self-update が **herdr-drover 自身**
//	                 の更新なのに対し、こちらは **claude 本体**の更新＝別物
//	update-all     → Web のワンボタン。claude 更新＋セッション反映 → 自己更新 →
//	                 自身の再起動、を**この順で逐次**（DoUpdateAll seam）。
//	                 ⚠自身の再起動は exit でしか反映できずハンドラが終わる＝
//	                 必ず最後。よって restart 指示は成功時のみ返り、Ack はその
//	                 **前**に打つ（exit で監査が消えないための既存規律と同じ）
//
// 破壊的命令（restart-agent / self-update 成功時）は **Ack を先行**して
// から実行する（cm 規律: kickstart -k / exit で agent が死んでも監査が
// done で残る。後 Ack だと永遠に running のまま滞留する）。
// 不能命令・未知 sid は status=error で Ack＝pending を滞留させない。
package commands

import (
	"context"
	"fmt"
	"time"

	"github.com/4noha/drover-cloud/state"
)

// StateClient は CommandRunner が使うクラウド状態の契約。master の
// *state.Client も slave の relay 経由クライアント（cmd/herdr-drover の
// *relayState）も満たす（agentState のサブセット＝メソッド 3 本のみ）。
// commands は internal パッケージで cmd/herdr-drover（package main）を import
// できないため、必要メソッドだけをここに宣言してインターフェース越しに受ける。
type StateClient interface {
	WatchCommands(ctx context.Context, fn func(state.Command)) error
	AckCommand(ctx context.Context, id, status, detail string) error
	IsSelfRevoked(ctx context.Context) bool
}

type CommandRunner struct {
	St      StateClient
	Revoked func(context.Context) bool // 既定 St.IsSelfRevoked（多層防御の再検査）
	// DoRestart は launchd 配下の自分を kickstart -k（自己 kill を含むため
	// 戻らないことがある＝Ack 先行が前提）。
	DoRestart func(context.Context) error
	// DoUpdate は selfupdate.Update（戻り値: tag, 更新有無, err）。
	DoUpdate func(context.Context) (tag string, updated bool, err error)
	// DoExit は self-update 成功後の再起動手段（既定配線は os.Exit(0)＝
	// launchd KeepAlive が新バイナリで再起動。cm は monitor/cloud 2 デーモン
	// を kickstart したが、drover は単一 agent なので exit が最短・確実）。
	DoExit func()
	// DoProxy は当該 sid の bridge respawn（webterm。Web ターミナル無効=
	// CLOUD_RELAY_URL 未設定なら nil のまま＝未配線 error で Ack）。
	DoProxy func(ctx context.Context, sid string) error
	// DoRestartClaude は claude セッションの作り直し（restartclaude.go）。
	// sid 空＝その PC のローカル claude pane 全部。戻り値の要約が Ack detail に
	// 載る（何件やって何件 skip したかを監査に残す）。
	DoRestartClaude func(ctx context.Context, sid, agent string) (summary string, err error)
	// DoUpdateClaude は claude 本体の更新＋セッション反映（updateclaude.go）。
	// ダウンロードを含むので DoRestartClaude より長くかかる（agent 側で上限あり）。
	DoUpdateClaude func(ctx context.Context, sid, agent string) (summary string, err error)
	// DoUpdateAll は update-claude＋self-update を逐次実行する（update-all）。
	// restart=true なら呼び手（handle）が Ack 後に DoExit する＝launchd が新
	// バイナリで再起動。err 時は restart=false（原因調査のため生かす）。
	DoUpdateAll func(ctx context.Context) (summary string, restart bool, err error)
	// DoReadPane は画面テキスト取得（pane.read）＋結果の mcp_results 保存までを
	// 一体で行う（保存先の選び方は呼び手の責務。slave 等で保存できない場合は
	// err を返す。agent 自身は死なないので Ack は実行後＝戻り値 summary が
	// detail に載る。drover-cloud-mcp-design.md Phase 2）。
	DoReadPane func(ctx context.Context, id, sid string, args map[string]string) (summary string, err error)
	// DoSendInput はテキスト/キー入力（pane.send_text/send_keys）。
	// agent_status ゲート・text/keys 排他の判定は実装側（cmd/herdr-drover）の
	// 責務＝ここは args をそのまま渡すだけ（画面 I/O の意味を知らない）。
	DoSendInput func(ctx context.Context, sid string, args map[string]string) (summary string, err error)
	// DoInterrupt は Ctrl-C 相当（pane.send_keys C-c）。
	DoInterrupt func(ctx context.Context, sid string) (summary string, err error)
}

// checkExpiry は Args["expires_at"]（RFC3339Nano）を検証する。send-input /
// interrupt は**必須**（スリープ中に滞留した命令が復帰後に突然発火する事故を
// 防ぐ。drover-cloud-mcp-design.md §3.3）。この2命令は Phase 2 で新設された
// ばかりで既存呼び手が無いため、後方互換の考慮なく必須化できる。
func checkExpiry(args map[string]string) error {
	exp := args["expires_at"]
	if exp == "" {
		return fmt.Errorf("expires_at が必須（スリープ復帰後の遅延実行事故を防ぐため）")
	}
	t, err := time.Parse(time.RFC3339Nano, exp)
	if err != nil {
		return fmt.Errorf("expires_at の形式が不正: %q", exp)
	}
	if time.Now().After(t) {
		return fmt.Errorf("expired: 期限切れ（%s）", exp)
	}
	return nil
}

// Run は命令制御線（WatchCommands）を回す。claim 済命令のみ handle
// （claim は state 側 transaction＝Snapshot 再配信や複数 agent でも
// 二重実行しない）。
func (cr *CommandRunner) Run(ctx context.Context) error {
	return cr.St.WatchCommands(ctx, func(cm state.Command) {
		cr.handle(ctx, cm)
	})
}

func (cr *CommandRunner) handle(ctx context.Context, cm state.Command) {
	revoked := cr.Revoked
	if revoked == nil {
		revoked = cr.St.IsSelfRevoked
	}
	if revoked(ctx) {
		_ = cr.St.AckCommand(ctx, cm.ID, "error", "revoked: 実行拒否")
		return
	}

	// 旧命令名は新名へ写像する（Cloud 側の allowlist には旧名も残っているので、
	// まだ更新していない Web/CLI から旧名が飛んでくる）。旧名は claude 専用
	// だったので agent を "claude" に固定する＝推測ではなく事実。
	cmd, agent, legacy := state.NormalizeCommand(cm.Cmd, cm.Agent)
	legacyNote := ""
	if legacy {
		// どう解釈したかを必ず監査に残す（黙って読み替えない）。
		legacyNote = fmt.Sprintf("（旧名 %s を %s/agent=%q として解釈）", cm.Cmd, cmd, agent)
	}

	switch cmd {
	case "restart-daemon":
		if cr.DoRestart == nil {
			_ = cr.St.AckCommand(ctx, cm.ID, "error", "restart 未配線")
			return
		}
		// Ack 先行（kickstart -k は自己を kill し戻らない）
		_ = cr.St.AckCommand(ctx, cm.ID, "done", "launchd 再起動（kickstart -k）を実行")
		_ = cr.DoRestart(ctx)

	case "self-update":
		if cr.DoUpdate == nil {
			_ = cr.St.AckCommand(ctx, cm.ID, "error", "update 未配線")
			return
		}
		tag, up, err := cr.DoUpdate(ctx)
		if err != nil {
			_ = cr.St.AckCommand(ctx, cm.ID, "error", "更新失敗: "+err.Error())
			return
		}
		if !up {
			_ = cr.St.AckCommand(ctx, cm.ID, "done", "既に最新: "+tag)
			return
		}
		// 更新成功→exit で launchd が新バイナリを読む。exit 前に Ack。
		_ = cr.St.AckCommand(ctx, cm.ID, "done", "更新 "+tag+"→再起動（exit）を実行")
		if cr.DoExit != nil {
			cr.DoExit()
		}

	case "restart-proxy":
		if cr.DoProxy == nil {
			_ = cr.St.AckCommand(ctx, cm.ID, "error",
				"restart-proxy 未配線（CLOUD_RELAY_URL 未設定＝Web ターミナル無効）")
			return
		}
		if err := cr.DoProxy(ctx, cm.SID); err != nil {
			_ = cr.St.AckCommand(ctx, cm.ID, "error",
				"bridge respawn 失敗: "+err.Error())
			return
		}
		_ = cr.St.AckCommand(ctx, cm.ID, "done", "bridge respawn: "+cm.SID)

	case "restart-agent-session":
		if cr.DoRestartClaude == nil {
			_ = cr.St.AckCommand(ctx, cm.ID, "error", "restart-agent-session 未配線")
			return
		}
		// agent 自身は死なない＝Ack 先行にせず、実行結果の要約を detail に残す
		// （何件再起動・何件 skip したかが監査で追える）。
		summary, err := cr.DoRestartClaude(ctx, cm.SID, agent)
		if err != nil {
			_ = cr.St.AckCommand(ctx, cm.ID, "error", "セッション再起動 失敗: "+err.Error()+legacyNote)
			return
		}
		_ = cr.St.AckCommand(ctx, cm.ID, "done", summary+legacyNote)

	case "update-all":
		if cr.DoUpdateAll == nil {
			_ = cr.St.AckCommand(ctx, cm.ID, "error", "update-all 未配線")
			return
		}
		// 段の途中で失敗しても、そこまでの結果を detail に残す（どこで止まったかを
		// 監査から読めるようにする＝silent 失敗にしない）。
		summary, restart, err := cr.DoUpdateAll(ctx)
		if err != nil {
			_ = cr.St.AckCommand(ctx, cm.ID, "error", summary+" / 失敗: "+err.Error())
			return
		}
		// Ack 先行（この後の DoExit でプロセスが死ぬ＝後 Ack だと running 滞留）。
		_ = cr.St.AckCommand(ctx, cm.ID, "done", summary+" → 再起動（exit）を実行")
		if restart && cr.DoExit != nil {
			cr.DoExit()
		}

	case "update-agent-cli":
		if cr.DoUpdateClaude == nil {
			_ = cr.St.AckCommand(ctx, cm.ID, "error", "update-agent-cli 未配線")
			return
		}
		// restart-claude と同じく agent 自身は死なない＝Ack は実行後。版の
		// 前後と再起動結果の要約を detail に残す。
		summary, err := cr.DoUpdateClaude(ctx, cm.SID, agent)
		if err != nil {
			_ = cr.St.AckCommand(ctx, cm.ID, "error", "エージェント更新 失敗: "+err.Error()+legacyNote)
			return
		}
		_ = cr.St.AckCommand(ctx, cm.ID, "done", summary+legacyNote)

	case "read-pane":
		if cr.DoReadPane == nil {
			_ = cr.St.AckCommand(ctx, cm.ID, "error", "read-pane 未配線")
			return
		}
		summary, err := cr.DoReadPane(ctx, cm.ID, cm.SID, cm.Args)
		if err != nil {
			_ = cr.St.AckCommand(ctx, cm.ID, "error", "画面取得失敗: "+err.Error())
			return
		}
		_ = cr.St.AckCommand(ctx, cm.ID, "done", summary)

	case "send-input":
		if cr.DoSendInput == nil {
			_ = cr.St.AckCommand(ctx, cm.ID, "error", "send-input 未配線")
			return
		}
		if err := checkExpiry(cm.Args); err != nil {
			_ = cr.St.AckCommand(ctx, cm.ID, "error", err.Error())
			return
		}
		summary, err := cr.DoSendInput(ctx, cm.SID, cm.Args)
		if err != nil {
			_ = cr.St.AckCommand(ctx, cm.ID, "error", "入力失敗: "+err.Error())
			return
		}
		_ = cr.St.AckCommand(ctx, cm.ID, "done", summary)

	case "interrupt":
		if cr.DoInterrupt == nil {
			_ = cr.St.AckCommand(ctx, cm.ID, "error", "interrupt 未配線")
			return
		}
		if err := checkExpiry(cm.Args); err != nil {
			_ = cr.St.AckCommand(ctx, cm.ID, "error", err.Error())
			return
		}
		summary, err := cr.DoInterrupt(ctx, cm.SID)
		if err != nil {
			_ = cr.St.AckCommand(ctx, cm.ID, "error", "中断失敗: "+err.Error())
			return
		}
		_ = cr.St.AckCommand(ctx, cm.ID, "done", summary)

	default:
		// PushCommand 側 allowlist を通らない値がここへ来るのは
		// Firestore 直書き等の異常系＝黙殺せず error で監査に残す。
		_ = cr.St.AckCommand(ctx, cm.ID, "error", "未知のコマンド: "+cm.Cmd)
	}
}
