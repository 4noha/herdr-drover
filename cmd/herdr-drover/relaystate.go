package main

// relayState — 共用 PC（slave）用の relay 経由クラウド状態クライアント。
// SA 鍵を持たず、durable な refresh secret（~/.herdr-drover/slave.json）で
// 1h の bearer を relay から取り、全 Firestore 操作を /slave/* HTTP へ委譲する
// （DESIGN_SLAVE_SPEC §4）。agentState を満たし、master の *state.Client と
// 差し替え可能（webterm/commands/producer の seam 共有）。
//
// 信頼境界は relay 側にある（bearer を毎リクエスト検証し、slave が viewer や
// 他 PC のデータに触れないことを構造的に禁止する）。この client は素直な
// HTTP クライアントで、near-$0 のための content_hash ゲートだけは
// **client 側**でも掛ける（state.contentHash と同一＝差分なし tick は POST
// しない）。
//
// メソッド→エンドポイント写像:
//   Close               → (no-op)
//   IsSelfRevoked       → GET  /slave/revoked   （network err ⇒ false）
//   RegisterPCVersion   → POST /slave/register
//   PushStatus          → POST /slave/push      （client 側 content_hash ゲート）
//   DeleteSession       → POST /slave/delete
//   OwnSessionKeys      → GET  /slave/sessions  （起動 seed）
//   WatchWake           → GET  /slave/wake      （long-poll・since カーソル）
//   PutRelayGrant       → POST /slave/grant     （role は常に source）
//   WatchCommands       → (P4/optional・no-op)
//   AckCommand          → (P4/optional・no-op)
// DialSource            → WSS  /session         （Authorization: Bearer 付き）

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/4noha/drover-cloud/state"
	"github.com/coder/websocket"
)

// tokenRefreshLead は bearer を exp のどれだけ前に先回り更新するか
// （DESIGN_SLAVE_SPEC §1.3: ≥5min before exp）。
const tokenRefreshLead = 5 * time.Minute

// wakePollTimeout は long-poll 1 回の client 側上限。relay の hold 窓（~25s）
// ＋往復レイテンシを吸収する余裕を持たせる（短すぎると hold 中に client が
// 切って無駄 re-poll になる）。
const wakePollTimeout = 40 * time.Second

// wakeBackoff は wake poll のエラー/403 後の再試行待ち（tick 側 dormant が
// 失効を権威判定するので、ここは軽い backoff で十分）。
const wakeBackoff = 2 * time.Second

// slaveFile は ~/.herdr-drover/slave.json のスキーマ（enroll --slave が書く）。
// SA 鍵は**含まない**（refresh secret だけが持続認証情報）。
type slaveFile struct {
	PC            string `json:"pc"`
	RefreshSecret string `json:"refresh_secret"`
	RelayURL      string `json:"relay_url"`
	GCPProject    string `json:"gcp_project"`
}

// slaveFilePath は ~/.herdr-drover/slave.json。
func slaveFilePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home ディレクトリ不明（slave.json を探せない）: %w", err)
	}
	return filepath.Join(home, ".herdr-drover", "slave.json"), nil
}

// readSlaveFile は slave.json を読む。不在は明示エラー（slave モードなのに
// 認証情報が無い＝enroll --slave 未実行）。
func readSlaveFile() (slaveFile, error) {
	path, err := slaveFilePath()
	if err != nil {
		return slaveFile{}, err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return slaveFile{}, fmt.Errorf("%s が無い（`herdr-drover enroll <code> --slave` を実行）", path)
	}
	if err != nil {
		return slaveFile{}, err
	}
	var sf slaveFile
	if err := json.Unmarshal(b, &sf); err != nil {
		return slaveFile{}, fmt.Errorf("%s が壊れている（JSON 解析失敗）: %w", path, err)
	}
	return sf, nil
}

type relayState struct {
	wsBase   string // wss://… /session dial 用（DialSource）
	httpBase string // https://… /slave/* HTTP 用（wss→https 変換済）
	pc       string
	secret   string       // durable refresh secret
	hc       *http.Client // /slave/* 通常 HTTP（30s timeout）
	wakeHC   *http.Client // long-poll 専用（timeout 無し・per-request ctx で制御）
	lg       *log.Logger

	tokMu    sync.Mutex
	token    string
	tokenExp time.Time

	hashMu   sync.Mutex
	lastHash map[string]string // key→content_hash（push 差分ゲート）
}

