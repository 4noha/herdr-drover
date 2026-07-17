//go:build !windows

package selfupdate

import (
	"fmt"
	"os"
)

// placeBinary は新バイナリ(tmp)を実行中バイナリ位置へ原子 rename する。
// unix は実行中でも rename 可（旧 inode は走行中プロセスが保持＝安全、
// 次回起動から新版）。cm place_unix.go と同 body（Windows 版は drover
// out-of-scope＝DESIGN。必要になったら cm place_windows.go を写す）。
func placeBinary(tmpName, exe string) error {
	if err := os.Rename(tmpName, exe); err != nil {
		return fmt.Errorf("置換失敗 %s: %w", exe, err)
	}
	return nil
}
