//go:build !windows

package bridge

// imagepaste_unix_test.go — 実 herdr を要する画像貼付テスト（startHerdr は
// bridge_test.go にあり !windows 制約。合成で緑にしない＝鉄則 2）。

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestHandleImageInjectsCtrlV は**実 herdr の pane** に Ctrl+V(0x16) が
// 1 バイトだけ届くことを見る（合成では通っても本番経路で当たらない、を防ぐ）。
func TestHandleImageInjectsCtrlV(t *testing.T) {
	_, hc := startHerdr(t)
	ws, err := hc.WorkspaceCreate()
	if err != nil {
		t.Fatalf("workspace.create: %v", err)
	}
	paneID := ws.RootPane.PaneID

	orig := setClipboardImage
	setClipboardImage = func(path, ext string) error { return nil } // 実クリップボードを汚さない
	t.Cleanup(func() { setClipboardImage = orig })

	b := New(paneID, nil, hc)
	b.Logf = t.Logf
	b.ImagePaste = true
	b.ImagePasteDir = t.TempDir()

	waitFor(t, 15*time.Second, "pane 準備", func() (bool, error) {
		if err := hc.PaneSendText(paneID, "echo HD_IMG_'UP'\r"); err != nil {
			return false, err
		}
		rd, err := hc.PaneRead(paneID, "visible")
		if err != nil {
			return false, err
		}
		return strings.Contains(rd.Text, "HD_IMG_UP"), nil
	})

	capFile := filepath.Join(t.TempDir(), "cap.bin")
	if err := hc.PaneSendText(paneID, "stty raw -echo; cat > "+capFile+"\r"); err != nil {
		t.Fatalf("start capture: %v", err)
	}
	waitFor(t, 10*time.Second, "capture ファイル出現", func() (bool, error) {
		_, err := os.Stat(capFile)
		return err == nil, err
	})

	b.handleImage(Event{Kind: EvImage, ImageLen: 4, ImageExt: 1, Image: []byte{1, 2, 3, 4}})

	waitFor(t, 15*time.Second, "Ctrl+V(0x16) が pane に届く", func() (bool, error) {
		got, err := os.ReadFile(capFile)
		if err != nil {
			return false, err
		}
		return bytes.Contains(got, []byte{0x16}), nil
	})
}
