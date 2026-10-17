//go:build windows

package main

// tty_windows.go — tty_unix.go の対（attach / localview の端末 seam）。
//
// herdr の Windows pane は ConPTY＝pane 内プロセスの stdin/stdout はコンソール
// ハンドル。termios の代わりにコンソールモードを切り替える:
//   - 入力: ECHO/LINE/PROCESSED を外し VIRTUAL_TERMINAL_INPUT を立てる＝キーが
//     VT バイト列のまま ReadFile に届く（unix の cfmakeraw＋ISIG 無効と同義＝
//     Ctrl+C もシグナルでなく 0x03 として pane へ送る）。
//   - 出力: VIRTUAL_TERMINAL_PROCESSING を立てる＝observe フレームの ANSI を
//     コンソールが解釈する（無いと ESC がそのまま文字として出る）。
// SIGWINCH は無い＝サイズをポーリングして変化を同じ形（chan os.Signal）で通知する。

import (
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// rawState は restoreRaw 用の旧コンソールモード。
type rawState struct {
	in, out    windows.Handle
	inMode     uint32
	outMode    uint32
	outChanged bool
}

func enterRaw(fd int) (*rawState, error) {
	st := &rawState{in: windows.Handle(fd), out: windows.Handle(os.Stdout.Fd())}
	if err := windows.GetConsoleMode(st.in, &st.inMode); err != nil {
		return nil, err
	}
	raw := st.inMode &^ (windows.ENABLE_ECHO_INPUT | windows.ENABLE_PROCESSED_INPUT |
		windows.ENABLE_LINE_INPUT | windows.ENABLE_MOUSE_INPUT)
	raw |= windows.ENABLE_VIRTUAL_TERMINAL_INPUT
	if err := windows.SetConsoleMode(st.in, raw); err != nil {
		return nil, err
	}
	if windows.GetConsoleMode(st.out, &st.outMode) == nil {
		vt := st.outMode | windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING | windows.ENABLE_PROCESSED_OUTPUT
		st.outChanged = vt != st.outMode && windows.SetConsoleMode(st.out, vt) == nil
	}
	return st, nil
}

func restoreRaw(fd int, st *rawState) {
	if st == nil {
		return
	}
	_ = windows.SetConsoleMode(st.in, st.inMode)
	if st.outChanged {
		_ = windows.SetConsoleMode(st.out, st.outMode)
	}
}

// termSize は表示ウィンドウ（バッファではなく可視範囲）の行・桁。取得不能や
// 0 は 24x80 へ fallback（unix 版と同じ理由）。
func termSize(fd int) (rows, cols int) {
	var info windows.ConsoleScreenBufferInfo
	if err := windows.GetConsoleScreenBufferInfo(windows.Handle(fd), &info); err != nil {
		return 24, 80
	}
	rows = int(info.Window.Bottom-info.Window.Top) + 1
	cols = int(info.Window.Right-info.Window.Left) + 1
	if rows <= 0 || cols <= 0 {
		return 24, 80
	}
	return rows, cols
}

// resizeSignal は notifyResize が流す擬似シグナル（Windows に SIGWINCH は無い）。
type resizeSignal struct{}

func (resizeSignal) String() string { return "resize" }
func (resizeSignal) Signal()        {}

// resizePollInterval は Windows のサイズ変化検知の周期（SIGWINCH の代替）。
const resizePollInterval = 250 * time.Millisecond

// notifyResize は fd の端末サイズ変化を chan へ通知する（unix は SIGWINCH）。
// stop で監視 goroutine を止める。
func notifyResize(fd int) (<-chan os.Signal, func()) {
	ch := make(chan os.Signal, 1)
	done := make(chan struct{})
	go func() {
		r0, c0 := termSize(fd)
		t := time.NewTicker(resizePollInterval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if r, c := termSize(fd); r != r0 || c != c0 {
					r0, c0 = r, c
					select {
					case ch <- resizeSignal{}:
					default:
					}
				}
			}
		}
	}()
	return ch, func() { close(done) }
}
