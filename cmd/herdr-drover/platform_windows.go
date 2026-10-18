//go:build windows

package main

// platform_windows.go — platform_unix.go の対の Windows 実装＋Windows で
// out-of-scope な機能（ssh-forward）のスタブ。attach（↗窓）とローカル
// ビューアは herdr Windows 正式版（0.8.2〜）で移植済み＝端末差は tty_windows.go。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

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

// agentExeNames はエージェントの名前（InstallSpec の BinNames / WellKnownPaths）を
// ディスク上のファイル名候補（同一 dir 内の優先順）にする。Windows は
// `<name>.exe` → `<name>.cmd`（PATHEXT の既定順）:
//   - 拡張子無しの名前は実行できず、os.FileMode に実行ビットも無い（旧コードは
//     unix の判定のままで **claude.exe を一切見つけられず、シムが常に失敗**していた
//     ＝2026-10-17 実 herdr 0.9.5 のテストで発覚）。
//   - `.cmd` は npm の global 導入形（codex / cursor-agent 等は .exe を置かない）。
//     herdr 0.9.5 は argv[0] の `.cmd` を落として agent と検出し、引数も本体へ届く
//     （TestWindowsHerdrAliasDetection で実測。herdr 自身も cursor の再開を
//     `cursor-agent.cmd` で組む）。旧コードは .exe だけを見て npm 導入の agent を
//     見つけられなかった（回帰: TestLookupAgentBinFindsNpmCmdShim）。
//   - npm が同時に置く拡張子なし（sh 用）と .ps1 は CreateProcess で起動できない＝候補外。
func agentExeNames(name string) []string { return []string{name + ".exe", name + ".cmd"} }

// isExecFile は fi が実行可能な通常ファイルか（Windows に実行ビットは無い＝
// 拡張子で絞った上で通常ファイルなら可）。
func isExecFile(fi os.FileInfo) bool { return !fi.IsDir() }

// shellChildArgvs は shellPID 配下の子孫プロセスの argv を返す（Toolhelp の
// スナップショットで親子を辿り、各プロセスのコマンドラインを分解）。
//
// 必要な理由（実 herdr 0.9.5・2026-10-18 実測）: Windows の process_info は、シェルの
// 子として起動した**エージェントでない**プロセスを前景に出さず、前景はシェルのまま。
// ↗窓 の復活（シェルに attach を打ち込む）後の client はシェルの子なので、前景だけ
// 見ると「不在」と判定し続け、生存確認のたびに実行中の attach へコマンド行を打ち
// 込み直してしまう。drover は同じマシンで動く＝OS から直接確かめる。
// 開けない／読めないプロセスは飛ばす（＝不在側に倒れ、次の確認でまた試す）。
func shellChildArgvs(shellPID int) [][]string {
	if shellPID <= 0 {
		return nil
	}
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(snap)
	children := map[uint32][]uint32{}
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		children[e.ParentProcessID] = append(children[e.ParentProcessID], e.ProcessID)
	}
	var out [][]string
	queue := append([]uint32(nil), children[uint32(shellPID)]...)
	seen := map[uint32]bool{uint32(shellPID): true}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		if argv, err := processArgv(pid); err == nil {
			out = append(out, argv)
		}
		queue = append(queue, children[pid]...)
	}
	return out
}

// processArgv は他プロセスのコマンドラインを読み、CommandLineToArgv 規則で分解する。
func processArgv(pid uint32) ([]string, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(h)
	buf := make([]byte, 1024)
	for {
		var n uint32
		st := windows.NtQueryInformationProcess(h, windows.ProcessCommandLineInformation,
			unsafe.Pointer(&buf[0]), uint32(len(buf)), &n)
		if st == nil {
			break
		}
		if (st == windows.STATUS_INFO_LENGTH_MISMATCH || st == windows.STATUS_BUFFER_TOO_SMALL) &&
			int(n) > len(buf) && n < 1<<20 {
			buf = make([]byte, n)
			continue
		}
		return nil, st
	}
	us := (*windows.NTUnicodeString)(unsafe.Pointer(&buf[0]))
	if us.Buffer == nil || us.Length == 0 {
		return nil, fmt.Errorf("pid %d: コマンドライン空", pid)
	}
	line := windows.UTF16ToString(unsafe.Slice(us.Buffer, us.Length/2))
	return windows.DecomposeCommandLine(line)
}
