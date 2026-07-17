package herdrapi

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

// APIError は herdr の {"error":{"code","message"}} を Go error にしたもの。
// 実採取例: {"code":"pane_not_found","message":"pane w99:p9 not found"} /
// {"code":"invalid_request",...} / {"code":"invalid_metadata_request",...}。
// code は exact-match で分岐できる（ヒューリスティック分類はしない）。
type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *APIError) Error() string {
	return fmt.Sprintf("herdr api: %s: %s", e.Code, e.Message)
}

const (
	// defaultDialTimeout: socket 不在/サーバ死亡はすぐ分かるので短め。
	defaultDialTimeout = 5 * time.Second
	// defaultCallTimeout: 1 リクエスト往復の上限。通常応答はミリ秒オーダー
	//（実測）なので余裕を大きく取っても実害はない。
	defaultCallTimeout = 30 * time.Second
)

// Client は herdr ndjson API socket への 1 接続=1 リクエスト クライアント。
// フィールドは接続毎に読むだけなので並行 Call は安全（seq のみ atomic）。
type Client struct {
	// SocketPath は解決済みの socket パス。New で解決される。
	SocketPath string
	// Timeout はリクエスト往復の上限。ゼロなら defaultCallTimeout。
	Timeout time.Duration

	seq atomic.Uint64 // リクエスト id 採番（応答突合せ・ログ識別用）
}

// New は socket パスを解決して Client を作る。
// 解決順: 明示引数 > HERDR_SOCKET_PATH > ~/.config/herdr/herdr.sock（既定）。
// ⚠sun_path は 104B 制約（macOS）＝深い階層のパスはサーバ起動自体が失敗する。
func New(socketPath string) *Client {
	return &Client{SocketPath: ResolveSocketPath(socketPath)}
}

// ResolveSocketPath は New と同じ解決規則を単体で提供する（診断表示用）。
func ResolveSocketPath(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if p := os.Getenv("HERDR_SOCKET_PATH"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// home 不明でも「herdr.sock」への相対 dial で即エラーになり原因が
		// 分かる方が、空文字で意味不明に失敗するよりまし。
		return "herdr.sock"
	}
	return filepath.Join(home, ".config", "herdr", "herdr.sock")
}

// request / response は wire 形式（実採取に一致）。
type request struct {
	ID     string `json:"id"`
	Method string `json:"method"`
	Params any    `json:"params"`
}

type response struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *APIError       `json:"error"`
}

// Call は 1 接続で 1 リクエストを送り result（json.RawMessage）を返す。
// params が nil の場合は {} を送る（params フィールド自体が必須＝実測
// "missing field `params`"）。エラー応答は *APIError として返す。
//
// サーバは応答 1 行を書いた後に接続を close する（実測: 同一接続の 2 発目
// は BrokenPipe）ので、接続の使い回しはしない。
func (c *Client) Call(method string, params any) (json.RawMessage, error) {
	if params == nil {
		params = struct{}{}
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = defaultCallTimeout
	}
	conn, err := net.DialTimeout("unix", c.SocketPath, defaultDialTimeout)
	if err != nil {
		return nil, fmt.Errorf("herdr dial %s: %w", c.SocketPath, err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return nil, err
	}

	id := fmt.Sprintf("%d", c.seq.Add(1))
	buf, err := json.Marshal(request{ID: id, Method: method, Params: params})
	if err != nil {
		return nil, fmt.Errorf("herdr marshal %s: %w", method, err)
	}
	buf = append(buf, '\n')
	if _, err := conn.Write(buf); err != nil {
		return nil, fmt.Errorf("herdr write %s: %w", method, err)
	}

	// 応答は必ず 1 行（ndjson）。
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return nil, fmt.Errorf("herdr read %s: %w", method, err)
	}
	var resp response
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("herdr decode %s: %w (line=%.200s)", method, err, line)
	}
	if resp.Error != nil {
		return nil, resp.Error
	}
	// パースエラー時は id="" で返る（実測）。それは resp.Error 側で返済み。
	// domain 応答の id は送った id が echo される（実測）＝不一致は異常。
	if resp.ID != id {
		return nil, fmt.Errorf("herdr %s: response id mismatch: got %q want %q", method, resp.ID, id)
	}
	return resp.Result, nil
}

