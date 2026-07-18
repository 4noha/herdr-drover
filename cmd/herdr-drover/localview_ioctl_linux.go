//go:build linux

package main

import "golang.org/x/sys/unix"

// getTermiosReq / setTermiosReq は tcgetattr(3) / tcsetattr(3) 相当の ioctl
// 要求番号（linux は TCGETS / TCSETS）。ロックフリー・ローカルビューア
// （localview.go）の raw mode 出入りが使う OS-split 定数。x/sys/unix の
// プラットフォーム別定義に一致（既存 claudeshim_tty_linux.go と同じ規律）。
const (
	getTermiosReq = unix.TCGETS
	setTermiosReq = unix.TCSETS
)
