//go:build unix

package main

// tty_unix.go — attach / localview の端末 seam（raw mode・サイズ・サイズ変化通知）。
// Windows の対は tty_windows.go（コンソールモード＋サイズのポーリング）。

import (
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"
)

// enterRaw は fd を cfmakeraw 相当の raw mode にし、復元用の旧 termios を返す。
func enterRaw(fd int) (*unix.Termios, error) {
	old, err := unix.IoctlGetTermios(fd, getTermiosReq)
	if err != nil {
		return nil, err
	}
	raw := makeRawTermios(*old)
	if err := unix.IoctlSetTermios(fd, setTermiosReq, &raw); err != nil {
		return nil, err
	}
	return old, nil
}

func restoreRaw(fd int, old *unix.Termios) {
	if old != nil {
		_ = unix.IoctlSetTermios(fd, setTermiosReq, old)
	}
}

// makeRawTermios は cfmakeraw(3) 相当のフラグ操作。x/sys/unix の定数は各 OS で
// 正しい型（darwin=uint64 / linux=uint32 のフラグ）へ解決されるため OS-split
// 不要（get/set の ioctl 要求番号だけ localview_ioctl_{darwin,linux}.go）。
func makeRawTermios(t unix.Termios) unix.Termios {
	t.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP |
		unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	t.Oflag &^= unix.OPOST
	t.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	t.Cflag &^= unix.CSIZE | unix.PARENB
	t.Cflag |= unix.CS8
	t.Cc[unix.VMIN] = 1
	t.Cc[unix.VTIME] = 0
	return t
}

// termSize は fd の端末サイズ（行・桁）。取得不能や 0 は 24x80 へ fallback
// （observe は 1 桁級の異常サイズだとクリップ画面しか返さないため）。
func termSize(fd int) (rows, cols int) {
	ws, err := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ)
	if err != nil || ws.Row == 0 || ws.Col == 0 {
		return 24, 80
	}
	return int(ws.Row), int(ws.Col)
}

// notifyResize は端末サイズ変化を chan へ通知する（unix は SIGWINCH）。
// stop で通知登録を解除する。fd は Windows 版（ポーリング対象）との対称のため。
func notifyResize(fd int) (<-chan os.Signal, func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	return ch, func() { signal.Stop(ch) }
}
