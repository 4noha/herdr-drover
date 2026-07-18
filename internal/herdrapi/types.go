// Package herdrapi は herdr の ndjson API socket（HERDR_SOCKET_PATH →
// 既定 ~/.config/herdr/herdr.sock）への薄いクライアント。
//
// 本パッケージの型・挙動記述は全て実 herdr 0.7.4（protocol 16）の隔離
// サーバへの実リクエストで採取した生応答から写した（推測フィールドなし・
// AGPL 衛生: ソース参照はせず挙動観察のみ）。採取例は各型のコメントに残す。
//
// 通信規約（実測）:
//   - 1 接続 = 1 リクエスト。応答 1 行を書いた後サーバが close する
//     （2 発目は BrokenPipe）＝毎リクエスト再接続する。
//   - リクエストは {"id","method","params"} の 1 行 JSON。params は必須
//     （欠くと invalid_request "missing field `params`"）。
//   - 未知フィールドは無視される（ping に bogus_field を足しても pong が
//     返ることを実測）＝ params 構造体の前方互換は保たれる。
//   - エラーは {"id","error":{"code","message"}}。パースエラー時は id=""
//     で返る（domain エラー時は元の id が echo される）ことを実測。
//   - events.subscribe のみ長寿命接続（events.go 参照）。
package herdrapi

import "encoding/json"

// PongInfo は ping の応答。
// 実採取: {"type":"pong","version":"0.7.4","protocol":16,
//
//	"capabilities":{"live_handoff":true,"detached_server_daemon":false}}
type PongInfo struct {
	Type         string          `json:"type"` // "pong"
	Version      string          `json:"version"`
	Protocol     int             `json:"protocol"`
	Capabilities map[string]bool `json:"capabilities"`
}

// ScrollInfo は PaneInfo.Scroll。
// 実採取: {"offset_from_bottom":0,"max_offset_from_bottom":0,"viewport_rows":21}
type ScrollInfo struct {
	OffsetFromBottom    int `json:"offset_from_bottom"`
	MaxOffsetFromBottom int `json:"max_offset_from_bottom"`
	ViewportRows        int `json:"viewport_rows"`
}

// PaneInfo は pane.list / pane.get / pane 系 event に現れる pane 情報。
// 実採取（pane.get・report_metadata 適用後）:
//
//	{"pane_id":"w3:p1","terminal_id":"term_656c6d34313494","workspace_id":"w3",
//	 "tab_id":"w3:t1","focused":false,"cwd":"/private/tmp",
//	 "foreground_cwd":"/private/tmp","title":"HD-TITLE","agent_status":"unknown",
//	 "tokens":{"sid":"w3:p1","pc":"mac-studio"},
//	 "scroll":{"offset_from_bottom":0,"max_offset_from_bottom":0,"viewport_rows":23},
//	 "revision":1}
//
// label は pane rename 時のみ・title/tokens は pane.report_metadata 適用時のみ
// 現れる optional フィールド（未設定の pane では JSON にキー自体が無い）。
// revision は metadata 更新等でインクリメントされることを実測。
type PaneInfo struct {
	PaneID        string            `json:"pane_id"`     // 例 "w1:p1"。server 再起動を跨いで安定＝セッション key
	TerminalID    string            `json:"terminal_id"` // 例 "term_656c6d0eb4c2f1"。揮発ハンドル
	WorkspaceID   string            `json:"workspace_id"`
	TabID         string            `json:"tab_id"`
	Focused       bool              `json:"focused"`
	Cwd           string            `json:"cwd"`
	ForegroundCwd string            `json:"foreground_cwd"`
	Label         string            `json:"label,omitempty"`
	Title         string            `json:"title,omitempty"`
	AgentStatus   string            `json:"agent_status"` // "unknown"|"idle"|"working"|"blocked"（CLI help の列挙）
	Tokens        map[string]string `json:"tokens,omitempty"`
	Scroll        ScrollInfo        `json:"scroll"`
	AgentSession  AgentSession      `json:"agent_session"`
	Revision      int               `json:"revision"`
}

// AgentSession は herdr が検出したエージェントのセッション識別子。claude では
// `{source:"herdr:claude", agent:"claude", kind:"id", value:<会話 uuid>}` で、
// value が `claude --resume <uuid>` の uuid と一致する（実測・resume backstop の権威）。
type AgentSession struct {
	Source string `json:"source"`
	Agent  string `json:"agent"`
	Kind   string `json:"kind"` // "id"（uuid）｜"path"
	Value  string `json:"value"`
}

