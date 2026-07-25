//go:build unix

package main

// platform_unix.go — 常時コンパイル側（agent.go / nudge.go / pidfile.go /
// claudeshim.go）から切り出した OS 依存ヘルパの unix 実装。挙動は元コードと
// バイト等価（Windows 移植のための build-tag 分割であって仕様変更ではない）。
// 対の windows 実装は platform_windows.go。

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"unsafe"
)

// remoteInjectSupported: unix は注入 pane 内の attach viewer が動く＝従来どおり
// リモート pane 注入（↗窓）を有効にできる。
const remoteInjectSupported = true

// notifyNudge は即時 re-scan トリガ（SIGUSR1）の受け口を作る。返る stop で
// 購読解除。agent.go の signal.Notify(nudge, SIGUSR1) と同一。
func notifyNudge() (chan os.Signal, func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGUSR1)
	return ch, func() { signal.Stop(ch) }
}

// nudgeDaemon は稼働 daemon へ SIGUSR1 を送り即時 re-scan させる。
func nudgeDaemon(pid int, stdout io.Writer) error {
	if err := syscall.Kill(pid, syscall.SIGUSR1); err != nil {
		return fmt.Errorf("SIGUSR1 送出失敗（pid %d）: %w", pid, err)
	}
	fmt.Fprintf(stdout, "nudged: pid %d へ SIGUSR1（即時 re-scan）\n", pid)
	return nil
}

// pidAlive は pid の実生存を signal 0 で判定する。EPERM は「存在するが
// 権限がない」＝生存扱い（cm diag の isAlive と同じ規約）。
func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM)
}

// tryLockExclusiveNB は f に排他ロックを非ブロッキングで試みる（flock
// LOCK_EX|LOCK_NB）。取得済みなら即エラー。close で解放。
func tryLockExclusiveNB(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

// execProcess は現プロセスを argv0 で置換する（成功時は戻らない）。
func execProcess(argv0 string, argv []string, env []string) error {
	return syscall.Exec(argv0, argv, env)
}

// stdinIsTTYImpl は stdin が対話端末か（tcgetattr 相当 ioctl の成否）。
// /dev/null=false・pipe=false・pty slave=true（実測）。ioctl 番号は
// claudeshim_tty_{darwin,linux}.go の OS-split 定数。
func stdinIsTTYImpl() bool {
	// バッファは termios 実サイズ（darwin 72B / linux 36B）より十分大きく。
	var termios [128]byte
	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL, os.Stdin.Fd(), uintptr(ioctlReadTermios),
		uintptr(unsafe.Pointer(&termios[0])))
	return errno == 0
}

// setDetachedProc は子プロセスを親から切り離す（unix: 新セッション）。
func setDetachedProc(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
