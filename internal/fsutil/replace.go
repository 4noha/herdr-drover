// Package fsutil は tmp→rename の原子置換を OS 差込みで提供する。
package fsutil

import "os"

// ReplaceFile は src を dst へ rename で置き換える（dst が既存でも置換）。
// unix の rename(2) は置換が原子的で、dst を開いている reader がいても成功する。
// Windows は replaceFile（replace_windows.go）を参照。
func ReplaceFile(src, dst string) error {
	return replaceFile(src, dst)
}

var rename = os.Rename
