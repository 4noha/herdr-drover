//go:build unix

package main

// 実 SIGUSR1 が pidfile 経由でカーネルを通って届くことの検証（unix 限定）。
// nudge_test.go 本体（pidfile/flock 系）は OS 非依存なので分離した。
// Windows の対（nudgeDaemon が no-op 報告になること）は nudge_windows_test.go。

import (
	"bytes"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
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
