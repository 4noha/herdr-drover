//go:build darwin

package main

import "syscall"

// ioctlReadTermios は tcgetattr(3) 相当の ioctl 要求番号（darwin は TIOCGETA）。
// stdinIsTTY（claudeshim.go）の TTY 判定が使う OS-split 定数。
// syscall パッケージ内蔵の定数のみ＝依存追加なし（go.mod 不変）。
const ioctlReadTermios = syscall.TIOCGETA