// AgentInfo は agent.list / agent.start に現れる agent 情報。
// 実採取（agent.start 応答の agent）:
//
//	{"terminal_id":"term_656c6d7143a445","name":"hdprobe","agent_status":"unknown",
//	 "workspace_id":"w1","tab_id":"w1:t1","pane_id":"w1:p3","focused":false,
//	 "cwd":"/Users/...","foreground_cwd":"/Users/...","revision":0}
type AgentInfo struct {
	TerminalID    string `json:"terminal_id"`
	Name          string `json:"name"`
	AgentStatus   string `json:"agent_status"`
	WorkspaceID   string `json:"workspace_id"`
	TabID         string `json:"tab_id"`
	PaneID        string `json:"pane_id"`
	Focused       bool   `json:"focused"`
	Cwd           string `json:"cwd"`
	ForegroundCwd string `json:"foreground_cwd"`
	Revision      int    `json:"revision"`
}

// PaneReadInfo は pane.read の応答 read。
// 実採取: {"pane_id":"w3:p1","workspace_id":"w3","tab_id":"w3:t1",
//
//	"source":"visible","format":"text","text":"...\n","revision":0,
//	"truncated":false}
//
// source は visible|recent|recent_unwrapped|detection（invalid variant の
// エラーメッセージで列挙を実測）。
type PaneReadInfo struct {
	PaneID      string `json:"pane_id"`
	WorkspaceID string `json:"workspace_id"`
	TabID       string `json:"tab_id"`
	Source      string `json:"source"`
	Format      string `json:"format"` // "text"
	Text        string `json:"text"`
	Revision    int    `json:"revision"`
	Truncated   bool   `json:"truncated"`
}

// WorkspaceInfo は workspace.list / workspace.create に現れる workspace 情報。
// 実採取: {"workspace_id":"w3","number":3,"label":"tmp","focused":false,
//
//	"pane_count":1,"tab_count":1,"active_tab_id":"w3:t1",
//	"agent_status":"unknown"}
type WorkspaceInfo struct {
	WorkspaceID string `json:"workspace_id"`
	Number      int    `json:"number"`
	Label       string `json:"label,omitempty"`
	Focused     bool   `json:"focused"`
	PaneCount   int    `json:"pane_count"`
	TabCount    int    `json:"tab_count"`
	ActiveTabID string `json:"active_tab_id"`
	AgentStatus string `json:"agent_status"`
}

// TabInfo は workspace.create 応答等の tab 情報。
// 実採取: {"tab_id":"w3:t1","workspace_id":"w3","number":1,"label":"1",
//
//	"focused":false,"pane_count":1,"agent_status":"unknown"}
type TabInfo struct {
	TabID       string `json:"tab_id"`
	WorkspaceID string `json:"workspace_id"`
	Number      int    `json:"number"`
	Label       string `json:"label,omitempty"`
	Focused     bool   `json:"focused"`
	PaneCount   int    `json:"pane_count"`
	AgentStatus string `json:"agent_status"`
}

// WorkspaceCreated は workspace.create の応答（type:"workspace_created"）。
type WorkspaceCreated struct {
	Type      string        `json:"type"`
	Workspace WorkspaceInfo `json:"workspace"`
	Tab       TabInfo       `json:"tab"`
	RootPane  PaneInfo      `json:"root_pane"`
}

// AgentStarted は agent.start の応答（type:"agent_started"）。
// 実採取: {"type":"agent_started","agent":{...AgentInfo...},"argv":["sleep","5"]}
type AgentStarted struct {
	Type  string    `json:"type"`
	Agent AgentInfo `json:"agent"`
	Argv  []string  `json:"argv"`
}

// Event は events.subscribe が流す 1 行。
// 実採取: {"data":{"pane":{...},"type":"pane_created"},"event":"pane_created"}
//
// ⚠命名の非対称（実測）: 購読名は dot 形（"pane.created"）だが、配信される
// event 名は underscore 形（"pane_created"）。exact-match で扱うこと。
type Event struct {
	Name string          `json:"event"`
	Data json.RawMessage `json:"data"`
}

// Pane は pane 系 event（pane_created/pane_closed/pane_updated 等）の data から
// pane 情報を取り出す。pane を含まない event なら nil を返す。
func (e Event) Pane() (*PaneInfo, error) {
	var d struct {
		Pane *PaneInfo `json:"pane"`
	}
	if err := json.Unmarshal(e.Data, &d); err != nil {
		return nil, err
	}
	return d.Pane, nil
}
