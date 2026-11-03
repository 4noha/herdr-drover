//go:build unix

// term_unix.go — 端末の raw モード・サイズ・リサイズ通知の unix 実装（attach / localview が使う）。
// Windows 実装は term_windows.go。呼び手はこの 5 つの seam だけを使い、OS を意識しない。
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"
)

// termState は raw モード前の端末状態（restoreRaw に渡す）。
type termState = unix.Termios

// enterRaw は fd を cfmakeraw 相当の raw mode にし、復元用の旧 termios を返す。
func enterRaw(fd int) (*termState, error) {
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

func restoreRaw(fd int, old *termState) {
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

// watchWinch は端末サイズ変更の通知チャネル（容量 1・最新のみ）。unix は SIGWINCH。
// ctx 終了で購読を解く。
func watchWinch(ctx context.Context) <-chan struct{} {
	ch := make(chan struct{}, 1)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGWINCH)
	go func() {
		defer signal.Stop(sig)
		for {
			select {
			case <-ctx.Done():
				return
			case <-sig:
				select {
				case ch <- struct{}{}:
				default:
				}
			}
		}
	}()
	return ch
}

// termSignals は「終了して」の合図（attach の ctx を切る）。pane close で herdr が送る。
func termSignals() []os.Signal { return []os.Signal{syscall.SIGTERM, syscall.SIGINT} }
