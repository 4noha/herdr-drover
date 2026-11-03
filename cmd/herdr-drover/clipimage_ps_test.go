package main

import (
	"strings"
	"testing"
)

func TestWinClipboardScriptsQuotePaths(t *testing.T) {
	p := `C:\Users\o'neil\AppData\Local\Temp\clip.png`
	s := winClipboardGetScript(p)
	if !strings.Contains(s, `'C:\Users\o''neil\AppData\Local\Temp\clip.png'`) {
		t.Fatalf("単一引用符が '' にエスケープされていない: %s", s)
	}
	if !strings.Contains(s, "ContainsImage()") || !strings.Contains(s, "ImageFormat]::Png") || !strings.HasSuffix(s, "'ok' } else { 'none' }") {
		t.Fatalf("スクリプトの形: %s", s)
	}
}
