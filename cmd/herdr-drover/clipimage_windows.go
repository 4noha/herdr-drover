//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// localClipboardPNG は Windows のクリップボードに画像があれば PNG で返す（無ければ nil, nil＝
// Ctrl+V をそのまま通す）。PowerShell（STA）の System.Windows.Forms.Clipboard を使う。
func localClipboardPNG() ([]byte, error) {
	dir, err := os.MkdirTemp("", "drover-clip")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	out := filepath.Join(dir, "clip.png")
	b, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-STA", "-Command", winClipboardGetScript(out)).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("powershell（クリップボード読取）: %v (%s)", err, strings.TrimSpace(string(b)))
	}
	if strings.TrimSpace(string(b)) != "ok" {
		return nil, nil // 画像なし
	}
	return os.ReadFile(out)
}
