//go:build !windows

// webterm_e2e_test — Phase 2「Web ターミナル」の e2e 基盤（常設・実環境）:
//
//	ローカル実 relay（drover-cloud の relay 実行体を無改変で go build 起動）
//	＋ 実 herdr 隔離サーバ ＋ 実 Firestore エミュレータ ＋ 実 drover agent
//	（go build した herdr-drover を `agent` サブコマンドで spawn）＋ 機械
//	viewer（cm ワイヤ契約抽出の viewer dial 仕様で WSS 接続）で
//
//	  viewer 接続 → RESIZE 送出 → wake/{pc} 書込 → agent WatchWake
//	  → PutRelayGrant(source) → relay dial → bridge → observe frame が
//	  viewer に届く（DECSET 2026 括り・base64 でなく生 ANSI）
//	  → pane へ echo marker send → フレームに marker 出現
//	  → 多重 wake は既存 bridge 生存中なら無視（bridge 開始ログ 1 回）
//	  → SIGTERM で全 bridge 停止＝agent graceful 終了・viewer 切断
//
// を検証する。鉄則どおり合成 relay での代替はしない（drover-cloud リポジトリ不在＝
// relay 実行体を用意できない環境は理由を明示して Skip。drover-cloud リポジトリは
// 読み取り＋外部 dir への go build のみ＝一切変更しない）。
//
// 依存パッケージの都合（本テスト自体は internal/bridge を import しない＝
// vet/コンパイルは bridge 着地前から緑）: 実 agent バイナリの go build が
// internal/bridge を要求するため、bridge 未着地の間は Skip する（統合
// フェーズが着地後に実行する）。
package e2e

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/4noha/drover-cloud/state"
)

// webtermPCID は本ゲート専用の PC id（TestE2EAgentLifecycle の e2e-herdr と
// 分離＝同一エミュレータ project 内でセッション doc/wake が干渉しない）。
// -herdr サフィックスは DESIGN 決定事項（cm agent との削除合戦防止）。
const webtermPCID = "e2e-webterm-herdr"

// ============ ヘルパ ============

// syncBuf は syncbuf_test.go（パッケージ共有ヘルパ）へ移動した。

// requireBridge は internal/bridge（並行実装中）の着地前は Skip する。
// 本テストの実行には実 agent バイナリ（= bridge import 済みの cmd）の
// go build が必要なため。黙って FAIL にせず理由を明示する（鉄則2 の
// 「不在環境は Skip」と同じ扱い。着地後は自動で実行対象になる）。
func requireBridge(t *testing.T) {
	t.Helper()
	dir := filepath.Join(moduleRoot(t), "internal", "bridge")
	ents, err := os.ReadDir(dir)
	if err == nil {
		for _, e := range ents {
			if strings.HasSuffix(e.Name(), ".go") {
				return
			}
		}
	}
	t.Skip("SKIP: internal/bridge 未着地（並行実装中）。着地後に統合フェーズが本テストを実行する")
}

// droverCloudRoot は drover-cloud リポジトリ（切り出したクラウド層＝canonical
// relay。cm と byte 等価）の場所を解決する。DROVER_CLOUD_REPO 環境変数 > 既定
// ~/works/tools/drover-cloud。不在なら Skip（合成 relay で代替しない＝鉄則。
// 理由はメッセージで正直に返す）。cm 依存を切り離し drover 単独で e2e 可能に。
func droverCloudRoot(t *testing.T) string {
	t.Helper()
	repo := os.Getenv("DROVER_CLOUD_REPO")
	if repo == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Skipf("SKIP: HOME 不明で drover-cloud リポジトリを解決できない（実 relay 起動不可・合成では代替しない）: %v", err)
		}
		repo = filepath.Join(home, "works", "tools", "drover-cloud")
	}
	if fi, err := os.Stat(filepath.Join(repo, "cmd", "relay", "main.go")); err != nil || fi.IsDir() {
		t.Skipf("SKIP: drover-cloud リポジトリ不在（%s）＝ローカル実 relay を起動できない。合成 relay では代替しない（DROVER_CLOUD_REPO で場所を指定可）", repo)
	}
	return repo
}

