package herdrapi

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"time"
)

// Subscribe の再接続 backoff（切断→初回 0.5s、以後倍々で上限 30s。
// 購読確立に成功したらリセット）。cm の M9 reconnect gate と同じ規律。
const (
	subscribeBackoffMin = 500 * time.Millisecond
	subscribeBackoffMax = 30 * time.Second
)

// Subscribe は events.subscribe の長寿命購読を張り、受けた event を ch へ
// 流し続ける。ctx が終わるまで戻らない（戻り値は ctx.Err()）。切断
// （サーバ再起動含む）は backoff 付きで自動再購読する。購読名が不正等の
// APIError は再試行しても無駄なので即 error で戻る。
//
// events は dot 形の購読名（"pane.created" 等。不正名はサーバがエラー応答で
// 全列挙を返す）。配信される Event.Name は underscore 形（"pane_created"）
// ＝命名の非対称に注意（types.go Event 参照）。
//
// ⚠受信側の必須規律（実測に基づく）:
//   - **herdr は新規購読のたびに、当該サーバ稼働中に起きた過去 event の
//     バックログを再送する**（実測: 同一 pane_created/pane_updated が購読の
//     たびに再着。サーバ起動時の session 復元で生えた pane は再送に現れ
//     ない）。再接続でも同じ＝event は「差分の権威」ではなく nudge として
//     扱い、pane.list との突合せ・dedupe を呼び手が行うこと
//     （DESIGN: 周期 poll backstop 常設）。
//   - 接続維持性（実測・v0.7.4・本 Mac）: keepalive/ping の類は一切流れず、
//     **完全無通信 384 秒（6 分 24 秒）放置後も同一接続のまま後続 event が
//     即配信された**（隔離サーバ・AF_UNIX。サーバ側 idle 切断は少なくとも
//     この時間軸では無し）。とはいえサーバ再起動では当然切れるので、
//     backoff 再購読＋周期 poll backstop（DESIGN）を常設する。
//
// ch への送出は ctx とのみ select する＝ch が詰まると読み取りが止まる。
// 呼び手は十分なバッファを持つか消費を止めないこと。ch は close しない
// （所有権は呼び手）。
func (c *Client) Subscribe(ctx context.Context, events []string, ch chan<- Event) error {
	backoff := subscribeBackoffMin
	for {
		established, err := c.subscribeOnce(ctx, events, ch)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			if apiErr, ok := err.(*APIError); ok {
				// 購読名不正等は永続エラー＝リトライ無意味（invalid_request
				// が variant 全列挙を返すので原因はメッセージで即分かる）。
				return apiErr
			}
			// dial 失敗・途中切断は backoff 再購読へ。
		}
		if established {
			backoff = subscribeBackoffMin
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > subscribeBackoffMax {
			backoff = subscribeBackoffMax
		}
	}
}

// subscribeOnce は 1 本の購読接続を張り、切断まで event を ch へ流す。
// established は subscription_started 受領まで到達したか（backoff リセット
// 判定用）。
func (c *Client) subscribeOnce(ctx context.Context, events []string, ch chan<- Event) (established bool, err error) {
	type subscription struct {
		Type string `json:"type"`
	}
	subs := make([]subscription, 0, len(events))
	for _, e := range events {
		subs = append(subs, subscription{Type: e})
	}

	conn, err := net.DialTimeout("unix", c.SocketPath, defaultDialTimeout)
	if err != nil {
		return false, fmt.Errorf("herdr dial %s: %w", c.SocketPath, err)
	}
	defer conn.Close()
	// ctx 終了で blocking Read を確実に解く（deadline ではなく Close＝
	// cm relay の「conn 毎に読み手 1 つ」規律と同じく、この goroutine が
	// 唯一の reader のまま外から破る）。
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	id := fmt.Sprintf("%d", c.seq.Add(1))
	req, err := json.Marshal(request{
		ID:     id,
		Method: "events.subscribe",
		Params: struct {
			Subscriptions []subscription `json:"subscriptions"`
		}{subs},
	})
	if err != nil {
		return false, err
	}
	// 購読確立（1 行目の応答）までは通常 Call と同じ短い期限を課す。
	if err := conn.SetDeadline(time.Now().Add(defaultCallTimeout)); err != nil {
		return false, err
	}
	if _, err := conn.Write(append(req, '\n')); err != nil {
		return false, fmt.Errorf("herdr subscribe write: %w", err)
	}

	r := bufio.NewReader(conn)
	line, err := r.ReadBytes('\n')
	if err != nil {
		return false, fmt.Errorf("herdr subscribe read: %w", err)
	}
	var resp response
	if err := json.Unmarshal(line, &resp); err != nil {
		return false, fmt.Errorf("herdr subscribe decode: %w (line=%.200s)", err, line)
	}
	if resp.Error != nil {
		return false, resp.Error
	}
	// 実採取: {"id":"...","result":{"type":"subscription_started"}}
	var started struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(resp.Result, &started); err != nil || started.Type != "subscription_started" {
		return false, fmt.Errorf("herdr subscribe: unexpected result %.200s", resp.Result)
	}

	// 以降は長寿命: 期限を外して event 行を読み続ける（idle keepalive は
	// 流れない実測＝read deadline を置くと idle で誤切断する）。
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return true, err
	}
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			// ctx 起因の Close も EOF 系で出る（呼び手が ctx.Err() で判別）。
			return true, fmt.Errorf("herdr subscribe stream: %w", err)
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			return true, fmt.Errorf("herdr subscribe event decode: %w (line=%.200s)", err, line)
		}
		select {
		case <-ctx.Done():
			return true, ctx.Err()
		case ch <- ev:
		}
	}
}
