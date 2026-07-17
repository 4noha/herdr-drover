//go:build !windows

package e2e

// 二重起動ゲートの実プロセス競走テスト（レビュー指摘の再発防止）。
//
// 旧コード（readPidfile→pidAlive→writePidfile の check-then-write＋固定
// path+".tmp"）は非原子で、実バイナリ 8 並行起動 ×10 ラウンドの実測で
//   - 9/10 ラウンド: 複数プロセスがゲートを同時通過（二重 agent 稼働）
//   - 10/10 ラウンド: 固定 tmp の共有で rename が ENOENT（"pidfile 書込失敗"）
// を再現した。二重 agent は producer の in-memory 差分検出を壊す（agent.go
// の不変条件）ため、ゲートは flock で原子化した。本テストは旧コードで FAIL
// することを確認済み（鉄則: 修正前に旧コードで落とす）。

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// startHangingSocket は accept するが一切応答しない unix socket を立てる。
// ゲート通過者は herdr ping でブロックする＝「勝者がゲートを保持したまま」
// の状態を決定論的に作れる（即失敗する socket だと勝者が exit→lock 解放→
// 次の候補が正当に通過し得て、同時稼働と逐次再起動を区別できない）。
func startHangingSocket(t *testing.T) string {
	t.Helper()
	// 短い /tmp dir（sun_path 104B 制約は client 側 dial にも効く）
	dir, err := os.MkdirTemp("/tmp", "hd")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	sock := filepath.Join(dir, "h.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	// 受けた conn は保持だけする（参照を手放すと GC finalizer が fd を閉じ、
	// ブロック中の ping が偽のエラー復帰をして勝者が exit してしまう）。
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		l.Close()
		mu.Lock()
		for _, c := range conns {
			c.Close()
		}
		mu.Unlock()
		os.RemoveAll(dir)
	})
	return sock
}

func TestE2EConcurrentStartSingleWinner(t *testing.T) {
	bin := buildBinary(t)
	sock := startHangingSocket(t)
	tmpHome := t.TempDir()
	env := []string{
		"HOME=" + tmpHome,
		"PATH=" + os.Getenv("PATH"),
		"GCP_PROJECT=dummy", // ゲートは ping より前＝Firestore には到達しない
		"HERDR_SOCKET_PATH=" + sock,
	}
	spawn := func() (*exec.Cmd, *bytes.Buffer) {
		cmd := exec.Command(bin, "agent")
		cmd.Env = env
		var eb bytes.Buffer
		cmd.Stderr = &eb
		if err := cmd.Start(); err != nil {
			t.Fatalf("agent start: %v", err)
		}
		return cmd, &eb
	}

	const n = 8
	type result struct {
		err    error
		stderr string
	}
	var mu sync.Mutex
	exited := map[int]result{}
	cmds := make([]*exec.Cmd, n)
	for i := 0; i < n; i++ {
		cmd, eb := spawn()
		cmds[i] = cmd
		go func(i int, cmd *exec.Cmd, eb *bytes.Buffer) {
			err := cmd.Wait()
			mu.Lock()
			exited[i] = result{err, eb.String()}
			mu.Unlock()
		}(i, cmd, eb)
	}
	t.Cleanup(func() {
		for _, c := range cmds {
			_ = c.Process.Kill() // 終了済みへの Kill はエラーになるだけで無害
		}
	})

	// 敗者 7 本が二重起動拒否で exit するのを待つ（勝者 1 本は ping ブロック
	// 中＝生存し続ける）。旧コードは複数が ping ブロックへ進む（exited が
	// 7 に届かない）か、固定 tmp の rename 競合で落ちる＝ここか下の内容
	// 検査で FAIL する。
	waitFor(t, 15*time.Second, "7 losers rejected", func() (bool, error) {
		mu.Lock()
		defer mu.Unlock()
		return len(exited) >= n-1, fmt.Errorf("exited=%d/%d", len(exited), n)
	})
	time.Sleep(1 * time.Second) // 勝者が遅れて死んでいないかの猶予観測
	mu.Lock()
	if len(exited) != n-1 {
		for i, r := range exited {
			t.Logf("agent[%d]: err=%v stderr=%s", i, r.err, r.stderr)
		}
		mu.Unlock()
		t.Fatalf("生存 agent が 1 本でない（exited=%d/%d）＝ゲート多重通過か勝者死亡", len(exited), n)
	}
	winnerIdx := -1
	for i := 0; i < n; i++ {
		r, done := exited[i]
		if !done {
			winnerIdx = i
			continue
		}
		// 敗者は必ず「既に稼働中」の明示拒否で終わる。旧コードの
		// "pidfile 書込失敗"（tmp rename 競合）や "herdr へ接続できない"
		//（ゲート通過後の失敗）はここで検出される。
		if !strings.Contains(r.stderr, "既に稼働中") {
			mu.Unlock()
			t.Fatalf("敗者 %d が二重起動拒否以外で終了: err=%v stderr=%s", i, r.err, r.stderr)
		}
	}
	mu.Unlock()
	if winnerIdx < 0 {
		t.Fatal("勝者が特定できない")
	}

	// 勝者を SIGKILL（launchd 強制再起動・クラッシュ相当）。flock はカーネル
	// がプロセス消滅で自動解放する＝stale lock が次の正当な起動を拒否しない
	//（O_CREATE|O_EXCL のロックファイル方式には無い性質。stale unlink の
	// 再試行レース自体が存在しない）。
	if err := cmds[winnerIdx].Process.Kill(); err != nil {
		t.Fatalf("kill winner: %v", err)
	}
	waitFor(t, 5*time.Second, "winner reaped", func() (bool, error) {
		mu.Lock()
		defer mu.Unlock()
		return len(exited) == n, nil
	})

	// 次の起動はゲートを通過できる（拒否されず ping ブロックに入る）。
	again, againErr := spawn()
	againDone := make(chan error, 1)
	go func() { againDone <- again.Wait() }()
	select {
	case err := <-againDone:
		t.Fatalf("SIGKILL 後の再起動が拒否/失敗した（stale lock?）: err=%v stderr=%s", err, againErr.String())
	case <-time.After(1500 * time.Millisecond):
		// 生存＝ゲート通過して ping ブロック中（期待どおり）
	}
	_ = again.Process.Kill()
	<-againDone
}
