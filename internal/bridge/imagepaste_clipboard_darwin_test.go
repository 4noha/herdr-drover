//go:build darwin

package bridge

// 実 OS クリップボードへの書き込みの確認（setMacClipboardImage をそのまま使う）。
// **この Mac のクリップボードを上書きする**ので HERDR_DROVER_REAL_CLIPBOARD=1 の時だけ走る
// （他のテストは setClipboardImage を差し替えて実クリップボードを汚さない）。
// 事前のクリップボードがテキストなら終了後に戻す。画像など戻せない内容なら走らせず失敗にする
// （黙って上書きしない）。

import (
	"bytes"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRealClipboardImage(t *testing.T) {
	if os.Getenv("HERDR_DROVER_REAL_CLIPBOARD") != "1" {
		t.Skip("SKIP: 実クリップボードを上書きするため HERDR_DROVER_REAL_CLIPBOARD=1 の時だけ実行")
	}
	info, err := exec.Command("osascript", "-e", "clipboard info").CombinedOutput()
	if err != nil {
		t.Fatalf("clipboard info: %v: %s", err, info)
	}
	prev := ""
	restorable := strings.TrimSpace(string(info)) == "" // 空
	if !restorable {
		// テキスト（«class utf8» / string / Unicode text）だけなら戻せる。
		restorable = !strings.Contains(string(info), "PNGf") && !strings.Contains(string(info), "TIFF") &&
			!strings.Contains(string(info), "JPEG") && !strings.Contains(string(info), "furl") &&
			!strings.Contains(string(info), "PDF")
		if restorable {
			b, _ := exec.Command("pbpaste").Output()
			prev = string(b)
		}
	}
	if !restorable {
		t.Fatalf("クリップボードに画像/ファイル等があり、戻せないので実行しない: %s", info)
	}
	t.Cleanup(func() {
		c := exec.Command("pbcopy")
		c.Stdin = strings.NewReader(prev)
		_ = c.Run()
	})

	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 8, 8))); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "x.png")
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := setMacClipboardImage(path, "png"); err != nil {
		t.Fatalf("setMacClipboardImage: %v", err)
	}
	after, err := exec.Command("osascript", "-e", "clipboard info").CombinedOutput()
	if err != nil {
		t.Fatalf("clipboard info(後): %v", err)
	}
	if !strings.Contains(string(after), "PNGf") {
		t.Fatalf("クリップボードに PNG が載っていない: %s", after)
	}
	// 載った画像が送ったものと同じ（読み戻して PNG として復号できる寸法）。
	out := filepath.Join(t.TempDir(), "back.png")
	scr := `set f to open for access POSIX file "` + out + `" with write permission
write (the clipboard as «class PNGf») to f
close access f`
	if o, err := exec.Command("osascript", "-e", scr).CombinedOutput(); err != nil {
		t.Fatalf("読み戻し: %v: %s", err, o)
	}
	bb, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(bb))
	if err != nil || cfg.Width != 8 || cfg.Height != 8 {
		t.Fatalf("読み戻した画像が 8x8 PNG でない: %+v err=%v", cfg, err)
	}
}