// startLocalRelay は drover-cloud の relay 実行体（cmd/relay・cm と byte 等価）を
// **無改変**で go build（出力は本テストの TempDir＝リポジトリへ一切書かない）し、
// ローカルで起動する。戻り値は ws://127.0.0.1:<port>（relay ベース URL）。
//
// 環境は意図的に最小（PORT のみ・GCP_PROJECT/WEB_SIGNING_KEY 等を渡さない）
// ＝relay main.go handler() は web 無効・Grant フック nil で公開 /session が
// 無認可で通る構成になる（cm ワイヤ契約「ローカル relay でも同じか」で実証
// 済みの構成。ペアリング/再接続/2 分待ち等の semantics は本番と同一コード）。
// grant 強制（CheckRelayGrant）の検証は cm 側の実 GCP e2e が担う。agent は
// 本番同順で PutRelayGrant を書く（エミュレータへ・無害）。
func startLocalRelay(t *testing.T, repoRoot string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "cloud-relay")
	build := exec.Command("go", "build", "-o", bin, "./cmd/relay")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		// リポジトリはあるのに build できないのは実障害＝Skip でなく FAIL
		// （理由を正直に返す）。
		t.Fatalf("ローカル実 relay を起動できない（drover-cloud cmd/relay の go build 失敗）: %v\n%s", err, out)
	}

	port := freePort()
	relay := exec.Command(bin)
	// 最小 env: PORT のみ意味を持つ。PATH/HOME は無害な補助。
	relay.Env = []string{
		"PORT=" + fmt.Sprint(port),
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
	}
	var rlog syncBuf
	relay.Stdout = &rlog
	relay.Stderr = &rlog
	if err := relay.Start(); err != nil {
		t.Fatalf("ローカル実 relay を起動できない（spawn 失敗）: %v", err)
	}
	relayDone := make(chan error, 1)
	go func() { relayDone <- relay.Wait() }()
	t.Cleanup(func() {
		_ = relay.Process.Kill()
		<-relayDone
	})

	// ヘルスは "/"（cm 知見: /healthz は GFE 予約。ローカルでも同じ流儀）。
	httpURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	deadline := time.Now().Add(20 * time.Second)
	for {
		select {
		case err := <-relayDone:
			t.Fatalf("ローカル実 relay が早期終了: %v\nlog:\n%s", err, rlog.String())
		default:
		}
		resp, err := http.Get(httpURL + "/")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return fmt.Sprintf("ws://127.0.0.1:%d", port)
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("ローカル実 relay がヘルス応答しない（%s/）\nlog:\n%s", httpURL, rlog.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// stripANSI は ESC シーケンス（CSI/OSC/2 バイト ESC）を除去して可読文字だけ
// 残す。marker 検索用のテキスト抽出であって内容のヒューリスティック分類では
// ない（何かを推測しない＝機械的なシーケンス構文の除去のみ）。
func stripANSI(b []byte) string {
	var out []byte
	for i := 0; i < len(b); i++ {
		c := b[i]
		if c != 0x1b {
			out = append(out, c)
			continue
		}
		i++
		if i >= len(b) {
			break
		}
		switch b[i] {
		case '[': // CSI: 中間バイト（0x20-0x3f）を読み飛ばし final byte（0x40-0x7e）で終端
			for i++; i < len(b) && (b[i] < 0x40 || b[i] > 0x7e); i++ {
			}
		case ']': // OSC: BEL か ST（ESC \）で終端
			for i++; i < len(b); i++ {
				if b[i] == 0x07 {
					break
				}
				if b[i] == 0x1b && i+1 < len(b) && b[i+1] == '\\' {
					i++
					break
				}
			}
		default: // 2 バイト ESC（ESC 7 / ESC = 等）: 1 バイト読み飛ばし
		}
	}
	return string(out)
}

// dialViewer は機械 viewer（cm ワイヤ契約の dial 仕様そのまま: URL =
// base + "/session?sid=" + sid + "&role=viewer"・エスケープ無し・
// DialOptions{} 既定 → NetConn(MessageBinary)）で relay へ接続し、
// conn／受信スナップショット関数／切断通知 chan を返す。読み手は 1 つ
// （cm relay takeover 修正の規律）。
func dialViewer(t *testing.T, ctx context.Context, relayURL, sid string) (net.Conn, func() []byte, chan struct{}) {
	t.Helper()
	wc, _, err := websocket.Dial(ctx, relayURL+"/session?sid="+sid+"&role=viewer",
		&websocket.DialOptions{})
	if err != nil {
		t.Fatalf("viewer dial: %v", err)
	}
	viewer := websocket.NetConn(ctx, wc, websocket.MessageBinary)
	t.Cleanup(func() { viewer.Close() })

	var mu sync.Mutex
	var buf []byte
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		b := make([]byte, 32*1024)
		for {
			n, err := viewer.Read(b)
			if n > 0 {
				mu.Lock()
				buf = append(buf, b[:n]...)
				mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	recv := func() []byte {
		mu.Lock()
		defer mu.Unlock()
		return append([]byte{}, buf...)
	}
	return viewer, recv, closed
}

// resizeFrame は cm ワイヤの RESIZE magic（0xff 0xff + rows u16BE + cols u16BE）。
func resizeFrame(rows, cols uint16) []byte {
	f := []byte{0xff, 0xff, 0, 0, 0, 0}
	binary.BigEndian.PutUint16(f[2:4], rows)
	binary.BigEndian.PutUint16(f[4:6], cols)
	return f
}

// observeChildren は agent プロセス**直下**の observe サブプロセスを列挙する
// （プロセスリーク検査）。ppid の exact-match で自テストの agent の子だけを
// 見る＝pgrep -f のようなシステム全域のコマンドライン検索は、本 PC で実運用
// 中の別 drover／別テストの observe と衝突して偽判定になるため使わない
// （bridge.observePID コメントと同じ理由。ヒューリスティックではなく
// 親子関係の機械的照合）。
func observeChildren(t *testing.T, ppid int) []string {
	t.Helper()
	// ps は macOS/Linux 共通の POSIX 形式（-axo pid=,ppid=,command=）。
	out, err := exec.Command("ps", "-axo", "pid=,ppid=,command=").Output()
	if err != nil {
		t.Fatalf("ps: %v", err)
	}
	var got []string
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || f[1] != strconv.Itoa(ppid) {
			continue
		}
		if strings.Contains(strings.Join(f[2:], " "), "terminal session observe") {
			got = append(got, strings.TrimSpace(line))
		}
	}
	return got
}

// ============ Phase 2 gate（実 relay＋実 herdr＋実 agent＋機械 viewer） ============

func TestE2EWebTerminal(t *testing.T) {
	if os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
		t.Skip("SKIP: gcloud / Java21+ 不在のため Firestore emulator 検証不可")
	}
	requireBridge(t)
	srv, hc := startHerdr(t)
	relayURL := startLocalRelay(t, droverCloudRoot(t))
	bin := buildBinary(t)

	// wake/読み取り用の in-process クライアント（機械 viewer の GCP 側半分。
	// cm TestE2ERealGCP の stV と同役。ローカル relay は Grant nil なので
	// PutRelayGrant(viewer) は不要＝書かない。書く経路の検証は cm 実 GCP e2e）。
	st, err := state.New(context.Background(), projectID, "e2e-webterm-viewer")
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	// --- 実 agent 起動（隔離 env は TestE2EAgentLifecycle と同じ流儀＋relay）---
	tmpHome := t.TempDir()
	agent := exec.Command(bin, "agent")
	agent.Env = []string{
		"HOME=" + tmpHome,
		"PATH=" + os.Getenv("PATH"),
		"GCP_PROJECT=" + projectID,
		"PC_ID=" + webtermPCID,
		"DROVER_TICK=1s",
		"FIRESTORE_EMULATOR_HOST=" + os.Getenv("FIRESTORE_EMULATOR_HOST"),
		"HERDR_SOCKET_PATH=" + srv.sock,
		"CLOUD_RELAY_URL=" + relayURL,
	}
	var agentErr, agentOut syncBuf
	agent.Stderr = &agentErr
	agent.Stdout = &agentOut
	if err := agent.Start(); err != nil {
		t.Fatalf("agent start: %v", err)
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- agent.Wait() }()
	agentDead := false
	t.Cleanup(func() {
		if !agentDead {
			_ = agent.Process.Kill()
			<-waitCh
		}
	})
	dead := func() bool {
		select {
		case err := <-waitCh:
			agentDead = true
			t.Fatalf("agent が早期終了: %v\nstderr:\n%s\nstdout:\n%s", err, agentErr.String(), agentOut.String())
			return true
		default:
			return false
		}
	}

	// 起動完了の物証: RegisterPC で pcs/{pc} が出る（WatchWake は初回
	// snapshot でも既在 wake doc で発火する契約なので、厳密な listener
	// attach 待ちは不要＝wake を書けば必ず拾われる）。
	waitFor(t, 20*time.Second, "pcs/"+webtermPCID+" registered", func() (bool, error) {
		if dead() {
			return false, nil
		}
		pcs, err := st.ListPCs(context.Background())
		if err != nil {
			return false, err
		}
		for _, p := range pcs {
			if p == webtermPCID {
				return true, nil
			}
		}
		return false, fmt.Errorf("pcs=%v", pcs)
	})

	// --- 対象 pane（sid = pane_id。DESIGN のセッション key と同じ）---
	ws, err := hc.WorkspaceCreate()
	if err != nil {
		t.Fatalf("workspace.create: %v", err)
	}
	paneID := ws.RootPane.PaneID
	t.Cleanup(func() { _ = hc.WorkspaceClose(ws.Workspace.WorkspaceID) })

	// --- 機械 viewer: cm ワイヤ契約の dial 仕様そのまま ---
	// URL = base + "/session?sid=" + sid + "&role=viewer"（エスケープ無し・
	// DialOptions{} 既定）→ NetConn(MessageBinary) でバイトストリーム化。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wc, _, err := websocket.Dial(ctx, relayURL+"/session?sid="+paneID+"&role=viewer",
		&websocket.DialOptions{})
	if err != nil {
		t.Fatalf("viewer dial: %v", err)
	}
	viewer := websocket.NetConn(ctx, wc, websocket.MessageBinary)
	t.Cleanup(func() { viewer.Close() })

	var recvMu sync.Mutex
	var recvBuf []byte
	viewerClosed := make(chan struct{})
	go func() {
		defer close(viewerClosed)
		b := make([]byte, 32*1024)
		for {
			n, err := viewer.Read(b)
			if n > 0 {
				recvMu.Lock()
				recvBuf = append(recvBuf, b[:n]...)
				recvMu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	recv := func() []byte {
		recvMu.Lock()
		defer recvMu.Unlock()
		return append([]byte{}, recvBuf...)
	}

	// RESIZE フレーム（0xff 0xff + rows u16BE + cols u16BE）を先に送出。
	// relay の先着待ち semantics（writePeer が相手到着まで最大 2 分保持）で
	// source（bridge）接続後に届き、bridge の「初回 RESIZE magic 待ち→
	// observe spawn」を駆動する。
	const rows, cols = 40, 120
	rs := []byte{0xff, 0xff}
	var pp [4]byte
	binary.BigEndian.PutUint16(pp[0:2], rows)
	binary.BigEndian.PutUint16(pp[2:4], cols)
	if _, err := viewer.Write(append(rs, pp[:]...)); err != nil {
		t.Fatalf("viewer RESIZE write: %v", err)
	}

	// --- wake: viewer 側から wake/{pc} を書く（Web /ws の st.Wake と同役）---
	if err := st.Wake(ctx, webtermPCID, paneID); err != nil {
		t.Fatalf("Wake: %v", err)
	}

	// --- フレーム到達: 全フレーム DECSET 2026 括り（実 herdr observe 実測）---
	waitFor(t, 30*time.Second, "observe frame が viewer に届く（\\x1b[?2026h）", func() (bool, error) {
		if dead() {
			return false, nil
		}
		return bytes.Contains(recv(), []byte("\x1b[?2026h")), fmt.Errorf(
			"received=%d bytes; agent stderr:\n%s", len(recv()), agentErr.String())
	})

	// --- 生 ANSI であること: observe の ndjson envelope（base64）が漏れて
	// いない＝bridge が decode 済みのバイトを流している。exact-match の
	// 物証 3 点（envelope の型名・encoding フィールド・base64 の器）。
	got := recv()
	for _, leak := range []string{`"type":"terminal.frame"`, `"encoding":"ansi"`, `"bytes":"`} {
		if bytes.Contains(got, []byte(leak)) {
			t.Fatalf("viewer 受信が ndjson envelope のまま（base64 未 decode）: %q が出現\nhead=%.200q", leak, got)
		}
	}

	// --- echo marker 往復: pane へ send_text（\r 込みリテラル＝実行到達の
	// 実測レシピ）→ observe フレームに marker が出る。ANSI 除去は構文的
	// strip のみ（BlitEncoder のセル差分でも新規出力行は連続 run で出る）。
	marker := fmt.Sprintf("HDWT_MARK_%d", time.Now().UnixNano())
	if err := hc.PaneSendText(paneID, "echo "+marker+"\r"); err != nil {
		t.Fatalf("pane.send_text: %v", err)
	}
	waitFor(t, 20*time.Second, "marker がフレームに出現", func() (bool, error) {
		if dead() {
			return false, nil
		}
		return strings.Contains(stripANSI(recv()), marker), fmt.Errorf(
			"marker 未着 (received=%d bytes)", len(recv()))
	})

	// --- 多重 wake: 既存 bridge 生存中の wake は無視（sid 毎 1 本）---
	if err := st.Wake(ctx, webtermPCID, paneID); err != nil {
		t.Fatalf("Wake(2 回目): %v", err)
	}
	time.Sleep(2 * time.Second) // listener 伝播猶予
	// データ線が無傷である物証: 2 個目の marker も届く（もし agent が二重
	// dial していれば relay の source slot 置換で旧 bridge の conn が切れ、
	// ここが壊れる）。
	marker2 := fmt.Sprintf("HDWT_MARK2_%d", time.Now().UnixNano())
	if err := hc.PaneSendText(paneID, "echo "+marker2+"\r"); err != nil {
		t.Fatalf("pane.send_text(2): %v", err)
	}
	waitFor(t, 20*time.Second, "多重 wake 後も marker が届く", func() (bool, error) {
		if dead() {
			return false, nil
		}
		return strings.Contains(stripANSI(recv()), marker2), fmt.Errorf("marker2 未着")
	})
	// 直接の物証: bridge 開始ログが当該 sid で 1 回だけ（webterm.go の
	// ログ書式 %q と exact-match）。
	startLog := fmt.Sprintf("bridge 開始 sid=%q", paneID)
	if n := strings.Count(agentErr.String(), startLog); n != 1 {
		t.Fatalf("bridge 開始が %d 回（期待 1 回＝多重 wake 無視）\nstderr:\n%s", n, agentErr.String())
	}

	// --- SIGTERM → 全 bridge 停止＝graceful 終了（exit 0）・viewer 切断 ---
	if err := agent.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	select {
	case err := <-waitCh:
		agentDead = true
		if err != nil {
			t.Fatalf("SIGTERM で exit 0 にならない（bridge 停止がハングした疑い）: %v\nstderr:\n%s", err, agentErr.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("SIGTERM 後 15s 経っても agent が終了しない\nstderr:\n%s", agentErr.String())
	}
	// source（bridge conn）の自然死で relay が相手（viewer）も畳む契約。
	select {
	case <-viewerClosed:
	case <-time.After(10 * time.Second):
		t.Fatalf("agent 終了後も viewer 接続が切れない（relay のペア解放契約に反する）")
	}
	t.Logf("agent stderr:\n%s", agentErr.String())
}

// ============ Phase 2 gate（quiescence: 無通信自切断＋observe 掃除） ============

// TestE2EWebTerminalQuiescence は viewer 無通信での quiescence 自切断
// （cm BridgeSourceIdle の 30s 無通信切断と同じ意味論・near-$0 設計の要）と、
// 切断後の observe subprocess 掃除（プロセスリーク無し）・次 wake での復帰を
// フルスタック（実 relay＋実 herdr＋実 agent＋機械 viewer）で検証する。
//
// bridge 単体の quiescence は internal/bridge の TestBridgeQuiescenceIdleClose
// が実 herdr で担保済み。ここは「agent プロセス経路（DROVER_IDLE env →
// webterm 配線 → bridge.Idle）＋relay のペア解放（source 自切断で viewer も
// 畳まれる）＋子プロセスの実掃除」の一気通貫を実バイナリで通す。
//
// idle=3s は knob（DROVER_IDLE）経由＝本番既定 30s と同じコード経路。
// 旧コード（knob 未配線＝常に 30s）では viewer 切断が 20s 窓に来ず本テストは
// FAIL する（鉄則: 旧コードで FAIL を実確認してから knob を配線した）。
func TestE2EWebTerminalQuiescence(t *testing.T) {
	if os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
		t.Skip("SKIP: gcloud / Java21+ 不在のため Firestore emulator 検証不可")
	}
	requireBridge(t)
	srv, hc := startHerdr(t)
	relayURL := startLocalRelay(t, droverCloudRoot(t))
	bin := buildBinary(t)

	const qPCID = "e2e-webterm-q-herdr" // 他 e2e と分離（wake/doc 干渉防止）
	st, err := state.New(context.Background(), projectID, "e2e-webterm-q-viewer")
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	// --- 実 agent 起動（TestE2EWebTerminal と同じ隔離 env＋DROVER_IDLE=3s）---
	agent := exec.Command(bin, "agent")
	agent.Env = []string{
		"HOME=" + t.TempDir(),
		"PATH=" + os.Getenv("PATH"),
		"GCP_PROJECT=" + projectID,
		"PC_ID=" + qPCID,
		"DROVER_TICK=1s",
		"DROVER_IDLE=3s", // quiescence を e2e 時間内へ（経路は本番と同一）
		"FIRESTORE_EMULATOR_HOST=" + os.Getenv("FIRESTORE_EMULATOR_HOST"),
		"HERDR_SOCKET_PATH=" + srv.sock,
		"CLOUD_RELAY_URL=" + relayURL,
	}
	var agentErr, agentOut syncBuf
	agent.Stderr = &agentErr
	agent.Stdout = &agentOut
	if err := agent.Start(); err != nil {
		t.Fatalf("agent start: %v", err)
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- agent.Wait() }()
	agentDead := false
	t.Cleanup(func() {
		if !agentDead {
			_ = agent.Process.Kill()
			<-waitCh
		}
	})
	dead := func() bool {
		select {
		case err := <-waitCh:
			agentDead = true
			t.Fatalf("agent が早期終了: %v\nstderr:\n%s\nstdout:\n%s", err, agentErr.String(), agentOut.String())
			return true
		default:
			return false
		}
	}
	waitFor(t, 20*time.Second, "pcs/"+qPCID+" registered", func() (bool, error) {
		if dead() {
			return false, nil
		}
		pcs, err := st.ListPCs(context.Background())
		if err != nil {
			return false, err
		}
		for _, p := range pcs {
			if p == qPCID {
				return true, nil
			}
		}
		return false, fmt.Errorf("pcs=%v", pcs)
	})

	// --- 対象 pane＋機械 viewer → RESIZE → wake → フレーム到達 ---
	ws, err := hc.WorkspaceCreate()
	if err != nil {
		t.Fatalf("workspace.create: %v", err)
	}
	paneID := ws.RootPane.PaneID
	t.Cleanup(func() { _ = hc.WorkspaceClose(ws.Workspace.WorkspaceID) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	viewer, recv, viewerClosed := dialViewer(t, ctx, relayURL, paneID)
	if _, err := viewer.Write(resizeFrame(40, 120)); err != nil {
		t.Fatalf("viewer RESIZE write: %v", err)
	}
	if err := st.Wake(ctx, qPCID, paneID); err != nil {
		t.Fatalf("Wake: %v", err)
	}
	waitFor(t, 30*time.Second, "observe frame が viewer に届く（\\x1b[?2026h）", func() (bool, error) {
		if dead() {
			return false, nil
		}
		return bytes.Contains(recv(), []byte("\x1b[?2026h")), fmt.Errorf(
			"received=%d bytes; agent stderr:\n%s", len(recv()), agentErr.String())
	})

	// --- observe subprocess の実在（agent 直下の子・exact 親子照合）---
	// フレーム到達直後＝observe 生存中。idle 3s より十分手前で確認する。
	if kids := observeChildren(t, agent.Process.Pid); len(kids) != 1 {
		t.Fatalf("observe subprocess が agent 直下に %d 個（期待 1）: %v", len(kids), kids)
	}

	// --- 無通信 → quiescence 自切断。source 自切断で relay がペアを畳み
	// viewer も切れる（relay「現役 conn の自然死は相手も Close」契約）---
	quietFrom := time.Now()
	select {
	case <-viewerClosed:
	case <-time.After(20 * time.Second):
		t.Fatalf("無通信 20s 経過（idle=3s）でも viewer が切断されない＝quiescence 自切断が動いていない\nstderr:\n%s", agentErr.String())
	}
	t.Logf("quiescence 切断まで %.1fs（idle=3s＋検査 tick idle/2）", time.Since(quietFrom).Seconds())

	// 物証: bridge の quiescence ログ（webterm.go の Logf 書式と exact-match）
	// と正常終了ログ（Run が nil で戻った＝エラー扱いでない）。ログ書込は
	// conn 切断より僅かに後になり得るため waitFor で待つ。
	wantQuiet := fmt.Sprintf("bridge sid=%q: quiescence: 3s 無通信＝データ線を自切断", paneID)
	wantEnd := fmt.Sprintf("bridge 終了 sid=%q（正常）", paneID)
	waitFor(t, 5*time.Second, "quiescence＋正常終了ログ", func() (bool, error) {
		if dead() {
			return false, nil
		}
		s := agentErr.String()
		return strings.Contains(s, wantQuiet) && strings.Contains(s, wantEnd),
			fmt.Errorf("stderr:\n%s", s)
	})

	// --- 掃除: observe subprocess が agent 直下から消える（リーク無し）---
	waitFor(t, 10*time.Second, "observe subprocess の掃除（リーク無し）", func() (bool, error) {
		if dead() {
			return false, nil
		}
		kids := observeChildren(t, agent.Process.Pid)
		return len(kids) == 0, fmt.Errorf("残存: %v", kids)
	})

	// --- 復帰: 次の wake で新 bridge が張れる（active map 解放の物証。
	// M9 push 自動復帰のサーバ側前提＝「切断は解放であって故障ではない」）---
	viewer2, recv2, _ := dialViewer(t, ctx, relayURL, paneID)
	if _, err := viewer2.Write(resizeFrame(40, 120)); err != nil {
		t.Fatalf("viewer2 RESIZE write: %v", err)
	}
	if err := st.Wake(ctx, qPCID, paneID); err != nil {
		t.Fatalf("Wake(復帰): %v", err)
	}
	waitFor(t, 30*time.Second, "復帰 wake で新フレームが届く", func() (bool, error) {
		if dead() {
			return false, nil
		}
		return bytes.Contains(recv2(), []byte("\x1b[?2026h")), fmt.Errorf(
			"received=%d bytes; stderr:\n%s", len(recv2()), agentErr.String())
	})
	if n := strings.Count(agentErr.String(), fmt.Sprintf("bridge 開始 sid=%q", paneID)); n != 2 {
		t.Fatalf("bridge 開始が %d 回（期待 2 回＝初回＋quiescence 後の復帰）\nstderr:\n%s", n, agentErr.String())
	}

	// --- SIGTERM graceful（bridge 稼働中でも確実に畳む）---
	if err := agent.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	select {
	case err := <-waitCh:
		agentDead = true
		if err != nil {
			t.Fatalf("SIGTERM で exit 0 にならない: %v\nstderr:\n%s", err, agentErr.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("SIGTERM 後 15s 経っても agent が終了しない\nstderr:\n%s", agentErr.String())
	}
	t.Logf("agent stderr:\n%s", agentErr.String())
}
