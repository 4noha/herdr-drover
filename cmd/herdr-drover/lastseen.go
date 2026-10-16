package main

// pcs/{pc}.last_seen の定期書き込み（MCP の list_machines が online 判定の
// 土台にする。drover-cloud-mcp-design.md §9 決定事項5）。既存 producer tick
// には便乗せず別タイマー（間隔は tick と無関係に粗めで十分）。master のみ
// （slave の online 判定は未対応・known gap＝TODO.md 参照）。

import (
	"context"
	"log"
	"time"
)

// lastSeenInterval は書き込み間隔（決定事項5「2〜5分」の中間値）。
// state.pcOnlineWindow（10分）はこの倍以上を見ている＝1回取りこぼしても
// 誤って offline にしない。
const lastSeenInterval = 3 * time.Minute

// runLastSeenHeartbeat は touch を起動直後に 1 回、以後 interval 毎に呼ぶ
// （ctx 終了で戻る）。touch のエラーはログのみ（次周期で自然に再試行する＝
// 単発の Firestore 不調で daemon を落とさない）。
func runLastSeenHeartbeat(ctx context.Context, interval time.Duration, touch func(context.Context) error, lg *log.Logger) {
	do := func() {
		if err := touch(ctx); err != nil && ctx.Err() == nil {
			lg.Printf("last_seen 更新失敗（次周期で再試行）: %v", err)
		}
	}
	do()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			do()
		}
	}
}
