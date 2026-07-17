// Package relayclient は cm（claude-master-go）の relay クライアント側
// （source 役の dial と idle 付き双方向ポンプ）のコピー/適応。
// cm internal/cloud/relay/relay.go の Dial / pump / idlePump /
// BridgeSourceIdle が原典（同一作者＝コピー自由・cm リポジトリは無改変）。
//
// ワイヤ契約（cm コード実読の抽出・relay 側は既デプロイ Cloud Run を無改変
// 共有するため、この形式から一切逸脱しない）:
//   - URL = baseURL + "/session?sid=" + sid + "&role=" + role
//     （パス /session 固定・query は sid/role の 2 つだけ・カスタムヘッダ
//     無し・subprotocol 無し・DialOptions{} 既定。sid は URL エスケープ
//     しない＝cm と同じ前提。pane_id の `:` は RFC 3986 で query に生の
//     まま合法）
//   - websocket.NetConn(ctx, c, MessageBinary) でバイトストリーム化＝
//     WS message 境界は意味を持たない（既存 RESIZE/SCROLL/frame protocol
//     を無改変トンネル）
//   - quiescence（無通信 idle）切断は「**両方向とも** idle の時のみ」。
//     どちらか一方向でも流れていればリセット（cm idlePump と同一意味論）
//
// cm との差分は 1 点のみ: BridgeSourceIdle のローカル端が cm では
// `net.Dial("unix", <pid>.sock)`（ptyproxy）だったのに対し、herdr-drover
// には常駐 proxy socket が無いので呼び出し側が用意する io.ReadWriteCloser
// （bridge 側 pipe 端など）を受ける。
package relayclient

import (
	"context"
	"io"
	"net"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// Dial は relay へ WSS 接続して net.Conn（バイトストリーム）を返す。
// baseURL 例: ws://host:port / wss://…（/session は付けない）。
// cm relay.Dial（relay.go:257-266）のバイト同等コピー。
// conn の寿命は ctx に束縛される（cancel で read/write が死ぬ）＝呼び出し側
// は接続生存期間の ctx を渡すこと。
func Dial(ctx context.Context, baseURL, sid, role string) (net.Conn, error) {
	u := baseURL + "/session?sid=" + sid + "&role=" + role
	c, _, err := websocket.Dial(ctx, u, &websocket.DialOptions{})
	if err != nil {
		return nil, err
	}
	return websocket.NetConn(ctx, c, websocket.MessageBinary), nil
}

// DialSource は source 役の Dial（webterm 配線の dialRelay 委譲先）。
func DialSource(ctx context.Context, baseURL, sid string) (net.Conn, error) {
	return Dial(ctx, baseURL, sid, "source")
}

// BridgeSourceIdle は relay へ source として dial し、local と双方向ポンプ
// する。idle 秒 無通信（両方向とも）でデータ線を閉じて戻る（quiescence
// 切断＝次の wake まで解放）。cm relay.BridgeSourceIdle の適応コピー
// （unix socket dial の代わりに呼び出し側の local 端を使う）。
// ctx 終了／どちらか切断でも戻る。戻る時には両端とも close 済み。
func BridgeSourceIdle(ctx context.Context, baseURL, sid string, local io.ReadWriteCloser, idle time.Duration) error {
	ws, err := Dial(ctx, baseURL, sid, "source")
	if err != nil {
		return err
	}
	defer ws.Close()
	defer local.Close()
	idlePump(ws, local, idle)
	return nil
}

// pump は a⇄b をバイト透過で双方向中継。片方が閉じたら戻る。
// cm relay.pump のコピー（型だけ io.ReadWriteCloser へ一般化）。
func pump(a, b io.ReadWriteCloser) {
	d := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(a, b); d <- struct{}{} }()
	go func() { _, _ = io.Copy(b, a); d <- struct{}{} }()
	<-d
}

// idlePump は a⇄b を透過中継しつつ、両方向で idle 秒バイトが流れなければ
// 両端を閉じて戻る（= データ線の quiescence 切断）。idle<=0 なら通常 pump
// と同じ（無期限）。cm relay.idlePump のコピー（32KB バッファ・どちら向き
// でも read で bump・ticker(idle/2) 判定・read/write エラーでも両端 close、
// 全て同一）。
func idlePump(a, b io.ReadWriteCloser, idle time.Duration) {
	if idle <= 0 {
		pump(a, b)
		return
	}
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	bump := func() { last.Store(time.Now().UnixNano()) }
	d := make(chan struct{}, 2)
	cp := func(dst, src io.ReadWriteCloser) {
		buf := make([]byte, 32*1024)
		for {
			n, err := src.Read(buf)
			if n > 0 {
				bump()
				if _, werr := dst.Write(buf[:n]); werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		d <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	tick := time.NewTicker(idle / 2)
	defer tick.Stop()
	for {
		select {
		case <-d:
			a.Close()
			b.Close()
			return
		case <-tick.C:
			if time.Since(time.Unix(0, last.Load())) >= idle {
				a.Close() // 静止 → データ線解放
				b.Close()
				<-d
				return
			}
		}
	}
}
