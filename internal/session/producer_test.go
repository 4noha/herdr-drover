//go:build !windows

// producer の検証は鉄則どおり実物 2 系で行う（合成だけで緑にしない）:
//   - 実 herdr 隔離サーバ（client_test.go と同じ確定レシピ: 短い /tmp dir
//     ＝sun_path 104B 制約・XDG_CONFIG_HOME 隔離・停止は自 socket への
//     server stop → 自 spawn PID のみ kill。裸の pkill は恒久禁止）
//   - 実 Firestore エミュレータ（state_test.go の自前起動パターンを流用）
//
// 加えて「scan エラー tick で state を一切呼ばない」等の不変条件は
// fake StateClient の呼出記録で機械確認する（dial エラー自体は実物:
// 存在しない socket への実 dial 失敗を使う）。
package session

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/4noha/drover-cloud/state"
	"github.com/4noha/herdr-drover/internal/herdrapi"
)

// *state.Client が StateClient 契約を満たすことをコンパイル時に固定
// （インターフェースのシグネチャずれを実装変更時に即検出）。
var _ StateClient = (*state.Client)(nil)
var _ HerdrClient = (*herdrapi.Client)(nil)

const projectID = "demo-hd"

// ============ 実 Firestore エミュレータ（state_test.go の流用） ============

// java21Bin は Java 21+ の bin ディレクトリ（Firestore emulator 要件）。
func java21Bin() string {
	cands := []string{
		"/opt/homebrew/opt/openjdk/bin",
		"/opt/homebrew/opt/openjdk@25/bin",
		"/opt/homebrew/opt/openjdk@21/bin",
	}
	for _, d := range cands {
		j := d + "/java"
		if fi, err := os.Stat(j); err == nil && !fi.IsDir() {
			out, _ := exec.Command(j, "-version").CombinedOutput()
			s := string(out)
			for _, v := range []string{"\"21", "\"22", "\"23", "\"24", "\"25", "\"26"} {
				if strings.Contains(s, v) {
					return d
				}
			}
		}
	}
	return ""
}

func freePort() int {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

var emuCmd *exec.Cmd

// TestMain はエミュレータが起動できれば FIRESTORE_EMULATOR_HOST を立てる。
// 起動できない環境でも純関数/fake テストは走らせたいので exit 0 にはせず、
// エミュレータ必須テスト側が newStateClient() で Skip する（state_test.go
// より粒度を細かくした点）。
func TestMain(m *testing.M) {
	jbin := java21Bin()
	if _, err := exec.LookPath("gcloud"); err == nil && jbin != "" {
		port := freePort()
		host := fmt.Sprintf("127.0.0.1:%d", port)
		emuCmd = exec.Command("gcloud", "beta", "emulators", "firestore", "start",
			"--host-port="+host, "--quiet")
		emuCmd.Env = append(os.Environ(),
			"PATH="+jbin+":"+os.Getenv("PATH"),
			"CLOUDSDK_CORE_DISABLE_PROMPTS=1")
		emuCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := emuCmd.Start(); err == nil {
			for i := 0; i < 80; i++ { // 最大 40s
				if c, err := http.Get("http://" + host + "/"); err == nil {
					c.Body.Close()
					os.Setenv("FIRESTORE_EMULATOR_HOST", host)
					break
				}
				time.Sleep(500 * time.Millisecond)
			}
			if os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
				_ = syscall.Kill(-emuCmd.Process.Pid, syscall.SIGKILL)
				emuCmd = nil
			}
		} else {
			emuCmd = nil
		}
	}
	code := m.Run()
	if emuCmd != nil {
		_ = syscall.Kill(-emuCmd.Process.Pid, syscall.SIGKILL)
	}
	os.Exit(code)
}

