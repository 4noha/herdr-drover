//go:build !darwin && !windows

package main

// localClipboardPNG は macOS / Windows 以外では未対応（画像なし扱い＝Ctrl+V はそのまま通す）。
func localClipboardPNG() ([]byte, error) { return nil, nil }
