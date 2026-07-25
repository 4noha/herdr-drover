//go:build windows

package main

// platform_windows.go — platform_unix.go の対の Windows 実装＋Windows で
// out-of-scope な機能（attach / ssh-forward / ローカル direct-attach viewer）
// のスタブ。web/スマホ閲覧に必要な agent/producer 経路を Windows で成立させる
// のが目的で、TTY 直接アタッチ系は意図的にスタブ（DESIGN の Windows
// out-of-scope）。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"

	"github.com/4noha/herdr-drover/internal/herdrapi"
)

// remoteInjectSupported: Windows は注入 pane 内の attach viewer（cmdAttach）が
// 非対応＝リモート pane 注入は即死→再生成のスラッシングになるため常に無効。
// producer（自 PC セッションの publish）と Web ターミナル閲覧は影響なく動く。
const remoteInjectSupported = false

// --- platform_unix.go と対の OS 依存ヘルパ（Windows 実装） ---

// notifyNudge: Windows は SIGUSR1 が無い＝即時 re-scan トリガ無し。決して
// 発火しないチャネルを返し（agent の周期 tick=DROVER_TICK に委譲）、stop は
// no-op。producer ループの `case <-nudge` は常にブロックのままで無害。
func notifyNudge() (chan os.Signal, func()) {
	return make(chan os.Signal, 1), func() {}
}

// nudgeDaemon: Windows は即時 re-scan（SIGUSR1）非対応。エラーにせず、周期
// tick に委ねる旨を伝える（pidAlive 済み＝daemon は生存）。
func nudgeDaemon(pid int, stdout io.Writer) error {
	fmt.Fprintf(stdout, "nudge: Windows は即時 re-scan 非対応（agent の周期 tick=DROVER_TICK に委譲）。daemon pid %d は稼働中\n", pid)
	return nil
}

// pidAlive は pid の実生存を OpenProcess＋WaitForSingleObject(0) で判定する
// （signaled=終了済み／timeout=稼働中）。開けない＝不在扱い。
func pidAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	ev, err := windows.WaitForSingleObject(h, 0)
	if err != nil {
		return true // ハンドルは開けた＝存在。待機失敗は生存側に倒す。
	}
	return ev == uint32(windows.WAIT_TIMEOUT)
}

// tryLockExclusiveNB は f に排他ロックを非ブロッキングで試みる（LockFileEx の
// 排他＋即時失敗）。取得済みなら ERROR_LOCK_VIOLATION で即エラー＝unix の
// flock(LOCK_EX|LOCK_NB) と同意味論。close で解放。
func tryLockExclusiveNB(f *os.File) error {
	ol := new(windows.Overlapped)
	return windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 0xffffffff, 0xffffffff, ol,
	)
}

// execProcess: Windows に exec 置換は無いので、子を stdio 継承で起動して待ち、
// 子の終了コードで自プロセスを終える（=置換の近似。成功時は戻らない）。
func execProcess(argv0 string, argv []string, env []string) error {
	var rest []string
	if len(argv) > 1 {
		rest = argv[1:]
	}
	c := exec.Command(argv0, rest...)
	c.Env = env
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			os.Exit(ee.ExitCode())
		}
		return err
	}
	os.Exit(0)
	return nil
}

// stdinIsTTYImpl は stdin が対話端末（コンソール）か。GetConsoleMode は
// コンソールハンドルでのみ成功し、pipe/file/NUL では失敗する＝unix の
// tcgetattr 成否判定と同意味論。
func stdinIsTTYImpl() bool {
	var mode uint32
	return windows.GetConsoleMode(windows.Handle(os.Stdin.Fd()), &mode) == nil
}

// setDetachedProc は子を親コンソール/プロセスグループから切り離す
// （unix の Setsid 相当）。
func setDetachedProc(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS,
	}
}

// --- Windows out-of-scope 機能のスタブ（unix ファイルが提供する定義の代替） ---

func errWindowsUnsupported(feature string) error {
	return fmt.Errorf("%s は Windows では非対応（macOS/Linux 専用。web/スマホ閲覧は agent で利用可）", feature)
}

// cmdAttach（attach.go・//go:build unix の代替）: リモート pane 注入 viewer。
func cmdAttach(args []string, stdout, stderr io.Writer) error {
	return errWindowsUnsupported("attach（リモート pane 注入 viewer）")
}

// cmdSSHForward（sshforward.go の代替）: owner ssh-agent の一時転送。
func cmdSSHForward(args []string, stdout, stderr io.Writer) error {
	return errWindowsUnsupported("ssh-forward（SSH エージェント転送）")
}

// isSSHForwardSid（sshforward.go の代替）: Windows は ssh-forward を作らないので
// 常に false（webterm の wake 分岐を無効化）。
func isSSHForwardSid(sid string) bool { return false }

// handleSSHForwardWake（sshforward.go のメソッドの代替）: isSSHForwardSid が
// 常に false なので実際には呼ばれない。webterm.go のコンパイルのために定義。
func (w *webTerm) handleSSHForwardWake(ctx context.Context, afSid string) {}

// runViewer（localview.go の var の代替）: ローカル direct-attach ビューア。
var runViewer = func(api *herdrapi.Client, paneID string) error {
	return errWindowsUnsupported("ローカルビューア（direct attach）")
}
