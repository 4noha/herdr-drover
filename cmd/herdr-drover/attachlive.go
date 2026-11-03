// attachlive.go — ↗窓（attach）の relay 接続の生存確認。
//
// 無通信自切断（DefaultIdle）を 30s から 15 分へ伸ばした（2026-11-03）ことで、
// 黙って死んだ TCP（relay VM の再起動・NAT テーブル消失・経路変更）を最長 15 分
// 「接続中」と誤認する余地ができた。スリープ復帰と NIC 変化は watchLifecycle が
// 拾うが、自分側に変化が無い死に方（相手側の消失）はそれでは分からない。
// そこで接続ごとに定期 WS ping を送り、pong が期限内に無ければ自分から閉じる
// （attachOnce が戻り backoff 再接続へ。idleClosed=false・サイクル >5s なら backoff は
// リセットされ即再接続）。pong は pumpFrames の読み手が受ける（coder/websocket）。
package main

import (
	"context"
	"time"
)

const (
	// viewerPingInterval は生存確認の間隔。relay が死んでから気付くまでの上限が
	// これ＋viewerPingTimeout。30s より短くしても得るものが無い（frame が流れて
	// いる間は ping が無くても生きていると分かる）。
	viewerPingInterval = 60 * time.Second
	// viewerPingTimeout は pong を待つ上限。正常時の RTT（数十 ms〜）に十分な余裕。
	viewerPingTimeout = 10 * time.Second
)

// pinger は WS ping を持つ conn（drover-cloud relayclient.Conn）。net.Conn からの
// 型アサートで取る＝fake conn でもテストできる seam。
type pinger interface {
	Ping(ctx context.Context) error
}

// watchLiveness は ctx が終わるまで interval ごとに ping を送り、timeout 内に pong が
// 無ければ onFail(err) を 1 回呼んで戻る。ping できない conn なら何もしない（不明は
// 生きている扱い＝壊さない側に倒す）。ctx 終了による失敗は報告しない。
func watchLiveness(ctx context.Context, conn any, interval, timeout time.Duration, onFail func(error)) {
	p, ok := conn.(pinger)
	if !ok {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pctx, cancel := context.WithTimeout(ctx, timeout)
			err := p.Ping(pctx)
			cancel()
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				onFail(err)
				return
			}
		}
	}
}
