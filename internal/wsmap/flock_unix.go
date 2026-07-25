//go:build !windows

package wsmap

import (
	"os"
	"syscall"
)

// flockExclusive は lf に排他ロックを取る（unix: flock(2) LOCK_EX）。
// lf を close するとロックも解放される。
func flockExclusive(lf *os.File) error {
	return syscall.Flock(int(lf.Fd()), syscall.LOCK_EX)
}