// newStateClient はエミュレータ接続の実 state.Client（無ければ Skip）。
func newStateClient(t *testing.T, pc string) *state.Client {
	t.Helper()
	if os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
		t.Skip("SKIP: gcloud / Java21+ 不在のため Firestore emulator 検証不可")
	}
	c, err := state.New(context.Background(), projectID, pc)
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// ============ 実 herdr 隔離サーバ harness（client_test.go と同レシピ） ============

type testServer struct {
	t    *testing.T
	bin  string
	dir  string
	sock string
	env  []string
	cmd  *exec.Cmd
}

func startHerdr(t *testing.T) (*testServer, *herdrapi.Client) {
	t.Helper()
	bin, err := exec.LookPath("herdr")
	if err != nil {
		t.Skip("herdr not installed; skipping real-server test")
	}
	dir, err := os.MkdirTemp("/tmp", "hd")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	xdg := filepath.Join(dir, "xdg")
	if err := os.MkdirAll(xdg, 0o700); err != nil {
		t.Fatalf("mkdir xdg: %v", err)
	}
	s := &testServer{
		t:    t,
		bin:  bin,
		dir:  dir,
		sock: filepath.Join(dir, "h.sock"),
		env: append(os.Environ(),
			"HERDR_SOCKET_PATH="+filepath.Join(dir, "h.sock"),
			"XDG_CONFIG_HOME="+xdg),
	}
	t.Cleanup(func() {
		s.stop()
		os.RemoveAll(dir)
	})
	s.startProcess()
	return s, herdrapi.New(s.sock)
}

