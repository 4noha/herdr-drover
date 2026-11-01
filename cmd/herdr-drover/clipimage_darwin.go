//go:build darwin

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// localClipboardPNG は**この Mac のクリップボードの画像**を PNG で返す。画像が無ければ
// (nil, nil)。↗窓の Ctrl+V で、リモートへ画像を送るかの判断に使う。
//
// 画像の有無は `clipboard info` の型名（PNGf / TIFF / JPEG）で exact に見る。取り出しは
// `the clipboard as «class PNGf»`（TIFF のみのスクリーンショットも PNG に変換される）。
func localClipboardPNG() ([]byte, error) {
	info, err := exec.Command("osascript", "-e", "clipboard info").Output()
	if err != nil {
		return nil, fmt.Errorf("clipboard info: %w", err)
	}
	s := string(info)
	if !strings.Contains(s, "PNGf") && !strings.Contains(s, "TIFF") && !strings.Contains(s, "JPEG") {
		return nil, nil
	}
	dir, err := os.MkdirTemp("", "drover-clip")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	out := filepath.Join(dir, "clip.png")
	scr := fmt.Sprintf(`set f to open for access POSIX file %q with write permission
set eof of f to 0
write (the clipboard as «class PNGf») to f
close access f`, out)
	if b, err := exec.Command("osascript", "-e", scr).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("クリップボードの画像を PNG にできない: %v (%s)", err, strings.TrimSpace(string(b)))
	}
	return os.ReadFile(out)
}
