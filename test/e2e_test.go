//go:build !windows

// Package e2e は Phase 1 gate のミニチュア（常設・実環境）:
//
//	実 herdr 隔離サーバ ＋ 実 Firestore エミュレータ ＋ **実バイナリ**
//	（go build した herdr-drover を `agent` サブコマンドで spawn）で
//	  起動 → pane 作成 → pcs/e2e-herdr/sessions/{pane_id} doc 出現
//	  → pane 終了 → doc 消滅 → SIGTERM で graceful 終了（exit 0・pidfile 掃除）
//	を Firestore read-back で検証する。
//
// internal/session の producer e2e が「ライブラリ経路（Tick 直呼び）」を
// 深く検証するのに対し、ここは「プロセス経路」（env 解決 → pidfile →
// ping → RegisterPC → runAgentLoop → シグナル終了）を実バイナリで通す
// ＝launchd 常駐と同じ形の一気通貫。鉄則どおり合成ストリームは使わない
// （herdr / gcloud エミュレータ不在の環境のみ Skip）。
package e2e

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/4noha/herdr-drover/internal/cloud/state"
	"github.com/4noha/herdr-drover/internal/herdrapi"
)

// projectID はエミュレータ内の隔離プロジェクト（demo- prefix はエミュレータ
// 慣習＝実 GCP に絶対に当たらない名前空間）。
const projectID = "demo-hd-e2e"

// pcID は本ゲートの決め打ち PC id。-herdr サフィックスは DESIGN 決定事項
// （cm agent と同一 id にすると DeleteSession 削除合戦）＝warnConfig も沈黙。
const pcID = "e2e-herdr"

// ============ 実 Firestore エミュレータ（state_test.go の確定パターン） ============

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

// TestMain はエミュレータが起動できれば FIRESTORE_EMULATOR_HOST を立てる
// （in-process read-back と子プロセス agent の両方が同じ値を使う）。
// 起動できない環境では各テストが Skip する（exit 0 で握り潰さない＝
// producer_test.go と同じ粒度）。
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

// ============ 実 herdr 隔離サーバ（producer_test.go と同じ確定レシピ） ============

type testServer struct {
	t    *testing.T
	bin  string
	sock string
	env  []string
	cmd  *exec.Cmd
}

// startHerdr は隔離 herdr サーバを起動する。短い /tmp dir（sun_path 104B
// 制約）＋XDG_CONFIG_HOME 隔離。herdr 不在の環境は Skip（CI 耐性）。
// extraEnv は KEY=VALUE を server と CLI（testServer.env）へ追加する
// （重複キーは後勝ち＝os/exec の仕様。plugin link gate が HOME/XDG_STATE_HOME
// を隔離するために使う。既存呼出しは無引数＝挙動不変）。
func startHerdr(t *testing.T, extraEnv ...string) (*testServer, *herdrapi.Client) {
	t.Helper()
	bin, err := exec.LookPath("herdr")
	if err != nil {
		t.Skip("herdr not installed; skipping real-server e2e")
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
		sock: filepath.Join(dir, "h.sock"),
		env: append(append(os.Environ(),
			"HERDR_SOCKET_PATH="+filepath.Join(dir, "h.sock"),
			"XDG_CONFIG_HOME="+xdg), extraEnv...),
	}
	t.Cleanup(func() {
		s.stop()
		os.RemoveAll(dir)
	})
	cmd := exec.Command(bin, "server")
	cmd.Env = s.env
	if err := cmd.Start(); err != nil {
		t.Fatalf("start herdr server: %v", err)
	}
	s.cmd = cmd
	c := herdrapi.New(s.sock)
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := c.Ping(); err == nil {
			return s, c
		}
		if time.Now().After(deadline) {
			s.stop()
			t.Fatalf("herdr server did not become ready at %s", s.sock)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// stop は自分の spawn したサーバだけを graceful stop → 5s → 自 PID kill で
// 止める（裸の pkill herdr は恒久禁止＝他者のサーバを殺した実インシデント）。
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

// ============ ヘルパ ============

// moduleRoot はこのテストファイル位置からリポジトリルートを解く
// （test/ の親。go test 実行時の cwd に依存しない）。
func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Dir(filepath.Dir(file))
}

// buildBinary は実バイナリを一時 dir へ go build する（go run でなく build
// なのは、SIGTERM を **agent プロセス自身**へ確実に届けて graceful 経路を
// 検証するため。go run は仲介プロセスが挟まりシグナル伝播が別問題になる）。
func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "herdr-drover")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/herdr-drover")
	cmd.Dir = moduleRoot(t)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
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

// sessionDoc は pcs/{pc}/sessions から key の doc を引く（無ければ nil）。
func sessionDoc(t *testing.T, st *state.Client, pc, key string) map[string]any {
	t.Helper()
	ss, err := st.ListSessions(context.Background(), pc)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	for _, s := range ss {
		if state.SessionKeyOf(s) == key {
			return s
		}
	}
	return nil
}

// ============ Phase 1 gate（実バイナリ一気通貫） ============