func (s *testServer) startProcess() {
	s.t.Helper()
	cmd := exec.Command(s.bin, "server")
	cmd.Env = s.env
	if err := cmd.Start(); err != nil {
		s.t.Fatalf("start herdr server: %v", err)
	}
	s.cmd = cmd
	c := herdrapi.New(s.sock)
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := c.Ping(); err == nil {
			return
		}
		if time.Now().After(deadline) {
			s.stop()
			s.t.Fatalf("herdr server did not become ready at %s", s.sock)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// stop は自分の spawn したサーバだけを止める（graceful stop → 5s 待ち →
// 自 PID kill の backstop。他プロセスには触れない＝pkill 禁止の鉄則）。
func (s *testServer) stop() {
	if s.cmd == nil {
		return
	}
	stop := exec.Command(s.bin, "server", "stop")
	stop.Env = s.env
	_ = stop.Run()

	done := make(chan error, 1)
	go func() { done <- s.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = s.cmd.Process.Kill()
		<-done
	}
	s.cmd = nil
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() (bool, error)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		ok, err := cond()
		if ok {
			return
		}
		lastErr = err
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s (last err: %v)", what, lastErr)
}

// docsByKey は ListSessions を key→doc に引き直す（読み戻し用）。
func docsByKey(t *testing.T, st *state.Client, pc string) map[string]map[string]any {
	t.Helper()
	ss, err := st.ListSessions(context.Background(), pc)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	out := make(map[string]map[string]any, len(ss))
	for _, s := range ss {
		out[state.SessionKeyOf(s)] = s
	}
	return out
}

// ============ e2e: 実 herdr → Tick → 実エミュレータ read-back ============

// TestTickE2ELifecycle は producer の主経路を実物往復で検証する:
// pane/agent 作成 → Tick → Firestore read-back（スキーマ・U+2010 short_dir・
// window_name 優先順位）→ 無差分 tick で version 収束（content_hash ゲート）
// → pane 終了 → Tick → doc 消滅（DeleteSession 同期）。
func TestTickE2ELifecycle(t *testing.T) {
	_, hc := startHerdr(t)
	st := newStateClient(t, "hd-prod-e2e")
	ctx := context.Background()
	p := NewProducer(hc, st)

	// U+2010（多バイト・cm の lsof \xNN 化け実バグで問題になった文字）を
	// 含む実ディレクトリを cwd にした agent pane。short_dir の多バイト
	// 安全を実データで検証する。
	mbDir := "/tmp/hd‐間dir" // U+2010 ＋ 漢字
	if err := os.MkdirAll(mbDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", mbDir, err)
	}
	t.Cleanup(func() { os.RemoveAll(mbDir) })

	ag, err := hc.AgentStart("hdprod", []string{"/bin/sleep", "300"},
		&herdrapi.AgentStartOptions{Cwd: mbDir})
	if err != nil {
		t.Fatalf("agent.start: %v", err)
	}
	ws, err := hc.WorkspaceCreate()
	if err != nil {
		t.Fatalf("workspace.create: %v", err)
	}
	shellPane := ws.RootPane.PaneID
	if ws.Workspace.WorkspaceID == ag.WorkspaceID {
		t.Fatalf("前提崩れ: agent と root pane が同一 workspace（%s）", ag.WorkspaceID)
	}
	waitFor(t, 5*time.Second, "both panes in pane.list", func() (bool, error) {
		panes, err := hc.PaneList()
		if err != nil {
			return false, err
		}
		got := map[string]bool{}
		for _, pn := range panes {
			got[pn.PaneID] = true
		}
		return got[ag.PaneID] && got[shellPane], nil
	})

	if err := p.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	docs := docsByKey(t, st, "hd-prod-e2e")
	if len(docs) != 2 {
		t.Fatalf("doc 数 2 のはず: %v", docs)
	}
	a := docs[ag.PaneID]
	if a == nil {
		t.Fatalf("agent pane doc が無い: %v", docs)
	}
	// スキーマ互換（key/session_id=pane_id・cwd・short_dir・window_name・
	// is_active・PushStatus 付与の version/content_hash/updated_at）。
	if a["session_id"] != ag.PaneID || a["key"] != ag.PaneID {
		t.Fatalf("key/session_id が pane_id でない: %v", a)
	}
	// /tmp は macOS で /private/tmp の symlink＝cwd の先頭は環境次第だが
	// 末尾成分は不変（short_dir 検証には影響しない）。
	if sd := a["short_dir"]; sd != "hd‐間dir" {
		t.Fatalf("U+2010 含む short_dir が壊れた: %q (cwd=%q)", sd, a["cwd"])
	}
	if cwd, _ := a["cwd"].(string); !strings.HasSuffix(cwd, "/hd‐間dir") {
		t.Fatalf("cwd 不整合: %q", a["cwd"])
	}
	if a["window_name"] != "hdprod" {
		t.Fatalf("agent pane の window_name は agent 名のはず: %v", a["window_name"])
	}
	// is_active は agent_status の exact-match 写像（実サーバの現在値が
	// unknown/idle/working いずれでも「写像の一貫性」は常に成立するはず）。
	stStr, _ := a["agent_status"].(string)
	if act, ok := a["is_active"].(bool); !ok || act != (stStr == "working") {
		t.Fatalf("is_active(%v) が agent_status(%q) と不整合", a["is_active"], stStr)
	}
	t.Logf("agent pane doc: agent_status=%q is_active=%v", stStr, a["is_active"])
	if a["version"] == nil || a["content_hash"] == nil || a["updated_at"] == nil {
		t.Fatalf("PushStatus 契約フィールド欠落: %v", a)
	}
	b := docs[shellPane]
	if b == nil {
		t.Fatalf("shell pane doc が無い: %v", docs)
	}
	// label 無し・agent 無しの素 pane は pane_id が window_name（推測しない
	// 決定的フォールバック）。
	if b["window_name"] != shellPane {
		t.Fatalf("素 pane の window_name は pane_id のはず: %v", b["window_name"])
	}

	// ---- content_hash ゲート: 無差分 tick を重ねると version が収束する ----
	// 揮発フィールド（revision/scroll/focused/terminal_id）を session map に
	// 混入させる実装だと version が tick 毎に単調増加して永久に収束しない
	// ＝near-$0 破壊をここで検出する。
	versions := func() map[string]int64 {
		out := map[string]int64{}
		for k, d := range docsByKey(t, st, "hd-prod-e2e") {
			v, _ := d["version"].(int64)
			out[k] = v
		}
		return out
	}
	stable := false
	prevV := versions()
	for i := 0; i < 10 && !stable; i++ {
		if err := p.Tick(ctx); err != nil {
			t.Fatalf("Tick(#%d): %v", i+2, err)
		}
		curV := versions()
		stable = len(curV) == len(prevV)
		for k, v := range curV {
			if prevV[k] != v {
				stable = false
			}
		}
		prevV = curV
	}
	if !stable {
		t.Fatalf("10 tick しても version が収束しない（content_hash ゲート不全/揮発フィールド混入）: %v", prevV)
	}

	// ---- pane 終了 → Tick → doc 消滅（部分消滅→全消滅の順で検証） ----
	if err := hc.WorkspaceClose(ws.Workspace.WorkspaceID); err != nil {
		t.Fatalf("workspace.close(shell): %v", err)
	}
	waitFor(t, 5*time.Second, "shell pane gone from pane.list", func() (bool, error) {
		panes, err := hc.PaneList()
		if err != nil {
			return false, err
		}
		for _, pn := range panes {
			if pn.PaneID == shellPane {
				return false, fmt.Errorf("still listed")
			}
		}
		return true, nil
	})
	if err := p.Tick(ctx); err != nil {
		t.Fatalf("Tick(after close shell): %v", err)
	}
	docs = docsByKey(t, st, "hd-prod-e2e")
	if docs[shellPane] != nil {
		t.Fatalf("終了 pane の doc が残存（DeleteSession 不成立）: %v", docs)
	}
	if docs[ag.PaneID] == nil {
		t.Fatalf("生存 pane の doc が消えた（削除しすぎ）: %v", docs)
	}

	if err := hc.WorkspaceClose(ag.WorkspaceID); err != nil {
		t.Fatalf("workspace.close(agent): %v", err)
	}
	waitFor(t, 5*time.Second, "all panes gone", func() (bool, error) {
		panes, err := hc.PaneList()
		if err != nil {
			return false, err
		}
		return len(panes) == 0, fmt.Errorf("%d panes", len(panes))
	})
	// 正常な「空 scan」は空 STATUS flap とは別物＝削除は実行される。
	if err := p.Tick(ctx); err != nil {
		t.Fatalf("Tick(after close all): %v", err)
	}
	if docs = docsByKey(t, st, "hd-prod-e2e"); len(docs) != 0 {
		t.Fatalf("全終了後も doc 残存: %v", docs)
	}
}

// TestTickScanErrorKeepsCloudState は cm STATUS flap 事故の再発防止を実物で
// 検証する: herdr サーバ死亡（実 dial 失敗）の tick では push も delete も
// 起きず、Firestore の前回状態がそのまま残る。
func TestTickScanErrorKeepsCloudState(t *testing.T) {
	s, hc := startHerdr(t)
	st := newStateClient(t, "hd-prod-err")
	ctx := context.Background()
	p := NewProducer(hc, st)

	ws, err := hc.WorkspaceCreate()
	if err != nil {
		t.Fatalf("workspace.create: %v", err)
	}
	paneID := ws.RootPane.PaneID
	if err := p.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if docs := docsByKey(t, st, "hd-prod-err"); docs[paneID] == nil {
		t.Fatalf("前提: doc が出来ていない: %v", docs)
	}

	// サーバ停止 → scan は実 dial エラーになる。
	s.stop()
	err = p.Tick(ctx)
	if err == nil {
		t.Fatal("サーバ死亡なのに Tick がエラーを返さない")
	}
	t.Logf("scan error (期待どおり): %v", err)
	// 前回状態維持: doc は消えない（旧 cm の「エラーを空扱い→全削除」だと
	// ここで落ちる）。
	if docs := docsByKey(t, st, "hd-prod-err"); docs[paneID] == nil {
		t.Fatalf("scan エラー tick で doc が消えた（flap 再発）: %v", docs)
	}
	// prev（削除判定の基礎）も維持されている（白箱確認）。
	if !p.prev[paneID] {
		t.Fatalf("scan エラー tick で prev が壊れた: %v", p.prev)
	}
}

// TestTickServerRestartNoFlap は「herdr サーバ再起動窓で成功した空/部分
// pane.list が返り、削除→再作成 churn になる」仮説（レビュー指摘）を実
// サーバで常設検証する。実測（v0.7.4・graceful/SIGKILL 各 5 回・3ms poll・
// pane 10 個）では、herdr は session.json の復元が完了するまで API socket
// への dial 自体を受け付けず、観測は常に「dial 失敗 → 完全な list」の直接
// 遷移だった＝空/部分 list の窓は存在しない。よって producer 側の遅延削除
// （1 tick confirm）は不要と裁定し、代わりに本テストがその前提を将来の
// herdr version 変化に対して見張る（窓が生まれれば delCalls>0 で FAIL）。
func TestTickServerRestartNoFlap(t *testing.T) {
	s, hc := startHerdr(t)
	rec := &recordingState{}
	p := NewProducer(hc, rec)
	ctx := context.Background()

	// pane 3 つ（複数にするのは「部分 list」も検出対象にするため）
	want := map[string]bool{}
	for i := 0; i < 3; i++ {
		ws, err := hc.WorkspaceCreate()
		if err != nil {
			t.Fatalf("workspace.create: %v", err)
		}
		want[ws.RootPane.PaneID] = true
	}
	waitFor(t, 5*time.Second, "3 panes in pane.list", func() (bool, error) {
		panes, err := hc.PaneList()
		if err != nil {
			return false, err
		}
		return len(panes) == 3, fmt.Errorf("%d panes", len(panes))
	})
	if err := p.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if len(p.prev) != 3 {
		t.Fatalf("前提: prev が 3 でない: %v", p.prev)
	}

	restart := func(label string, kill bool) {
		// 停止（graceful は server stop・crash 相当は SIGKILL）
		if kill {
			_ = s.cmd.Process.Kill()
			_, _ = s.cmd.Process.Wait()
			s.cmd = nil
		} else {
			s.stop()
		}
		// 停止中の tick は実 dial 失敗＝skip（前回状態維持）を先に固定
		if err := p.Tick(ctx); err == nil {
			t.Fatalf("[%s] サーバ停止中なのに Tick が成功した", label)
		}
		// 再 spawn（ping 待ちはしない: 起動窓そのものを Tick 連打で突く）
		cmd := exec.Command(s.bin, "server")
		cmd.Env = s.env
		if err := cmd.Start(); err != nil {
			t.Fatalf("[%s] restart herdr server: %v", label, err)
		}
		s.cmd = cmd
		deadline := time.Now().Add(20 * time.Second)
		for {
			err := p.Tick(ctx)
			if err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("[%s] 再起動後 20s 経っても Tick が成功しない: %v", label, err)
			}
			// sleep なしの連打＝起動直後の最初の応答を最速で観測する
		}
		// 最初に成功した scan の時点で既に全 pane が見えている（＝空/部分
		// list の窓なし）。窓があれば消滅キー扱いの DeleteSession が走る。
		if rec.delCalls != 0 {
			t.Fatalf("[%s] 再起動窓で DeleteSession が呼ばれた（flap）: deleted=%v", label, rec.deleted)
		}
		if len(p.prev) != len(want) {
			t.Fatalf("[%s] 再起動直後の scan が部分 list: prev=%v want=%v", label, p.prev, want)
		}
		for k := range want {
			if !p.prev[k] {
				t.Fatalf("[%s] pane %s が再起動後の scan に居ない: prev=%v", label, k, p.prev)
			}
		}
	}
	restart("graceful", false)
	restart("sigkill", true)
}

// ============ fake での不変条件・純関数テスト（実物テストの補完） ============

// recordingState は呼出しを記録する fake（scan エラー時の「一切呼ばない」
// を厳密に機械確認する用。正経路の検証は上の実エミュレータが担う）。
type recordingState struct {
	pushCalls int
	delCalls  int
	ownCalls  int
	delErr    error
	deleted   []string
	seedKeys  []string
}

func (r *recordingState) PushStatus(ctx context.Context, sessions []map[string]any) (int, error) {
	r.pushCalls++
	return len(sessions), nil
}
func (r *recordingState) DeleteSession(ctx context.Context, key string) error {
	r.delCalls++
	if r.delErr != nil {
		return r.delErr
	}
	r.deleted = append(r.deleted, key)
	return nil
}
func (r *recordingState) OwnSessionKeys(ctx context.Context) ([]string, error) {
	r.ownCalls++
	return r.seedKeys, nil
}

// fakeHerdr は Tick のエラー分岐・差分ロジック検証用（delete 再試行など、
// 実サーバではタイミング制御が難しい分岐のみ担当）。
type fakeHerdr struct {
	panes    []herdrapi.PaneInfo
	agents   []herdrapi.AgentInfo
	paneErr  error
	agentErr error
}

func (f *fakeHerdr) PaneList() ([]herdrapi.PaneInfo, error)   { return f.panes, f.paneErr }
func (f *fakeHerdr) AgentList() ([]herdrapi.AgentInfo, error) { return f.agents, f.agentErr }

// TestTickScanErrorMakesNoStateCalls: 存在しない socket への**実 dial 失敗**
// で、state 側が 1 回も呼ばれないことを呼出記録で確認（seed すら呼ばない
// ＝scan 成功が全ての前提）。
func TestTickScanErrorMakesNoStateCalls(t *testing.T) {
	dir := t.TempDir()
	hc := herdrapi.New(filepath.Join(dir, "no-such.sock"))
	rec := &recordingState{}
	p := NewProducer(hc, rec)
	if err := p.Tick(context.Background()); err == nil {
		t.Fatal("dial 不能なのに Tick がエラーを返さない")
	}
	if rec.pushCalls != 0 || rec.delCalls != 0 || rec.ownCalls != 0 {
		t.Fatalf("scan エラー tick で state が呼ばれた: push=%d del=%d own=%d",
			rec.pushCalls, rec.delCalls, rec.ownCalls)
	}
}

// TestTickAgentListErrorAlsoSkips: pane.list 成功でも agent.list 失敗なら
// skip（部分結果で push すると agent pane の name/status が素値に退行した
// 「偽の差分」を書く）。
func TestTickAgentListErrorAlsoSkips(t *testing.T) {
	fh := &fakeHerdr{
		panes:    []herdrapi.PaneInfo{{PaneID: "w1:p1", Cwd: "/tmp/x", AgentStatus: "unknown"}},
		agentErr: fmt.Errorf("boom"),
	}
	rec := &recordingState{}
	p := NewProducer(fh, rec)
	if err := p.Tick(context.Background()); err == nil {
		t.Fatal("agent.list 失敗なのに Tick がエラーを返さない")
	}
	if rec.pushCalls != 0 || rec.delCalls != 0 || rec.ownCalls != 0 {
		t.Fatalf("agent.list エラー tick で state が呼ばれた: %+v", rec)
	}
}

// TestTickSeedAndDeleteRetry: 起動時 seed（OwnSessionKeys）に居て今 scan に
// 居ないキーは初回 tick で削除される（agent 停止中の終了を取りこぼさない）。
// delete 失敗キーは prev へ持ち越され次 tick で再試行される（cm では握り
// 潰しで幽霊 doc になり得た点の改善）。
func TestTickSeedAndDeleteRetry(t *testing.T) {
	fh := &fakeHerdr{
		panes: []herdrapi.PaneInfo{{PaneID: "w1:p1", Cwd: "/tmp/x", AgentStatus: "idle"}},
	}
	rec := &recordingState{seedKeys: []string{"w1:p1", "w9:p9"}, delErr: fmt.Errorf("transient")}
	p := NewProducer(fh, rec)

	// 1 tick 目: w9:p9 が消滅キー → delete 試行（失敗）→ 持ち越し
	if err := p.Tick(context.Background()); err == nil {
		t.Fatal("delete 失敗が error として返らない")
	}
	if rec.delCalls != 1 || !p.prev["w9:p9"] {
		t.Fatalf("delete 失敗キーが持ち越されない: calls=%d prev=%v", rec.delCalls, p.prev)
	}
	// 2 tick 目: 復旧 → 再試行で削除成功 → prev から消える
	rec.delErr = nil
	if err := p.Tick(context.Background()); err != nil {
		t.Fatalf("Tick(2): %v", err)
	}
	if len(rec.deleted) != 1 || rec.deleted[0] != "w9:p9" || p.prev["w9:p9"] {
		t.Fatalf("delete 再試行が働かない: deleted=%v prev=%v", rec.deleted, p.prev)
	}
	if rec.ownCalls != 1 {
		t.Fatalf("seed は初回のみのはず: ownCalls=%d", rec.ownCalls)
	}
}

// TestBuildSessions は写像の決定性（exact-match・優先順位・除外）を
// 実採取形の PaneInfo/AgentInfo（types.go コメントの実フィールド）で確認。
func TestBuildSessions(t *testing.T) {
	panes := []herdrapi.PaneInfo{
		{PaneID: "w1:p2", Cwd: "/Users/x/works/proj", AgentStatus: "working"},
		{PaneID: "w1:p1", Cwd: "/tmp", AgentStatus: "unknown", Label: "mylabel"},
		{PaneID: "w2:p1", Cwd: "", AgentStatus: "idle"},
		{PaneID: "", Cwd: "/tmp"}, // pane_id 無し＝同期キー不能→捏造せず除外
	}
	agents := []herdrapi.AgentInfo{
		{PaneID: "w1:p2", Name: "claude", AgentStatus: "working"},
	}
	ss := BuildSessions(panes, agents)
	if len(ss) != 3 {
		t.Fatalf("3 sessions のはず: %v", ss)
	}
	// key 昇順で決定的
	if ss[0]["key"] != "w1:p1" || ss[1]["key"] != "w1:p2" || ss[2]["key"] != "w2:p1" {
		t.Fatalf("順序が決定的でない: %v", ss)
	}
	// label 優先（agent 無し pane）
	if ss[0]["window_name"] != "mylabel" || ss[0]["is_active"] != false {
		t.Fatalf("label pane: %v", ss[0])
	}
	// agent 名優先＋working→is_active true（exact-match）
	if ss[1]["window_name"] != "claude" || ss[1]["is_active"] != true ||
		ss[1]["agent_status"] != "working" || ss[1]["short_dir"] != "proj" {
		t.Fatalf("agent pane: %v", ss[1])
	}
	// cwd 空→short_dir "unknown"（cm parity）・素 pane は pane_id 名
	if ss[2]["short_dir"] != "unknown" || ss[2]["window_name"] != "w2:p1" ||
		ss[2]["is_active"] != false {
		t.Fatalf("empty-cwd pane: %v", ss[2])
	}
}

// TestShortDir は多バイト安全（U+2010・全角）と cm parity の縁ケース。
func TestShortDir(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/a/b/c", "c"},
		{"/a/b/c/", "c"},
		{"/", "unknown"},
		{"", "unknown"},
		{"/Users/x/claude‐master-go", "claude‐master-go"}, // U+2010（lsof \xNN 化けの実バグ文字）
		{"/tmp/日本語ディレクトリ", "日本語ディレクトリ"},
		{"rel/dir", "dir"},
	}
	for _, c := range cases {
		if got := ShortDir(c.in); got != c.want {
			t.Errorf("ShortDir(%q)=%q want %q", c.in, got, c.want)
		}
	}
}
