//go:build windows

// term_windows.go — 端末の raw モード・サイズ・リサイズ通知の Windows 実装（term_unix.go の対）。
//
// herdr の pane（ConPTY）の中で動く attach / localview は、stdin/stdout がコンソールハンドル。
//   - raw: 入力は行編集・エコー・Ctrl+C 処理を切り、VT 入力（矢印等を ESC 列で受ける）を入れる。
//     出力は VT 処理を入れ、改行の自動 CR 付与を切る（フレームは生 ANSI を透過する）。
//   - サイズ: GetConsoleScreenBufferInfo の表示窓（スクリーンバッファ全体ではない）。
//   - リサイズ: SIGWINCH が無いので 500ms ごとにサイズを見て変化を通知する。
//   - 終了: SIGTERM は届かない。pane close は ConPTY 切断で読み取りが EOF になる＝Ctrl+C 相当の
//     os.Interrupt だけ拾う。
package main

import (
	"context"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// termState は raw モード前のコンソールモード（入力 fd と標準出力の 2 つ）。
type termState struct {
	in, out uint32
	hIn     windows.Handle
	hOut    windows.Handle
	outOK   bool
}

// winchPollInterval はリサイズ検知のポーリング間隔。
const winchPollInterval = 500 * time.Millisecond

// enterRaw は fd（入力コンソール）を raw にし、合わせて標準出力の VT 処理を有効にする。
// fd がコンソールでなければエラー（unix の tcgetattr 失敗と同意味論）。
func enterRaw(fd int) (*termState, error) {
	st := &termState{hIn: windows.Handle(fd), hOut: windows.Handle(os.Stdout.Fd())}
	if err := windows.GetConsoleMode(st.hIn, &st.in); err != nil {
		return nil, err
	}
	raw := st.in
	raw &^= windows.ENABLE_LINE_INPUT | windows.ENABLE_ECHO_INPUT | windows.ENABLE_PROCESSED_INPUT |
		windows.ENABLE_MOUSE_INPUT | windows.ENABLE_QUICK_EDIT_MODE
	raw |= windows.ENABLE_VIRTUAL_TERMINAL_INPUT | windows.ENABLE_EXTENDED_FLAGS
	if err := windows.SetConsoleMode(st.hIn, raw); err != nil {
		return nil, err
	}
	if err := windows.GetConsoleMode(st.hOut, &st.out); err == nil {
		st.outOK = true
		_ = windows.SetConsoleMode(st.hOut, st.out|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING|
			windows.ENABLE_PROCESSED_OUTPUT|windows.DISABLE_NEWLINE_AUTO_RETURN)
	}
	return st, nil
}

func restoreRaw(fd int, old *termState) {
	if old == nil {
		return
	}
	_ = windows.SetConsoleMode(old.hIn, old.in)
	if old.outOK {
		_ = windows.SetConsoleMode(old.hOut, old.out)
	}
}

// termSize は fd（出力コンソール）の表示窓の行・桁。取得不能や 0 は 24x80 へ fallback。
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

// watchWinch は端末サイズ変更の通知チャネル（容量 1・最新のみ）。Windows はポーリング。
func watchWinch(ctx context.Context) <-chan struct{} {
	ch := make(chan struct{}, 1)
	go func() {
		fd := int(os.Stdout.Fd())
		r0, c0 := termSize(fd)
		t := time.NewTicker(winchPollInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				r, c := termSize(fd)
				if r != r0 || c != c0 {
					r0, c0 = r, c
					select {
					case ch <- struct{}{}:
					default:
					}
				}
			}
		}
	}()
	return ch
}

// termSignals は「終了して」の合図。Windows は os.Interrupt（Ctrl+C / Ctrl+Break）だけ。
func termSignals() []os.Signal { return []os.Signal{os.Interrupt} }
