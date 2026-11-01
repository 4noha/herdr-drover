//go:build !darwin

package main

// localClipboardPNG は macOS 以外では未対応（画像なし扱い＝Ctrl+V はそのまま通す）。
func localClipboardPNG() ([]byte, error) { return nil, nil }
