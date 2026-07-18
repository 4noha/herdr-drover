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
//	restart-agent → launchctl kickstart -k（自己。DoRestart seam）
//	self-update   → selfupdate.Update → DoExit（os.Exit(0)。launchd
//	                KeepAlive が新バイナリで再起動）
//	restart-proxy → 当該 sid の bridge respawn（webterm。DoProxy seam）
//
// 破壊的命令（restart-agent / self-update 成功時）は **Ack を先行**して
// から実行する（cm 規律: kickstart -k / exit で agent が死んでも監査が
// done で残る。後 Ack だと永遠に running のまま滞留する）。
// 不能命令・未知 sid は status=error で Ack＝pending を滞留させない。
package commands

import (
	"context"

	"github.com/4noha/drover-cloud/state"
)

type CommandRunner struct {
	St      *state.Client
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

	switch cm.Cmd {
	case "restart-agent":
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

	default:
		// PushCommand 側 allowlist を通らない値がここへ来るのは
		// Firestore 直書き等の異常系＝黙殺せず error で監査に残す。
		_ = cr.St.AckCommand(ctx, cm.ID, "error", "未知のコマンド: "+cm.Cmd)
	}
}
