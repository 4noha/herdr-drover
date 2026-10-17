//go:build windows

package fsutil

import (
	"errors"
	"time"

	"golang.org/x/sys/windows"
)

// replaceRetryFor は Windows で rename 置換を再試行する上限時間。
var replaceRetryFor = 2 * time.Second

// replaceFile: Windows の MoveFileEx(REPLACE_EXISTING) は、dst を別プロセス／
// goroutine が開いている・同時に置換している間 ERROR_ACCESS_DENIED /
// ERROR_SHARING_VIOLATION で失敗する（unix の rename(2) と違う。実測:
// TestWritePidfileConcurrentWriters の 64 並行で "Access is denied"）。
// status が agent.pid を読んでいる瞬間に daemon 起動の書込が落ちる等の
// 一過性の衝突なので、短い backoff で replaceRetryFor まで再試行する。
func replaceFile(src, dst string) error {
	deadline := time.Now().Add(replaceRetryFor)
	wait := time.Millisecond
	for {
		err := rename(src, dst)
		if err == nil || !transient(err) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(wait)
		if wait < 50*time.Millisecond {
			wait *= 2
		}
	}
}

func transient(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}
