package main

// nudge の実キーパステスト: pidfile → 実 SIGUSR1 がカーネル経由で届くこと
// （合成 stub ではなく自プロセス宛の実シグナルで検証）。

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestSendNudgeRealSignalRoundTrip(t *testing.T) {
	// Notify を先に張ってから送る（Notify 前に届いた SIGUSR1 は Go runtime が
	// 黙って捨てる＝受信できずテストが timeout する）。
	// 旧コメントの「SIGUSR1 の既定動作はプロセス終了」は Go には当てはまら
	// ない誤り（POSIX 既定と混同）: runtime は全 _SigNotify シグナルへ自前
	// ハンドラを入れ、未登録の SIGUSR1 は no action（sigtable go1.20〜1.26 で
	// 確認・実 agent への実 SIGUSR1 で生存を実測済み）。
	got := make(chan os.Signal, 1)
	signal.Notify(got, syscall.SIGUSR1)
	defer signal.Stop(got)

	path := filepath.Join(t.TempDir(), "agent.pid")
	if err := writePidfile(path, os.Getpid()); err != nil {
		t.Fatalf("writePidfile: %v", err)
	}
	var out bytes.Buffer
	if err := sendNudge(path, &out); err != nil {
		t.Fatalf("sendNudge: %v", err)
	}
	select {
	case <-got:
		// 実 SIGUSR1 到達＝agent.go の signal.Notify(nudge, SIGUSR1) と同経路
	case <-time.After(5 * time.Second):
		t.Fatalf("SIGUSR1 が届かない")
	}
	if !strings.Contains(out.String(), "nudged") {
		t.Fatalf("成功メッセージが無い: %q", out.String())
	}
}

func TestSendNudgeNoDaemon(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.pid")
	err := sendNudge(path, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "起動していない") {
		t.Fatalf("pidfile 不在の明示エラーが無い: %v", err)
	}
}

func TestSendNudgeStalePidfile(t *testing.T) {
	// 実 dead pid: 実子プロセスを spawn→回収した直後の pid を使う（回収直後の
	// pid 再利用は実質起きない。合成の「絶対使われない pid」定数は使わない）。
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn true: %v", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait true: %v", err)
	}
	path := filepath.Join(t.TempDir(), "agent.pid")
	if err := writePidfile(path, pid); err != nil {
		t.Fatalf("writePidfile: %v", err)
	}
	err := sendNudge(path, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale pidfile の明示エラーが無い: %v", err)
	}
}

// TestAcquirePidfileSingleWinner: 二重起動ゲートの原子性。並行に獲得を試み
// ても勝者は常に 1 つで、敗者は「既に稼働中」の明示拒否になる（flock は
// open ごとの記述子で独立に競合する＝プロセス間と同じカーネル判定を
// goroutine 並行で検証できる）。解放（Close）後は再獲得できる。
func TestAcquirePidfileSingleWinner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.pid")
	const n = 32
	var (
		mu      sync.Mutex
		locks   []*os.File
		rejects int
		wg      sync.WaitGroup
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, err := acquirePidfile(path, os.Getpid())
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				locks = append(locks, l)
				return
			}
			if !strings.Contains(err.Error(), "既に稼働中") {
				t.Errorf("拒否理由が二重起動でない: %v", err)
			}
			rejects++
		}()
	}
	wg.Wait()
	if len(locks) != 1 || rejects != n-1 {
		t.Fatalf("勝者は常に 1 のはず: winners=%d rejects=%d", len(locks), rejects)
	}
	if pid, err := readPidfile(path); err != nil || pid != os.Getpid() {
		t.Fatalf("勝者の pidfile が書かれていない: pid=%d err=%v", pid, err)
	}
	// 解放後は次の起動が正当に通過できる
	locks[0].Close()
	l2, err := acquirePidfile(path, os.Getpid())
	if err != nil {
		t.Fatalf("解放後の再獲得が拒否された: %v", err)
	}
	l2.Close()
}

// TestWritePidfileConcurrentWriters: writer が並行しても writePidfile は
// 全員成功し、最終内容はいずれかの完全な pid になる（tmp 名の衝突なし）。
// 旧実装（固定 path+".tmp"）は tmp を共有して rename が ENOENT で失敗した
// （実バイナリ 8 並行起動 ×10 ラウンドの実測で全ラウンド発生＝レビュー指摘。
// 本テストも旧実装で FAIL することを確認済み）。
func TestWritePidfileConcurrentWriters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.pid")
	const n = 64
	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(pid int) {
			defer wg.Done()
			if err := writePidfile(path, pid); err != nil {
				errCh <- err
			}
		}(1000 + i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("並行 writePidfile が失敗: %v", err)
	}
	pid, err := readPidfile(path)
	if err != nil || pid < 1000 || pid >= 1000+n {
		t.Fatalf("最終 pidfile が壊れている: pid=%d err=%v", pid, err)
	}
}

func TestPidfileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "agent.pid") // MkdirAll 経路も踏む
	if err := writePidfile(path, 12345); err != nil {
		t.Fatalf("writePidfile: %v", err)
	}
	pid, err := readPidfile(path)
	if err != nil || pid != 12345 {
		t.Fatalf("readPidfile: pid=%d err=%v", pid, err)
	}
	// 壊れた pidfile は明示エラー
	if err := os.WriteFile(path, []byte("garbage\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readPidfile(path); err == nil || !strings.Contains(err.Error(), "壊れている") {
		t.Fatalf("壊れ pidfile の明示エラーが無い: %v", err)
	}
}