// newRelayState は slave.json を読み relayState を作る（DESIGN_SLAVE_SPEC §4.1）。
// relayURL/pcName は呼び手（cl）由来（config.json の cloud_relay_url / PCID）で、
// slave.json 側の値を権威として上書きする（enroll が書いた実体を信頼）。
func newRelayState(relayURL, pcName string, cfg Config, lg *log.Logger) (*relayState, error) {
	_ = cfg // Config は将来 knob 用に受けるが現状は slave.json が権威
	sf, err := readSlaveFile()
	if err != nil {
		return nil, err
	}
	if sf.RefreshSecret == "" {
		return nil, fmt.Errorf("slave.json に refresh_secret が無い（enroll --slave をやり直す）")
	}
	pc := pcName
	if sf.PC != "" {
		pc = sf.PC
	}
	if pc == "" {
		return nil, fmt.Errorf("slave: pc 名が解決できない")
	}
	wss := relayURL
	if wss == "" {
		wss = sf.RelayURL
	}
	if wss == "" {
		return nil, fmt.Errorf("slave: relay URL 未設定（config.json/slave.json を確認）")
	}
	httpBase := strings.Replace(strings.Replace(wss,
		"wss://", "https://", 1), "ws://", "http://", 1)
	return &relayState{
		wsBase:   wss,
		httpBase: httpBase,
		pc:       pc,
		secret:   sf.RefreshSecret,
		hc:       &http.Client{Timeout: 30 * time.Second},
		wakeHC:   &http.Client{},
		lg:       lg,
		lastHash: map[string]string{},
	}, nil
}

// --- token cache / refresh -------------------------------------------------

// bearer は現行 bearer を返す（exp まで <5min or force なら refresh）。
func (rs *relayState) bearer(ctx context.Context, force bool) (string, error) {
	rs.tokMu.Lock()
	defer rs.tokMu.Unlock()
	if !force && rs.token != "" && time.Until(rs.tokenExp) > tokenRefreshLead {
		return rs.token, nil
	}
	return rs.refreshLocked(ctx)
}

// invalidateToken は 401 応答時にキャッシュを捨てる（次 bearer で再取得）。
func (rs *relayState) invalidateToken() {
	rs.tokMu.Lock()
	rs.token = ""
	rs.tokMu.Unlock()
}

// slaveStatusErr は /slave/* の非 200 応答を HTTP status 付きで運ぶ。
// 汎用エラーへ潰すと IsSelfRevoked が「実 relay の失効＝403」を判別できず
// graceful dormancy が発火しない（＝revoke 後 launchd 再起動ストーム）実バグの
// 根治。call/refreshLocked が非 200 をこれで包み、slaveStatus で取り出す。
type slaveStatusErr struct {
	status int
	msg    string
}

func (e *slaveStatusErr) Error() string { return e.msg }

// slaveStatus は err（ラップ元含む）の HTTP status を返す。無ければ 0。
func slaveStatus(err error) int {
	var se *slaveStatusErr
	if errors.As(err, &se) {
		return se.status
	}
	return 0
}

