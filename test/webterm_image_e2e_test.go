//go:build !windows

// webterm_image_e2e_test — Web の窓から画像を送る経路の通し試験（実環境）:
//
//	機械 viewer が term.js の sendImageBlob と同じ IMAGE frame
//	（0xff 0xfd | u32BE len | u8 ext | bytes）を relay へ送る
//	→ 実 relay → 実 agent の bridge → handleImage（一時ファイル化→OS クリップボード→Ctrl+V 注入）
//	→ 実 herdr の pane に Ctrl+V(0x16) が届く。
//
// クリップボードは実機を汚さないよう、agent の PATH 先頭に置いた osascript の差し替え
// （引数と画像ファイルのパスを記録して終わる）で受ける。bridge の実コード
// （landImage → setMacClipboardImage → exec osascript）は一切差し替えていない。
// 実クリップボードへの書き込みは internal/bridge の実クリップボード試験が担当する。
package e2e

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/4noha/drover-cloud/state"
)

const imagePCID = "e2e-webimage-herdr"

// imageFrame は term.js sendImageBlob が組む IMAGE frame と同じバイト列。
func imageFrame(ext byte, payload []byte) []byte {
	f := make([]byte, 7+len(payload))
	f[0], f[1] = 0xff, 0xfd
	binary.BigEndian.PutUint32(f[2:6], uint32(len(payload)))
	f[6] = ext
	copy(f[7:], payload)
	return f
}

func TestE2EWebTerminalImagePaste(t *testing.T) {
	if os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
		t.Skip("SKIP: gcloud / Java 不在のため Firestore emulator 検証不可")
	}
	requireBridge(t)
	srv, hc := startHerdr(t)
	relayURL := startLocalRelay(t, droverCloudRoot(t))
	bin := buildBinary(t)

	st, err := state.New(context.Background(), projectID, "e2e-webimage-viewer")
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	// osascript の差し替え: 引数を 1 行ずつ記録して成功終了する。
	shimDir := t.TempDir()
	shimLog := filepath.Join(shimDir, "osascript.log")
	shim := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> " + shimLog + "\n"
	if err := os.WriteFile(filepath.Join(shimDir, "osascript"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}

	tmpHome := t.TempDir()
	agent := exec.Command(bin, "agent")
	agent.Env = []string{
		"HOME=" + tmpHome,
		"PATH=" + shimDir + ":" + os.Getenv("PATH"),
		"GCP_PROJECT=" + projectID,
		"PC_ID=" + imagePCID,
		"DROVER_TICK=1s",
		"DROVER_WEB_IMAGE_PASTE=true",
		"FIRESTORE_EMULATOR_HOST=" + os.Getenv("FIRESTORE_EMULATOR_HOST"),
		"HERDR_SOCKET_PATH=" + srv.sock,
		"CLOUD_RELAY_URL=" + relayURL,
	}
	var agentErr, agentOut syncBuf
	agent.Stderr, agent.Stdout = &agentErr, &agentOut
	if err := agent.Start(); err != nil {
		t.Fatalf("agent start: %v", err)
	}
	t.Cleanup(func() { _ = agent.Process.Kill(); _ = agent.Wait() })

	waitFor(t, 20*time.Second, "pcs registered", func() (bool, error) {
		pcs, err := st.ListPCs(context.Background())
		if err != nil {
			return false, err
		}
		for _, p := range pcs {
			if p == imagePCID {
				return true, nil
			}
		}
		return false, fmt.Errorf("pcs=%v", pcs)
	})

	ws, err := hc.WorkspaceCreate()
	if err != nil {
		t.Fatalf("workspace.create: %v", err)
	}
	paneID := ws.RootPane.PaneID
	t.Cleanup(func() { _ = hc.WorkspaceClose(ws.Workspace.WorkspaceID) })

	// Web が「この PC は画像を受けられる」と判断する材料（セッション doc の image_paste）。
	waitFor(t, 20*time.Second, "session doc に image_paste=true", func() (bool, error) {
		ss, err := st.ListSessions(context.Background(), imagePCID)
		if err != nil {
			return false, err
		}
		for _, s := range ss {
			if v, _ := s["image_paste"].(bool); v {
				return true, nil
			}
		}
		return false, fmt.Errorf("sessions=%d（image_paste なし）", len(ss))
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	viewer, recv, _ := dialViewer(t, ctx, relayURL, paneID)
	if _, err := viewer.Write(resizeFrame(40, 120)); err != nil {
		t.Fatalf("RESIZE: %v", err)
	}
	if err := st.Wake(ctx, imagePCID, paneID); err != nil {
		t.Fatalf("Wake: %v", err)
	}
	waitFor(t, 30*time.Second, "observe frame が届く", func() (bool, error) {
		return bytes.Contains(recv(), []byte("\x1b[?2026h")), fmt.Errorf("received=%dB\n%s", len(recv()), agentErr.String())
	})

	// pane に Ctrl+V を受ける捕捉を仕掛ける（stty raw で 0x16 をそのまま cat に渡す）。
	capFile := filepath.Join(t.TempDir(), "cap.bin")
	if err := hc.PaneSendText(paneID, "stty raw -echo; cat > "+capFile+"\r"); err != nil {
		t.Fatalf("start capture: %v", err)
	}
	waitFor(t, 10*time.Second, "capture ファイル出現", func() (bool, error) {
		_, err := os.Stat(capFile)
		return err == nil, err
	})

	// 本物の PNG を送る。
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for x := 0; x < 4; x++ {
		for y := 0; y < 4; y++ {
			img.Set(x, y, color.RGBA{uint8(60 * x), uint8(60 * y), 200, 255})
		}
	}
	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, img); err != nil {
		t.Fatal(err)
	}
	if _, err := viewer.Write(imageFrame(1, pngBuf.Bytes())); err != nil {
		t.Fatalf("IMAGE frame 送信: %v", err)
	}

	// (1) クリップボード投入が実コードの経路で呼ばれ、画像ファイルが送ったバイトと一致する。
	var imgPath string
	waitFor(t, 20*time.Second, "osascript にクリップボード投入が渡る", func() (bool, error) {
		b, err := os.ReadFile(shimLog)
		if err != nil {
			return false, fmt.Errorf("osascript 未呼出\n%s", agentErr.String())
		}
		m := regexp.MustCompile(`POSIX file "([^"]+\.png)"\) as «class PNGf»`).FindStringSubmatch(string(b))
		if m == nil {
			return false, fmt.Errorf("osascript 引数が想定外: %q", b)
		}
		imgPath = m[1]
		return true, nil
	})
	if !strings.HasPrefix(imgPath, tmpHome) {
		t.Fatalf("一時画像が agent の HOME 配下に無い: %s", imgPath)
	}
	got, err := os.ReadFile(imgPath)
	if err != nil {
		t.Fatalf("一時画像を読めない: %v", err)
	}
	if !bytes.Equal(got, pngBuf.Bytes()) {
		t.Fatalf("クリップボードへ載せる画像が送ったバイトと違う（%dB vs %dB）", len(got), pngBuf.Len())
	}

	// (2) 実 herdr の pane に Ctrl+V(0x16) が届く。
	waitFor(t, 15*time.Second, "pane に Ctrl+V(0x16) が届く", func() (bool, error) {
		b, err := os.ReadFile(capFile)
		if err != nil {
			return false, err
		}
		return bytes.Contains(b, []byte{0x16}), fmt.Errorf("cap=%q", b)
	})
}
