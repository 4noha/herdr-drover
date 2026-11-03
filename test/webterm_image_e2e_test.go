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
	"github.com/4noha/herdr-drover/internal/herdrapi"
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

// imageRig は「実 relay・実 herdr・実 agent（画像貼付 ON・osascript は記録用に差し替え）・
// リモート側の pane」まで。Web の窓／↗窓の両方の画像試験で共通。
type imageRig struct {
	srv       *testServer
	hc        *herdrapi.Client
	relayURL  string
	bin       string
	st        *state.Client
	agentErr  *syncBuf
	agentHome string
	shimLog   string // リモート側 osascript の呼出記録
	paneID    string // リモート側の pane（sid）
}

func newImageRig(t *testing.T) *imageRig {
	t.Helper()
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
	agentErr := &syncBuf{}
	var agentOut syncBuf
	agent.Stderr, agent.Stdout = agentErr, &agentOut
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

	// Web／↗窓が「この PC は画像を受けられる」と判断する材料（セッション doc の image_paste）。
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
	return &imageRig{srv: srv, hc: hc, relayURL: relayURL, bin: bin, st: st, agentErr: agentErr,
		agentHome: tmpHome, shimLog: shimLog, paneID: paneID}
}

// startCapture は pane に「受け取ったバイトをそのまま保存する cat」を仕掛ける。
func startCapture(t *testing.T, hc *herdrapi.Client, paneID string) string {
	t.Helper()
	capFile := filepath.Join(t.TempDir(), "cap.bin")
	if err := hc.PaneSendText(paneID, "stty raw -echo; cat > "+capFile+"\r"); err != nil {
		t.Fatalf("start capture: %v", err)
	}
	waitFor(t, 10*time.Second, "capture ファイル出現", func() (bool, error) {
		_, err := os.Stat(capFile)
		return err == nil, err
	})
	return capFile
}