// refreshLocked は POST /slave/token（refresh-secret gated・bearer 不要）で
// 1h bearer を鋳造する。tokMu 保持前提。
func (rs *relayState) refreshLocked(ctx context.Context) (string, error) {
	body, _ := json.Marshal(map[string]string{"pc": rs.pc, "secret": rs.secret})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rs.httpBase+"/slave/token", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := rs.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("slave token 取得失敗: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", &slaveStatusErr{resp.StatusCode,
			fmt.Sprintf("slave token 取得失敗(status=%d): %s", resp.StatusCode, strings.TrimSpace(string(b)))}
	}
	var tr struct {
		Token string `json:"token"`
		Exp   int64  `json:"exp"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return "", fmt.Errorf("slave token 応答解析失敗: %w", err)
	}
	if tr.Token == "" {
		return "", fmt.Errorf("slave token 応答が空")
	}
	rs.token = tr.Token
	if tr.Exp > 0 {
		rs.tokenExp = time.Unix(tr.Exp, 0)
	} else {
		rs.tokenExp = time.Now().Add(time.Hour)
	}
	return rs.token, nil
}

// --- generic /slave/* call（bearer 付き・401 で 1 回だけ refresh 再試行）---

func (rs *relayState) call(ctx context.Context, method, path string, reqBody, out any) error {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		tok, err := rs.bearer(ctx, attempt == 1)
		if err != nil {
			return err
		}
		var rdr io.Reader
		if reqBody != nil {
			b, err := json.Marshal(reqBody)
			if err != nil {
				return err
			}
			rdr = bytes.NewReader(b)
		}
		req, err := http.NewRequestWithContext(ctx, method, rs.httpBase+path, rdr)
		if err != nil {
			return err
		}
		if reqBody != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := rs.hc.Do(req)
		if err != nil {
			return fmt.Errorf("slave %s %s: %w", method, path, err)
		}
		if resp.StatusCode == 401 && attempt == 0 {
			resp.Body.Close()
			rs.invalidateToken()
			lastErr = fmt.Errorf("slave %s %s: 401（token 更新して再試行）", method, path)
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return &slaveStatusErr{resp.StatusCode,
				fmt.Sprintf("slave %s %s: status=%d body=%s", method, path, resp.StatusCode, strings.TrimSpace(string(body)))}
		}
		if out != nil {
			if err := json.Unmarshal(body, out); err != nil {
				return fmt.Errorf("slave %s %s 応答解析失敗: %w", method, path, err)
			}
		}
		return nil
	}
	return lastErr
}

// --- agentState 実装 -------------------------------------------------------

func (rs *relayState) Close() error { return nil }

// IsSelfRevoked は GET /slave/revoked。network/lookup 失敗は false
// （可用性優先・state.IsSelfRevoked と同意味論）。
func (rs *relayState) IsSelfRevoked(ctx context.Context) bool {
	var resp struct {
		Revoked bool `json:"revoked"`
	}
	err := rs.call(ctx, http.MethodGet, "/slave/revoked", nil, &resp)
	if err == nil {
		return resp.Revoked
	}
	// 実 relay は失効 slave を slaveGuard で **403**（/slave/revoked）にし、
	// /slave/token の mint も 403 で拒否する（200{revoked:true} は返らない）。
	// 403 = 失効と解釈して graceful dormancy を発火させる。network/その他
	// エラーは false（一時障害で誤って dormant にしない＝可用性優先）。
	return slaveStatus(err) == 403
}

func (rs *relayState) RegisterPCVersion(ctx context.Context, agentVersion string) error {
	return rs.call(ctx, http.MethodPost, "/slave/register",
		map[string]string{"agent_version": agentVersion}, nil)
}

// PushStatus は client 側 content_hash ゲート後に POST /slave/push。
// 差分なし tick は POST を一切出さない（near-$0）。成功時のみ lastHash を
// 更新する（POST 失敗分は次 tick で再送される）。
func (rs *relayState) PushStatus(ctx context.Context, sessions []map[string]any) (int, error) {
	rs.hashMu.Lock()
	var changed []map[string]any
	newHash := map[string]string{}
	for _, s := range sessions {
		k := sessionKey(s)
		h := contentHash(s)
		if rs.lastHash[k] != h {
			changed = append(changed, s)
			newHash[k] = h
		}
	}
	rs.hashMu.Unlock()

	if len(changed) == 0 {
		return 0, nil // 差分なし＝POST しない
	}
	var resp struct {
		Changed int `json:"changed"`
	}
	if err := rs.call(ctx, http.MethodPost, "/slave/push",
		map[string]any{"sessions": changed}, &resp); err != nil {
		return 0, err
	}
	rs.hashMu.Lock()
	if rs.lastHash == nil {
		rs.lastHash = map[string]string{}
	}
	for k, h := range newHash {
		rs.lastHash[k] = h
	}
	rs.hashMu.Unlock()
	return resp.Changed, nil
}

func (rs *relayState) DeleteSession(ctx context.Context, key string) error {
	if err := rs.call(ctx, http.MethodPost, "/slave/delete",
		map[string]string{"key": key}, nil); err != nil {
		return err
	}
	// 削除成功: hash を捨てて同 key が再出現したら再 push させる。
	rs.hashMu.Lock()
	delete(rs.lastHash, key)
	rs.hashMu.Unlock()
	return nil
}

func (rs *relayState) OwnSessionKeys(ctx context.Context) ([]string, error) {
	var resp struct {
		Keys []string `json:"keys"`
	}
	if err := rs.call(ctx, http.MethodGet, "/slave/sessions", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Keys, nil
}

// PutRelayGrant は POST /slave/grant（role は常に source）。viewer 要求は
// no-op（relay も slave の viewer grant を書かない＝呼ばれない想定）。
func (rs *relayState) PutRelayGrant(ctx context.Context, sid, role string, ttl time.Duration) error {
	if role != "source" {
		return nil
	}
	secs := int(ttl / time.Second)
	if secs < 1 {
		secs = 1
	}
	return rs.call(ctx, http.MethodPost, "/slave/grant",
		map[string]any{"sid": sid, "ttl_seconds": secs}, nil)
}

// WatchWake は GET /slave/wake の long-poll ループ（since カーソル）。
// state.WatchWake の func(sid string) コールバックを再構成する: 200 で
// cb(sid)＋since=ts→即 re-poll、204 で即 re-poll、403/エラーは軽い backoff
// 後に再試行（失効の権威判定は tick 側 dormant）。ctx 終了で戻る。
func (rs *relayState) WatchWake(ctx context.Context, cb func(sid string)) error {
	since := ""
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		sid, ts, status, err := rs.pollWake(ctx, since)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !sleepCtx(ctx, wakeBackoff) {
				return ctx.Err()
			}
			continue
		}
		switch status {
		case 200:
			if ts != "" {
				since = ts
			}
			if sid != "" {
				cb(sid)
			}
			// 即 re-poll（lossless catch-up）。
		case 204:
			// hold 窓で wake 無し。即 re-poll。
		default: // 403（mid-hold 失効）/ その他は backoff。
			if !sleepCtx(ctx, wakeBackoff) {
				return ctx.Err()
			}
		}
	}
}

// pollWake は long-poll 1 回。401 は 1 回だけ token 更新して再試行。
func (rs *relayState) pollWake(ctx context.Context, since string) (sid, ts string, status int, err error) {
	for attempt := 0; attempt < 2; attempt++ {
		tok, e := rs.bearer(ctx, attempt == 1)
		if e != nil {
			return "", "", 0, e
		}
		pollCtx, cancel := context.WithTimeout(ctx, wakePollTimeout)
		u := rs.httpBase + "/slave/wake?since=" + url.QueryEscape(since)
		req, e := http.NewRequestWithContext(pollCtx, http.MethodGet, u, nil)
		if e != nil {
			cancel()
			return "", "", 0, e
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, e := rs.wakeHC.Do(req)
		if e != nil {
			cancel()
			return "", "", 0, e
		}
		if resp.StatusCode == 401 && attempt == 0 {
			resp.Body.Close()
			cancel()
			rs.invalidateToken()
			continue
		}
		st := resp.StatusCode
		var wr struct {
			SID string `json:"sid"`
			TS  string `json:"ts"`
		}
		if st == 200 {
			_ = json.NewDecoder(resp.Body).Decode(&wr)
		}
		resp.Body.Close()
		cancel()
		return wr.SID, wr.TS, st, nil
	}
	return "", "", 0, fmt.Errorf("slave wake: 401（token 更新しても不可）")
}

// WatchCommands は slave 宛の遠隔命令を relay 越しに long-poll で受ける
// （master の state.Client.WatchCommands 相当。slave は SA レスで Firestore を
// 直読できないため relay の /slave/commands が仲介＝claim 済みのみ配信）。
// WatchWake と同型: 200 で命令を fn へ流して即 re-poll、204 は即 re-poll、
// 403/err は backoff。ctx 終了で戻る。relay の hold（~25s）で near-$0。
func (rs *relayState) WatchCommands(ctx context.Context, fn func(state.Command)) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		cmds, status, err := rs.pollCommands(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !sleepCtx(ctx, wakeBackoff) {
				return ctx.Err()
			}
			continue
		}
		switch status {
		case 200:
			for i := range cmds {
				fn(cmds[i])
			}
			// 即 re-poll（次の命令の catch-up）。
		case 204:
			// hold 窓で命令無し。即 re-poll。
		default: // 403（mid-hold 失効）/ その他は backoff。
			if !sleepCtx(ctx, wakeBackoff) {
				return ctx.Err()
			}
		}
	}
}

// pollCommands は /slave/commands の long-poll 1 回（pollWake と同型）。
// 401 は 1 回だけ token 更新して再試行。200 は {commands:[...]} を parse。
func (rs *relayState) pollCommands(ctx context.Context) (cmds []state.Command, status int, err error) {
	for attempt := 0; attempt < 2; attempt++ {
		tok, e := rs.bearer(ctx, attempt == 1)
		if e != nil {
			return nil, 0, e
		}
		pollCtx, cancel := context.WithTimeout(ctx, wakePollTimeout)
		u := rs.httpBase + "/slave/commands"
		req, e := http.NewRequestWithContext(pollCtx, http.MethodGet, u, nil)
		if e != nil {
			cancel()
			return nil, 0, e
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, e := rs.wakeHC.Do(req)
		if e != nil {
			cancel()
			return nil, 0, e
		}
		if resp.StatusCode == 401 && attempt == 0 {
			resp.Body.Close()
			cancel()
			rs.invalidateToken()
			continue
		}
		st := resp.StatusCode
		var cr struct {
			Commands []state.Command `json:"commands"`
		}
		if st == 200 {
			_ = json.NewDecoder(resp.Body).Decode(&cr)
		}
		resp.Body.Close()
		cancel()
		return cr.Commands, st, nil
	}
	return nil, 0, fmt.Errorf("slave commands: 401（token 更新しても不可）")
}

// AckCommand は命令の実行結果を relay 越しに書き戻す（/slave/command-ack へ POST）。
func (rs *relayState) AckCommand(ctx context.Context, id, status, detail string) error {
	return rs.call(ctx, http.MethodPost, "/slave/command-ack", map[string]any{
		"id": id, "status": status, "detail": detail,
	}, nil)
}

// DialSource は relay へ source 役で dial（Authorization: Bearer 付き）。
// webterm の dialSource seam に注入する（master は header-less relayclient.Dial）。
func (rs *relayState) DialSource(ctx context.Context, sid string) (net.Conn, error) {
	tok, err := rs.bearer(ctx, false)
	if err != nil {
		return nil, err
	}
	return dialAuthSession(ctx, rs.wsBase, sid, "source", tok)
}

// dialAuthSession は relayclient.Dial のワイヤ契約（URL=/session?sid=&role=・
// QueryEscape(sid)・NetConn(MessageBinary)）と byte 同一で、唯一の差は
// Authorization: Bearer ヘッダを付けること（DESIGN_SLAVE_SPEC §4.3 DialAuth）。
func dialAuthSession(ctx context.Context, wssBase, sid, role, bearer string) (net.Conn, error) {
	u := wssBase + "/session?sid=" + url.QueryEscape(sid) + "&role=" + role
	h := http.Header{}
	if bearer != "" {
		h.Set("Authorization", "Bearer "+bearer)
	}
	c, _, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		return nil, err
	}
	return websocket.NetConn(ctx, c, websocket.MessageBinary), nil
}

// --- content_hash（state.contentHash の byte 同一コピー）--------------------
//
// 同一作者による cm→drover コピーが許される層（DESIGN_SLAVE_SPEC §4.2）。
// version/updated_at/content_hash を除外・キー昇順・[][2]any の JSON を
// sha256。state 側と 1 bit も違えると client/server ゲートが食い違うので
// exact-match で写す。

func contentHash(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		if k == "version" || k == "updated_at" || k == "content_hash" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	ord := make([][2]any, 0, len(keys))
	for _, k := range keys {
		ord = append(ord, [2]any{k, m[k]})
	}
	b, _ := json.Marshal(ord)
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// sessionKey は session doc の id（state.sessionKey の byte 同一コピー）。
func sessionKey(s map[string]any) string {
	if v, ok := s["key"].(string); ok && v != "" {
		return v
	}
	if v, ok := s["session_id"].(string); ok && v != "" {
		return v
	}
	return "unknown"
}

// sleepCtx は ctx 中断可能な待機。待てたら true、ctx 終了で false。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
