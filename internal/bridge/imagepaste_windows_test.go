package bridge

import (
	"strings"
	"testing"
)

func TestWinClipboardSetScriptQuotesPath(t *testing.T) {
	s := WinClipboardSetScript(`C:\Users\o'neil\.herdr-drover\paste\ab.png`)
	if !strings.Contains(s, `FromFile('C:\Users\o''neil\.herdr-drover\paste\ab.png')`) {
		t.Fatalf("単一引用符のエスケープ: %s", s)
	}
	if !strings.Contains(s, "Clipboard]::SetImage($img)") || !strings.HasSuffix(s, "$img.Dispose()") {
		t.Fatalf("スクリプトの形: %s", s)
	}
}
