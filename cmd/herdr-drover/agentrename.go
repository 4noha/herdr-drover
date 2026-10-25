package main

// agent.rename の「検出待ち」リトライ（BUG-13・herdr 0.9.x 対応）。
//
// 何が変わったか（2026-10-25 に隔離した実 herdr 0.9.5 で実測）:
//
//	0.7.4: layout.apply で argv 直接実行 pane を作ると、**生成と同時に agent が
//	       検出済み**。その直後の agent.rename が通っていた。
//	0.9.5: agent の検出が**前景プロセス名の観測に基づく非同期処理**になった。
//	       layout.apply 直後は agent.list が 0 件で agent.rename は
//	       `agent_not_found: agent target <pane> not found`。
//
// 実測（本物の ~/.local/bin/claude を layout.apply で起動）:
//
//	t+0.00s rename→ agent_not_found
//	t+0.76s rename→ OK        ← 検出に約 0.8 秒
//
// ⚠検出は**前景プロセスの名前**で決まる（CLAUDE.md の実測 trap と同じ原理）。
// `exec sleep` のように前景を別名のバイナリへ置換する stub は、いつまで待っても
// 検出されない（未検出のまま）。これは遅延ではなく「agent ではない」だけなので、
// 本リトライは timeout で loud に諦める（無限に待たない）。
//
// rename 自体の形（{target,name}）は不変で、変わったのは「命名できる時点」だけ。
// よって API を作り直さず、`agent_not_found` を**検出待ち**として扱いリトライする。
// これを入れないと 0.9.x では claude シムも restart-agent-session / update-all も
// 起動直後に `agent.rename ... agent_not_found` で必ず失敗する。
//
// 方針:
//   - 待つのは **`agent_not_found` の exact-match のみ**（鉄則③。他のエラーは
//     従来どおり即返す。agent_name_taken は呼び手が次の採番へ進むので素通し）
//   - 上限時間つき（検出されない＝そもそも agent ではない pane を永遠に待たない）
//   - 待った事実は呼び手が追えるよう**回数・秒数を返す**（silent にしない）

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/4noha/herdr-drover/internal/herdrapi"
)

const (
	// agentDetectTimeout は「pane 生成 → herdr が agent を検出」を待つ上限。
	// 実測 0.9.5 は約 1.5 秒。余裕を見て 15 秒（claude の起動が遅い環境を想定）。
	agentDetectTimeout = 15 * time.Second
	// agentDetectPoll は検出待ちの再試行間隔。
	agentDetectPoll = 250 * time.Millisecond
)

// renameCaller は agent.rename を 1 回呼ぶ seam（テストで herdr を差し替える）。
type renameCaller func(paneID, name string) (json.RawMessage, error)

// renameWithDetectWait は agent.rename を呼び、`agent_not_found`（= まだ agent と
// 検出されていない）の間は timeout まで poll 間隔でリトライする。
//
// 戻り値の waited は検出待ちに費やした時間（0 なら初回で通った＝0.7.4 相当）。
// timeout 内に検出されなければ最後の `agent_not_found` をそのまま返す。
func renameWithDetectWait(call renameCaller, paneID, name string,
	timeout, poll time.Duration) (raw json.RawMessage, waited time.Duration, err error) {

	start := time.Now()
	for {
		raw, err = call(paneID, name)
		if err == nil {
			return raw, time.Since(start), nil
		}
		var apiErr *herdrapi.APIError
		if !errors.As(err, &apiErr) || apiErr.Code != "agent_not_found" {
			return nil, time.Since(start), err // 別種のエラーは待たない
		}
		if time.Since(start)+poll > timeout {
			return nil, time.Since(start),
				fmt.Errorf("agent が %s 以内に検出されなかった（pane %s）: %w",
					timeout.Truncate(time.Millisecond), paneID, err)
		}
		time.Sleep(poll)
	}
}

// agentRenameCall は herdr の agent.rename を叩く本番の renameCaller。
func agentRenameCall(api *herdrapi.Client) renameCaller {
	return func(paneID, name string) (json.RawMessage, error) {
		return api.Call("agent.rename", struct {
			Target string `json:"target"`
			Name   string `json:"name"`
		}{paneID, name})
	}
}
