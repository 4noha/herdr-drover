//go:build !windows

package fsutil

func replaceFile(src, dst string) error { return rename(src, dst) }
