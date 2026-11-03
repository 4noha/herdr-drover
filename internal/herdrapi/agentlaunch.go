package herdrapi

// agent の起動と命名（herdr 0.9.x 対応・BUG-13）。
//
// 何が変わったか（2026-10-25 に隔離した実 herdr 0.9.5 で実測）:
//
//	0.7.4: agent.start{name,argv} が「新 pane で argv を起動し name を付ける」を
//	       1 回で行えた。layout.apply で作った pane も生成と同時に agent として
//	       確定し、直後の agent.rename が通った。
//	0.9.5: agent.start は「**既存の対話シェル pane** に kind の正規実行体を起動する」
//	       別 API に変わった（必須: name・kind・pane_id。旧形は
//	       `missing field kind`）。agent の検出も**前景プロセス名の観測に基づく
//	       非同期処理**になり、生成直後の agent.rename は `agent_not_found`。
//
// 実測（本物の ~/.local/bin/claude を layout.apply で起動）:
//
//	t+0.00s rename→ agent_not_found
//	t+0.76s rename→ OK        ← 検出に約 0.8 秒
//
// ⚠検出は**前景プロセスの名前**で決まる（CLAUDE.md の実測 trap と同じ原理）。
// `exec sleep` のように前景を別名のバイナリへ置換する stub は、いつまで待っても
// 検出されない。これは遅延ではなく「agent ではない」だけなので、
// RenameWithDetectWait は timeout で loud に諦める（無限に待たない）。

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const (
	// AgentDetectTimeout は「pane 生成 → herdr が agent を検出」を待つ上限。
	// 実測 0.9.5 は約 0.8 秒。claude の起動が遅い環境を見て 15 秒。
	AgentDetectTimeout = 15 * time.Second
	// AgentDetectPoll は検出待ちの再試行間隔。
	AgentDetectPoll = 250 * time.Millisecond
)

// AgentRename は agent.rename を 1 回呼ぶ（target は pane_id）。応答は
// {"type":"agent_info","agent":{...AgentInfo...}}。0.9.x では検出前の pane に
// 対して `agent_not_found` を返すので、生成直後は RenameWithDetectWait を使う。
func (c *Client) AgentRename(paneID, name string) (json.RawMessage, error) {
	return c.Call("agent.rename", struct {
		Target string `json:"target"`
		Name   string `json:"name"`
	}{paneID, name})
}

// RenameWithDetectWait は call（通常は (*Client).AgentRename）を呼び、
// `agent_not_found`（= まだ agent と検出されていない）の間は timeout まで poll
// 間隔でリトライする。
//
//   - 待つのは `agent_not_found` の exact-match のみ（鉄則③）。他のエラーは即返す
//     （agent_name_taken は呼び手が次の採番へ進む契約なので素通し）
//   - 戻り値 waited は検出待ちに費やした時間（0.7.4 なら初回で通り 0 付近）。
//     呼び手は必要ならログに出す（silent にしない）
//   - timeout 内に検出されなければ最後の `agent_not_found` を包んで返す
//     （errors.As で APIError のコードを引き続き読める）
func RenameWithDetectWait(call func(paneID, name string) (json.RawMessage, error),
	paneID, name string, timeout, poll time.Duration) (raw json.RawMessage, waited time.Duration, err error) {

	start := time.Now()
	for {
		raw, err = call(paneID, name)
		if err == nil {
			return raw, time.Since(start), nil
		}
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Code != "agent_not_found" {
			return nil, time.Since(start), err
		}
		if time.Since(start)+poll > timeout {
			return nil, time.Since(start),
				fmt.Errorf("agent が %s 以内に検出されなかった（pane %s）: %w",
					timeout.Truncate(time.Millisecond), paneID, err)
		}
		time.Sleep(poll)
	}
}

// LaunchAgentOptions は LaunchAgent の任意パラメータ。
type LaunchAgentOptions struct {
	// Cwd は起動 pane の cwd（layout.apply の pane.cwd）。
	Cwd string
	// WorkspaceID は置き先の workspace（指定時はその workspace の**新しい Tab** に置く）。
	// 空なら**新しい workspace を作り、その root tab を agent pane 1 枚に置き換える**
	// （workspace.create が作る素シェル pane を余らせない＝旧 agent.start と同じく
	// 「agent だけが居る」状態になる）。layout.apply は workspace 未指定かつ
	// workspace ゼロの server で `workspace_not_found: no active workspace` になる
	// 実測＝推測で既存に相乗りさせず、置き先を決定的にする。
	WorkspaceID string
}

// LaunchAgent は「新しい Tab に argv を直接起動し、agent 名 name を付ける」。
// 旧 agent.start{name,argv}（0.9.x で廃止）が担っていた操作を、0.9.x の正規手段
// （layout.apply＋検出待ちつき agent.rename）で行う。argv は herdr が agent と
// 検出できる実行体であること（前景プロセス名が lookup_agent の表にある名前）。
func (c *Client) LaunchAgent(name string, argv []string, opts *LaunchAgentOptions) (*AgentInfo, error) {
	var o LaunchAgentOptions
	if opts != nil {
		o = *opts
	}
	ws, tab := o.WorkspaceID, ""
	if ws == "" {
		created, err := c.WorkspaceCreate()
		if err != nil {
			return nil, fmt.Errorf("workspace.create: %w", err)
		}
		// root tab を置き換える＝素シェル pane は layout.apply が閉じる。
		ws, tab = created.Workspace.WorkspaceID, created.Tab.TabID
	}

	type paneNode struct {
		Type    string   `json:"type"`
		Cwd     string   `json:"cwd,omitempty"`
		Command []string `json:"command"`
	}
	var applied struct {
		Layout struct {
			Root struct {
				PaneID string `json:"pane_id"`
			} `json:"root"`
		} `json:"layout"`
	}
	// tab_id と workspace_id は排他（両方送ると invalid_target: use either tab_id
	// or workspace_id, not both・実測）＝置き換え時は tab_id だけ、新 Tab 時は
	// workspace_id だけを送る。
	params := struct {
		WorkspaceID string   `json:"workspace_id,omitempty"`
		TabID       string   `json:"tab_id,omitempty"`
		Focus       bool     `json:"focus"`
		Root        paneNode `json:"root"`
	}{Focus: false, Root: paneNode{Type: "pane", Cwd: o.Cwd, Command: argv}}
	if tab != "" {
		params.TabID = tab
	} else {
		params.WorkspaceID = ws
	}
	if err := c.call("layout.apply", params, &applied); err != nil {
		return nil, fmt.Errorf("layout.apply: %w", err)
	}
	pane := applied.Layout.Root.PaneID
	if pane == "" {
		return nil, fmt.Errorf("layout.apply 応答に root pane_id が無い（wire 変化?）")
	}

	raw, _, err := RenameWithDetectWait(c.AgentRename, pane, name, AgentDetectTimeout, AgentDetectPoll)
	if err != nil {
		// pane は生成済み＝pane_id を残す（黙って孤児にしない）。
		return nil, fmt.Errorf("agent.rename %s→%s: %w", pane, name, err)
	}
	var out struct {
		Agent AgentInfo `json:"agent"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("agent_info decode: %w", err)
	}
	return &out.Agent, nil
}
