//go:build windows

package main

import (
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// killHerdrOrphans は**自分が spawn した** herdr server（pid・起動時刻 started）の
// 子孫で、`herdr server stop` 後も生き残ったプロセスを終了させる。
//
// 実測（herdr 0.9.5・2026-10-17）: Windows の herdr は stop で pane のプロセスツリーを
// 確実には殺さない。親（herdr）が消えた OpenConsole.exe＋pane プロセス
// （`cmd.exe /c stub` やシェル）が残り、その cwd（テストの t.TempDir）を握り続けて
// TempDir の後始末が "being used by another process" で落ちる（間欠）。孤児は
// 実行のたびに溜まる（テスト herdr 由来の powershell/OpenConsole が残っていた）。
//
// 対象の限定（他プロセス不可侵の恒久規律）: PPID 連鎖が herdr の pid に届き、かつ
// herdr 起動以降に作られたものだけ（PID 再利用で無関係のプロセスを巻き込まない）。
func killHerdrOrphans(herdrPID int, started time.Time) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return
	}
	defer windows.CloseHandle(snap)
	parent := map[uint32]uint32{}
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		parent[e.ProcessID] = e.ParentProcessID
	}
	descends := func(pid uint32) bool {
		for i := 0; i < 64; i++ { // 連鎖の上限（循環防止）
			pp, ok := parent[pid]
			if !ok || pp == 0 || pp == pid {
				return false
			}
			if pp == uint32(herdrPID) {
				return true
			}
			pid = pp
		}
		return false
	}
	var handles []windows.Handle
	for pid := range parent {
		if pid == uint32(herdrPID) || !descends(pid) {
			continue
		}
		h, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
		if err != nil {
			continue
		}
		var c, x, k, u windows.Filetime
		if windows.GetProcessTimes(h, &c, &x, &k, &u) != nil || time.Unix(0, c.Nanoseconds()).Before(started) {
			windows.CloseHandle(h) // herdr より古い＝PID 再利用の無関係プロセス
			continue
		}
		_ = windows.TerminateProcess(h, 1)
		handles = append(handles, h)
	}
	for _, h := range handles {
		_, _ = windows.WaitForSingleObject(h, 2000)
		windows.CloseHandle(h)
	}
}
