package main

// agentState は agent が使うクラウド状態クライアントの契約。master は
// 具象 *state.Client（Firestore 直結・byte 同一）、slave は relay 経由の
// *relayState（SA レス・/slave/* HTTP）が満たす。この 1 本のインターフェース
// で webterm / commands / producer の各 seam を共有し、master 経路は
// *state.Client の直渡し＝挙動完全不変にする（DESIGN_SLAVE_SPEC §4.1）。
//
// メソッド集合は *state.Client の既存メソッドと**署名まで exact-match**＝
// state パッケージは一切改変せずに master がこのインターフェースを満たす
// （下の compile-time 表明が回帰検知）。

import (
	"context"
	"time"

	"github.com/4noha/drover-cloud/state"
)

type agentState interface {
	Close() error
	IsSelfRevoked(ctx context.Context) bool
	RegisterPCVersion(ctx context.Context, agentVersion string) error

	// producer seam（session.StateClient のスーパーセット）。
	PushStatus(ctx context.Context, sessions []map[string]any) (int, error)
	DeleteSession(ctx context.Context, key string) error
	OwnSessionKeys(ctx context.Context) ([]string, error)

	// webterm seam。
	WatchWake(ctx context.Context, cb func(sid string)) error
	PutRelayGrant(ctx context.Context, sid, role string, ttl time.Duration) error

	// commands seam（P4/optional。slave では未配線 no-op）。
	WatchCommands(ctx context.Context, fn func(state.Command)) error
	AckCommand(ctx context.Context, id, status, detail string) error
}

// compile-time 表明: master は *state.Client（state 無改変）、slave は
// *relayState が agentState を満たす。署名がずれたらここでビルドが落ちる。
var (
	_ agentState = (*state.Client)(nil)
	_ agentState = (*relayState)(nil)
)