// call はラッパ共通の「result を out へ decode」ヘルパ。
func (c *Client) call(method string, params, out any) error {
	raw, err := c.Call(method, params)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("herdr decode %s result: %w", method, err)
	}
	return nil
}

// Ping はサーバ生存＋version/protocol 確認。
// DESIGN の hidden CLI リスク対応: agent 起動時に version を実測し、
// 未検証 version へは警告する用途の一次情報源。
func (c *Client) Ping() (*PongInfo, error) {
	var out PongInfo
	if err := c.call("ping", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PaneList は全 pane を列挙する。result: {"type":"pane_list","panes":[...]}
func (c *Client) PaneList() ([]PaneInfo, error) {
	var out struct {
		Panes []PaneInfo `json:"panes"`
	}
	if err := c.call("pane.list", nil, &out); err != nil {
		return nil, err
	}
	return out.Panes, nil
}

// AgentList は全 agent を列挙する。result: {"type":"agent_list","agents":[...]}
func (c *Client) AgentList() ([]AgentInfo, error) {
	var out struct {
		Agents []AgentInfo `json:"agents"`
	}
	if err := c.call("agent.list", nil, &out); err != nil {
		return nil, err
	}
	return out.Agents, nil
}

// PaneGet は 1 pane の情報。result: {"type":"pane_info","pane":{...}}
func (c *Client) PaneGet(paneID string) (*PaneInfo, error) {
	var out struct {
		Pane PaneInfo `json:"pane"`
	}
	err := c.call("pane.get", struct {
		PaneID string `json:"pane_id"`
	}{paneID}, &out)
	if err != nil {
		return nil, err
	}
	return &out.Pane, nil
}

// PaneSendText は pane の PTY へリテラル書込する（Enter は "\r" を含める）。
// ⚠JSON 文字列に生の制御バイトは書けない（実測: "control character found
// while parsing a string"）が、json.Marshal が \r → \\r へ escape するので
// Go からは素の文字列を渡してよい。制御バイト（\r）の実透過は実測済。
func (c *Client) PaneSendText(paneID, text string) error {
	return c.call("pane.send_text", struct {
		PaneID string `json:"pane_id"`
		Text   string `json:"text"`
	}{paneID, text}, nil)
}

// PaneSendInput は pane.send_input の text 形。
//
// ⚠実測（v0.7.4・plain zsh pane）: 印字文字は届くが **text 中の \r は落ち
// コマンドが実行されない**（send_text は同じ \r で実行される）。Enter が
// 必要なら PaneSendText か PaneSendKeys(paneID, "Enter") を使うこと。
// 制御バイト透過の決定木確定は DESIGN Phase 2（本メソッドは wire 忠実の
// 送出のみ担う）。
func (c *Client) PaneSendInput(paneID, text string) error {
	return c.call("pane.send_input", struct {
		PaneID string `json:"pane_id"`
		Text   string `json:"text"`
	}{paneID, text}, nil)
}

// PaneSendInputBytes は pane.send_input の bytes 形（base64 文字列）。
//
// ⚠実測（v0.7.4・plain zsh pane）: base64 文字列でも数値配列でも応答は
// {"type":"ok"} だが **pane には何も届かなかった**（"echo BYTES_OK\r" が
// 不着。同 pane への send_text/send_keys は到達）。agent pane では挙動が
// 異なる可能性があるが未測＝Phase 2 の入力決定木で確定する。ok 応答を
// 到達の証拠にしないこと。
func (c *Client) PaneSendInputBytes(paneID string, data []byte) error {
	return c.call("pane.send_input", struct {
		PaneID string `json:"pane_id"`
		Bytes  string `json:"bytes"`
	}{paneID, base64.StdEncoding.EncodeToString(data)}, nil)
}

// PaneSendKeys は名前付きキーを送る（例 "Enter"）。send_input の \r 欠落の
// 補完経路として実測済（"Enter" で zsh のコマンドが実行された）。
func (c *Client) PaneSendKeys(paneID string, keys ...string) error {
	return c.call("pane.send_keys", struct {
		PaneID string   `json:"pane_id"`
		Keys   []string `json:"keys"`
	}{paneID, keys}, nil)
}

// PaneRead は pane の表示内容を読む。source は
// visible|recent|recent_unwrapped|detection（実測列挙）。
// result: {"type":"pane_read","read":{...}}
func (c *Client) PaneRead(paneID, source string) (*PaneReadInfo, error) {
	var out struct {
		Read PaneReadInfo `json:"read"`
	}
	err := c.call("pane.read", struct {
		PaneID string `json:"pane_id"`
		Source string `json:"source"`
	}{paneID, source}, &out)
	if err != nil {
		return nil, err
	}
	return &out.Read, nil
}

// AgentStartOptions は agent.start の任意パラメータ（実測で受理・反映を
// 確認したフィールドのみ。cwd は起動 pane の cwd に反映されることを実測）。
type AgentStartOptions struct {
	Cwd         string `json:"cwd,omitempty"`
	WorkspaceID string `json:"workspace_id,omitempty"`
}

// AgentStart は名前付き agent を新 pane で起動する。name と argv は必須
// （実測: 欠くと "missing field `argv`" 等）。
// result: {"type":"agent_started","agent":{...},"argv":[...]}
func (c *Client) AgentStart(name string, argv []string, opts *AgentStartOptions) (*AgentInfo, error) {
	params := struct {
		Name        string   `json:"name"`
		Argv        []string `json:"argv"`
		Cwd         string   `json:"cwd,omitempty"`
		WorkspaceID string   `json:"workspace_id,omitempty"`
	}{Name: name, Argv: argv}
	if opts != nil {
		params.Cwd = opts.Cwd
		params.WorkspaceID = opts.WorkspaceID
	}
	var out AgentStarted
	if err := c.call("agent.start", params, &out); err != nil {
		return nil, err
	}
	return &out.Agent, nil
}

// ReportMetadata は pane.report_metadata で設定する値。少なくとも 1 つの
// metadata フィールドが必要（実測: 空だと invalid_metadata_request
// "missing metadata field to set or clear"）。
// title / tokens は pane.get・pane_updated event に反映されることを実測
// （revision がインクリメントされる）。drover の identity 符号化
// （source:"drover" ＋ tokens{pc,sid}）はこの経路を使う。
type ReportMetadata struct {
	Title  string            `json:"title,omitempty"`
	Tokens map[string]string `json:"tokens,omitempty"`
}

// PaneReportMetadata は pane へ外部 metadata を報告する。source は報告者
// 識別子（必須。実測: 欠くと "missing field `source`"）。
func (c *Client) PaneReportMetadata(paneID, source string, m ReportMetadata) error {
	if m.Title == "" && len(m.Tokens) == 0 {
		return errors.New("herdrapi: PaneReportMetadata requires at least one metadata field (title/tokens)")
	}
	return c.call("pane.report_metadata", struct {
		PaneID string            `json:"pane_id"`
		Source string            `json:"source"`
		Title  string            `json:"title,omitempty"`
		Tokens map[string]string `json:"tokens,omitempty"`
	}{paneID, source, m.Title, m.Tokens}, nil)
}

// WorkspaceCreate は新 workspace（root pane 付き）を作る。params は空で
// よい（実測）。テスト・reconcile の pane 生成起点。
func (c *Client) WorkspaceCreate() (*WorkspaceCreated, error) {
	var out WorkspaceCreated
	if err := c.call("workspace.create", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// WorkspaceClose は workspace を閉じる（配下 pane も終了）。
func (c *Client) WorkspaceClose(workspaceID string) error {
	return c.call("workspace.close", struct {
		WorkspaceID string `json:"workspace_id"`
	}{workspaceID}, nil)
}