func TestE2EAgentLifecycle(t *testing.T) {
	if os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
		t.Skip("SKIP: gcloud / Java21+ 不在のため Firestore emulator 検証不可")
	}
	srv, hc := startHerdr(t)
	_ = srv
	bin := buildBinary(t)

	// read-back 用の in-process クライアント（agent と同じエミュレータ・
	// 同じ project を読む＝合成の期待値でなく実 Firestore の実態を見る）。
	st, err := state.New(context.Background(), projectID, pcID)
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	// agent 環境は明示構築で隔離する:
	//   - HOME=一時 dir → pidfile (~/.herdr-drover/agent.pid) が本物の常駐
	//     daemon と衝突しない（二重起動拒否で偽 FAIL しない）
	//   - 外側の GOOGLE_APPLICATION_CREDENTIALS / PC_ID 等を継承しない
	//     （実 GCP に書いてしまう事故の遮断。エミュレータは資格情報不要）
	tmpHome := t.TempDir()
	env := []string{
		"HOME=" + tmpHome,
		"PATH=" + os.Getenv("PATH"),
		"GCP_PROJECT=" + projectID,
		"PC_ID=" + pcID,
		"DROVER_TICK=1s",
		"FIRESTORE_EMULATOR_HOST=" + os.Getenv("FIRESTORE_EMULATOR_HOST"),
		"HERDR_SOCKET_PATH=" + srv.sock,
	}
	agent := exec.Command(bin, "agent")
	agent.Env = env
	// 正常経路は Wait 完了後にのみ読むが、SIGTERM timeout の失敗経路
	// （376 行目付近の Fatalf）は稼働中に String() を評価する＝syncBuf に
	// 統一（revoke_e2e_test.go の -race 実検出と同根）。
	var stderr, stdout syncBuf
	agent.Stderr = &stderr
	agent.Stdout = &stdout
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
	// 早期死亡（設定/接続エラー等）は waitFor の timeout より先に stderr 込みで
	// 報告する（診断の一次情報を隠さない）。
	dead := func() bool {
		select {
		case err := <-waitCh:
			agentDead = true
			t.Fatalf("agent が早期終了: %v\nstderr:\n%s\nstdout:\n%s", err, stderr.String(), stdout.String())
			return true
		default:
			return false
		}
	}

	// --- 起動: RegisterPC で pcs/{pc} 親 doc が出る（idle でも一覧に出す規律） ---
	waitFor(t, 20*time.Second, "pcs/"+pcID+" registered", func() (bool, error) {
		if dead() {
			return false, nil
		}
		pcs, err := st.ListPCs(context.Background())
		if err != nil {
			return false, err
		}
		for _, p := range pcs {
			if p == pcID {
				return true, nil
			}
		}
		return false, fmt.Errorf("pcs=%v", pcs)
	})

	// --- pane 作成 → doc 出現（DROVER_TICK=1s なので数秒で出るはず） ---
	ws, err := hc.WorkspaceCreate()
	if err != nil {
		t.Fatalf("workspace.create: %v", err)
	}
	paneID := ws.RootPane.PaneID
	var doc map[string]any
	waitFor(t, 15*time.Second, "session doc appears for "+paneID, func() (bool, error) {
		if dead() {
			return false, nil
		}
		doc = sessionDoc(t, st, pcID, paneID)
		return doc != nil, fmt.Errorf("doc not yet in pcs/%s/sessions", pcID)
	})
	// スキーマ点検は producer e2e が深く担う。ここではプロセス経路の同期
	// キーと PushStatus 契約フィールドだけ確認する。
	if doc["key"] != paneID || doc["session_id"] != paneID {
		t.Fatalf("doc の key/session_id が pane_id でない: %v", doc)
	}
	if doc["version"] == nil || doc["content_hash"] == nil || doc["updated_at"] == nil {
		t.Fatalf("PushStatus 契約フィールド欠落: %v", doc)
	}

	// --- pane 終了 → doc 消滅（in-memory 差分 → DeleteSession の実伝播） ---
	if err := hc.WorkspaceClose(ws.Workspace.WorkspaceID); err != nil {
		t.Fatalf("workspace.close: %v", err)
	}
	waitFor(t, 15*time.Second, "session doc removed for "+paneID, func() (bool, error) {
		if dead() {
			return false, nil
		}
		return sessionDoc(t, st, pcID, paneID) == nil, fmt.Errorf("doc still present")
	})

	// --- SIGTERM → graceful 終了（exit 0・pidfile 掃除） ---
	if err := agent.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	select {
	case err := <-waitCh:
		agentDead = true
		if err != nil {
			t.Fatalf("SIGTERM で exit 0 にならない: %v\nstderr:\n%s", err, stderr.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("SIGTERM 後 15s 経っても agent が終了しない\nstderr:\n%s", stderr.String())
	}
	// graceful 経路の物証: defer os.Remove(pidfile) が走っている
	// （SIGKILL 死だと stale pidfile が残る＝この確認が graceful を峻別する）。
	pidPath := filepath.Join(tmpHome, ".herdr-drover", "agent.pid")
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Fatalf("graceful 終了なのに pidfile が残存: %s (err=%v)\nstderr:\n%s", pidPath, err, stderr.String())
	}
	t.Logf("agent stderr:\n%s", stderr.String())
}