// testPNG は本物の PNG。
func testPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for x := 0; x < 4; x++ {
		for y := 0; y < 4; y++ {
			img.Set(x, y, color.RGBA{uint8(60 * x), uint8(60 * y), 200, 255})
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// waitClipboardSet はリモート側 osascript の記録から、クリップボードへ載せた画像ファイルを
// 取り出す（実コードの landImage → setMacClipboardImage → exec osascript の経路）。
func waitClipboardSet(t *testing.T, r *imageRig, wantPrefix string) []byte {
	t.Helper()
	var imgPath string
	waitFor(t, 20*time.Second, "リモートの osascript にクリップボード投入が渡る", func() (bool, error) {
		b, err := os.ReadFile(r.shimLog)
		if err != nil {
			return false, fmt.Errorf("osascript 未呼出\n%s", r.agentErr.String())
		}
		m := regexp.MustCompile(`POSIX file "([^"]+\.png)"\) as «class PNGf»`).FindStringSubmatch(string(b))
		if m == nil {
			return false, fmt.Errorf("osascript 引数が想定外: %q", b)
		}
		imgPath = m[1]
		return true, nil
	})
	if !strings.HasPrefix(imgPath, wantPrefix) {
		t.Fatalf("一時画像が agent の HOME 配下に無い: %s", imgPath)
	}
	got, err := os.ReadFile(imgPath)
	if err != nil {
		t.Fatalf("一時画像を読めない: %v", err)
	}
	return got
}

func TestE2EWebTerminalImagePaste(t *testing.T) {
	r := newImageRig(t)
	hc, st, paneID, relayURL := r.hc, r.st, r.paneID, r.relayURL

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
		return bytes.Contains(recv(), []byte("\x1b[?2026h")), fmt.Errorf("received=%dB\n%s", len(recv()), r.agentErr.String())
	})

	// pane に Ctrl+V を受ける捕捉を仕掛ける（stty raw で 0x16 をそのまま cat に渡す）。
	capFile := startCapture(t, hc, paneID)

	// 本物の PNG を送る。
	pngBuf := bytes.NewBuffer(testPNG(t))
	if _, err := viewer.Write(imageFrame(1, pngBuf.Bytes())); err != nil {
		t.Fatalf("IMAGE frame 送信: %v", err)
	}

	// (1) クリップボード投入が実コードの経路で呼ばれ、画像ファイルが送ったバイトと一致する。
	got := waitClipboardSet(t, r, r.agentHome)
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

// TestE2EAttachImagePaste — ↗窓（herdr-drover attach）での画像貼付の通し試験。
//
//	この Mac のクリップボードに画像がある状態で、↗窓の pane に Ctrl+V キーを送る
//	→ attach が画像を PNG で読み、IMAGE frame を relay へ送る
//	→ リモートの producer がクリップボードへ載せて pane に Ctrl+V を注入。
//
// attach 側の osascript も差し替える（クリップボードの有無は FLAG ファイルで切替）。
// リモートに画像が無い（クリップボードがテキストだけ）時は、従来どおり Ctrl+V が
// そのままリモートへ届き、リモートのクリップボードは触られないことも見る。
func TestE2EAttachImagePaste(t *testing.T) {
	r := newImageRig(t)
	hc := r.hc
	remoteCap := startCapture(t, hc, r.paneID)

	// ↗窓側（この Mac 相当）の osascript 差し替え。
	pngBytes := testPNG(t)
	attDir := t.TempDir()
	fixture := filepath.Join(attDir, "fixture.png")
	flag := filepath.Join(attDir, "has-image")
	if err := os.WriteFile(fixture, pngBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	shim := `#!/bin/sh
if [ "$1" = "-e" ] && [ "$2" = "clipboard info" ]; then
  if [ -e ` + flag + ` ]; then echo "«class PNGf», 100"; else echo "«class utf8», 5"; fi
  exit 0
fi
case "$2" in
  *"write (the clipboard"*)
    out=$(printf '%s' "$2" | sed -n 's/.*POSIX file "\([^"]*\)".*/\1/p' | head -1)
    cp ` + fixture + ` "$out"
    ;;
esac
`
	if err := os.WriteFile(filepath.Join(attDir, "osascript"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}

	// ↗窓の pane（実 herdr）で本物の attach を動かす。
	ws, err := hc.WorkspaceCreate()
	if err != nil {
		t.Fatalf("workspace.create: %v", err)
	}
	attPane := ws.RootPane.PaneID
	t.Cleanup(func() { _ = hc.WorkspaceClose(ws.Workspace.WorkspaceID) })
	attHome := t.TempDir()
	// HERDR_PANE_ID を外す: attach は起動時に自 pane へ「注入 pane」の印を付けるが、この試験は
	// リモート側 agent と同じ herdr を共有するので、agent の注入 pane 掃除に巻き込まれる
	// （実運用の ↗窓は別 PC の herdr 上なので起きない構図）。
	cmd := fmt.Sprintf("env -u HERDR_PANE_ID HOME=%s PATH=%s:$PATH GCP_PROJECT=%s PC_ID=e2e-attach-herdr CLOUD_RELAY_URL=%s FIRESTORE_EMULATOR_HOST=%s HERDR_SOCKET_PATH=%s %s attach %s %s\r",
		attHome, attDir, projectID, r.relayURL, os.Getenv("FIRESTORE_EMULATOR_HOST"), r.srv.sock, r.bin, imagePCID, r.paneID)
	if err := hc.PaneSendText(attPane, cmd); err != nil {
		t.Fatalf("attach 起動: %v", err)
	}
	attLog := func() string {
		b, _ := os.ReadFile(filepath.Join(attHome, ".herdr-drover", "attach.log"))
		return string(b)
	}
	// attach がリモートに繋がり、image_paste=on をセッション doc から読んだ後で押す。
	// 接続の物証: リモートの pane に echo した marker が ↗窓の画面に出る。
	waitFor(t, 40*time.Second, "↗窓にリモートの画面が出る", func() (bool, error) {
		_ = hc.PaneSendText(r.paneID, "")
		vis, err := hc.PaneRead(attPane, "visible")
		if err != nil {
			return false, err
		}
		return strings.Contains(vis.Text, "stty raw") || strings.Contains(vis.Text, "cap.bin"), fmt.Errorf("attach.log:\n%s\nscreen:\n%s", attLog(), vis.Text)
	})

	countCtrlV := func() int {
		b, _ := os.ReadFile(remoteCap)
		return bytes.Count(b, []byte{0x16})
	}

	// --- (A) クリップボードにテキストだけ: Ctrl+V はそのままリモートへ届く・リモートのクリップボードは触れない ---
	if err := hc.PaneSendKeys(attPane, "ctrl+v"); err != nil {
		t.Fatalf("send ctrl+v: %v", err)
	}
	waitFor(t, 15*time.Second, "(A) テキストのみ: 0x16 がそのままリモートへ", func() (bool, error) {
		return countCtrlV() == 1, fmt.Errorf("0x16=%d attach.log:\n%s", countCtrlV(), attLog())
	})
	if b, _ := os.ReadFile(r.shimLog); len(b) != 0 {
		t.Fatalf("(A) 画像が無いのにリモートのクリップボードが操作された: %q", b)
	}

	// --- (B) クリップボードに画像: 画像がリモートのクリップボードへ載り、Ctrl+V が注入される ---
	if err := os.WriteFile(flag, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := hc.PaneSendKeys(attPane, "ctrl+v"); err != nil {
		t.Fatalf("send ctrl+v: %v", err)
	}
	got := waitClipboardSet(t, r, r.agentHome)
	if !bytes.Equal(got, pngBytes) {
		t.Fatalf("(B) リモートのクリップボードへ載った画像が違う（%dB vs %dB）\nattach.log:\n%s", len(got), len(pngBytes), attLog())
	}
	waitFor(t, 15*time.Second, "(B) producer が Ctrl+V を注入（合計 2 回目）", func() (bool, error) {
		return countCtrlV() == 2, fmt.Errorf("0x16=%d", countCtrlV())
	})
	// 画像貼付では attach 自身が 0x16 を送らない（二重に届かない）。
	time.Sleep(2 * time.Second)
	if n := countCtrlV(); n != 2 {
		t.Fatalf("(B) Ctrl+V が二重に届いた: 0x16=%d", n)
	}
	if !strings.Contains(attLog(), "画像貼付: ローカルのクリップボード画像") {
		t.Fatalf("attach.log に画像送信の記録が無い:\n%s", attLog())
	}
}
