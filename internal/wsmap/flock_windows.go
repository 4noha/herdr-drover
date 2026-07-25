//go:build windows

package wsmap

import (
	"os"

	"golang.org/x/sys/windows"
)

// flockExclusive は lf に排他ロックを取る（Windows: LockFileEx の排他ロック）。
// ファイル全域(最大バイト範囲)を対象にし、取得できるまでブロックする
// （LOCKFILE_FAIL_IMMEDIATELY 無し＝unix flock(LOCK_EX) と同じブロッキング
// 意味論）。lf を close するとロックも解放される。
func flockExclusive(lf *os.File) error {
	ol := new(windows.Overlapped)
	return windows.LockFileEx(
		windows.Handle(lf.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK,
		0,
		0xffffffff, 0xffffffff,
		ol,
	)
}
