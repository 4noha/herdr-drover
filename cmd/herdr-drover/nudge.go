package main

// nudge — 稼働中 daemon へ SIGUSR1 を送り即時 re-scan させる。
// herdr plugin の events（pane.created 等）から一発 spawn される想定
// （herdr-plugin.toml 参照。plugin 機構は常駐不可＝hook は合図のみ）。

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"syscall"
)

func cmdNudge(stdout io.Writer) error {
	path, err := pidfilePath()
	if err != nil {
		return err
	}
	return sendNudge(path, stdout)
}

// sendNudge は pidfile を読み、実生存を確認してから SIGUSR1 を送る。
// path 注入でテストから隔離 pidfile を使える（実シグナル経路のまま）。
func sendNudge(path string, stdout io.Writer) error {
	pid, err := readPidfile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("daemon が起動していない（pidfile %s なし）。`herdr-drover agent` を起動せよ", path)
	}
	if err != nil {
		return err
	}
	if !pidAlive(pid) {
		// SIGKILL 死・launchd 強制再起動では pidfile が残る（cm dead PID
		// sweep 教訓）。存在しない pid へ kill してエラーになるより先に
		// 「stale」と名指しする方が原因が伝わる。
		return fmt.Errorf("daemon が死んでいる（pid %d は不在＝stale pidfile %s）。`herdr-drover agent` を再起動せよ", pid, path)
	}
	if err := syscall.Kill(pid, syscall.SIGUSR1); err != nil {
		return fmt.Errorf("SIGUSR1 送出失敗（pid %d）: %w", pid, err)
	}
	fmt.Fprintf(stdout, "nudged: pid %d へ SIGUSR1（即時 re-scan）\n", pid)
	return nil
}
