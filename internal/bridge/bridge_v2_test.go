//go:build !windows

// bridge_v2_test.go — ワイヤ v2（Web viewer 向け: 元 pane 実サイズで observe＋
// ストリーム先頭に META＋履歴）の実 herdr 統合テスト。鉄則どおり合成ストリームで
// 緑にしない: 隔離 herdr の pane に実際に履歴を作り、`stty size` で pane の中から
// 測った実サイズと observe argv を突き合わせる（pane.layout の rect を信じない）。
package bridge

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func helloMagic() []byte { return scrollBytes(HelloV2Dy) }

func (s *connSink) bytesCopy() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.buf...)
}

func TestBridgeV2SourceSizeAndHistoryPrefix(t *testing.T) {
	_, hc := startHerdr(t)
	ws, err := hc.WorkspaceCreate()
	if err != nil {
		t.Fatalf("workspace.create: %v", err)
	}
	paneID := ws.RootPane.PaneID

	// --- 1) pane の中で実サイズを測り、可視画面より多い行を出して scrollback を作る ---
	if err := hc.PaneSendText(paneID, "stty size; for i in $(seq 1 120); do echo HD_HIST_$i; done\r"); err != nil {
		t.Fatalf("send_text: %v", err)
	}
	waitFor(t, 15*time.Second, "履歴マーカーが出る", func() (bool, error) {
		rd, err := hc.PaneRead(paneID, "visible")
		if err != nil {
			return false, err
		}
		return strings.Contains(rd.Text, "HD_HIST_120"), nil
	})
	rd, err := hc.PaneReadLines(paneID, "recent", HistoryLines)
	if err != nil {
		t.Fatalf("pane.read recent: %v", err)
	}
	m := regexp.MustCompile(`(?m)^(\d+) (\d+)\s*$`).FindStringSubmatch(rd.Text)
	if m == nil {
		t.Fatalf("stty size の出力が履歴に無い: %.400q", rd.Text)
	}
	rows, _ := strconv.Atoi(m[1])
	cols, _ := strconv.Atoi(m[2])
	t.Logf("pane 実サイズ（stty）: %dx%d", cols, rows)
	vis, _ := hc.PaneRead(paneID, "visible")
	if regexp.MustCompile(`(?m)^HD_HIST_1\s*$`).MatchString(vis.Text) {
		t.Fatalf("HD_HIST_1 がまだ可視画面にある＝scrollback が出来ていない（rows=%d）", rows)
	}

	// --- 2) v2 viewer: HELLO → 旧来の巨大 RESIZE(500x160) ---
	viewerConn, bridgeConn := net.Pipe()
	defer viewerConn.Close()
	sink := newConnSink(viewerConn)
	b := New(paneID, bridgeConn, hc)
	b.Idle = -1
	b.Logf = t.Logf
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- b.Run(ctx) }()

	if _, err := viewerConn.Write(append(helloMagic(), resizeMagic(500, 160)...)); err != nil {
		t.Fatalf("write HELLO+RESIZE: %v", err)
	}
	waitFor(t, 15*time.Second, "初回 frame (DECSET 2026)", func() (bool, error) {
		return sink.containsFrom(0, []byte("\x1b[?2026h")), nil
	})
	// 行は pty の真値（stty と一致）。桁は pane.layout の rect.width（本番 GUI では
	// pty 桁と一致。headless 隔離サーバでは pty 桁の上界＝広い側に外れるだけで
	// クリップしない）。狭い側に外れたら右端が切れる＝不合格。
	lay, err := hc.PaneLayout(paneID)
	if err != nil {
		t.Fatalf("pane.layout: %v", err)
	}
	layCols := 0
	for _, p := range lay.Panes {
		if p.PaneID == paneID {
			layCols = p.Rect.Width
		}
	}
	if layCols < cols {
		t.Fatalf("pane.layout の桁 %d が pty 桁 %d より狭い（observe が右端をクリップする）", layCols, cols)
	}
	args := strings.Join(b.observeArgs(), " ")
	if want := fmt.Sprintf("--cols %d --rows %d", layCols, rows); !strings.Contains(args, want) {
		t.Fatalf("observe argv が元 pane 実サイズ(%s: layout 桁×pty 行)でない: %q（stty=%dx%d）", want, args, cols, rows)
	}
	cols = layCols

	// --- 3) ストリーム先頭の順序: META → 履歴（HD_HIST_1 を含む）→ hist_end → frame ---
	buf := sink.bytesCopy()
	metaWant := MetaBegin(cols, rows, false) // 120 行 < 1000 行上限＝truncated=0
	if !bytes.HasPrefix(buf, metaWant) {
		t.Fatalf("ストリーム先頭が META でない: want=%q got=%.80q", metaWant, buf)
	}
	iEnd := bytes.Index(buf, MetaHistEnd())
	iFrame := bytes.Index(buf, []byte("\x1b[?2026h"))
	if iEnd < 0 || iFrame < 0 || iEnd > iFrame {
		t.Fatalf("順序異常: hist_end=%d frame=%d", iEnd, iFrame)
	}
	hist := string(buf[len(metaWant):iEnd])
	if !regexp.MustCompile(`(?m)^HD_HIST_1\r$`).MatchString(hist) {
		t.Fatalf("履歴に HD_HIST_1（scrollback 行）が無い: %.300q", hist)
	}
	if strings.Contains(hist, "HD_HIST_120\r\n") {
		t.Fatalf("可視画面の行（HD_HIST_120）が履歴に混入している")
	}
	if !strings.HasSuffix(hist, strings.Repeat("\r\n", rows+1)) {
		t.Fatalf("履歴末尾に rows(%d) 行の CRLF パディングが無い: %.60q", rows, hist[len(hist)-60:])
	}
	if strings.Contains(hist, "\n\n") {
		t.Fatalf("履歴に裸の LF がある")
	}

	// --- 4) observe を外部 kill → respawn しても META/履歴は再送しない（1 接続 1 回） ---
	pid1 := b.observePID()
	off := sink.len()
	if err := syscall.Kill(pid1, syscall.SIGKILL); err != nil {
		t.Fatalf("kill observe: %v", err)
	}
	waitFor(t, 15*time.Second, "respawn 後に frame が再開する", func() (bool, error) {
		pid := b.observePID()
		return pid != 0 && pid != pid1 && sink.containsFrom(off, []byte("\x1b[?2026h")), fmt.Errorf("pid=%d", pid)
	})
	if n := bytes.Count(sink.bytesCopy(), []byte(metaOSC+"v=2")); n != 1 {
		t.Fatalf("META が %d 回流れた（1 接続につき 1 回のはず）", n)
	}
	if args := strings.Join(b.observeArgs(), " "); !strings.Contains(args, fmt.Sprintf("--cols %d --rows %d", cols, rows)) {
		t.Fatalf("respawn 後の observe argv が元 pane 実サイズでない: %q", args)
	}

	cancel()
	select {
	case <-runDone:
	case <-time.After(10 * time.Second):
		t.Fatal("ctx cancel 後 10s で Run が戻らない")
	}
}

// TestBridgeLegacyViewerGetsNoMeta — HELLO を送らない viewer（旧 Web・↗窓 attach）
// は従来どおり: 要求サイズで observe し、META/履歴は一切流れない。
func TestBridgeLegacyViewerGetsNoMeta(t *testing.T) {
	_, hc := startHerdr(t)
	ws, err := hc.WorkspaceCreate()
	if err != nil {
		t.Fatalf("workspace.create: %v", err)
	}
	paneID := ws.RootPane.PaneID

	viewerConn, bridgeConn := net.Pipe()
	defer viewerConn.Close()
	sink := newConnSink(viewerConn)
	b := New(paneID, bridgeConn, hc)
	b.Idle = -1
	b.Logf = t.Logf
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- b.Run(ctx) }()

	if _, err := viewerConn.Write(resizeMagic(24, 80)); err != nil {
		t.Fatalf("write RESIZE: %v", err)
	}
	waitFor(t, 15*time.Second, "初回 frame (DECSET 2026)", func() (bool, error) {
		return sink.containsFrom(0, []byte("\x1b[?2026h")), nil
	})
	if args := strings.Join(b.observeArgs(), " "); !strings.Contains(args, "--cols 80 --rows 24") {
		t.Fatalf("旧 viewer の observe argv が要求サイズでない: %q", args)
	}
	if sink.containsFrom(0, []byte(metaOSC)) {
		t.Fatalf("旧 viewer に META が流れた")
	}
	cancel()
	<-runDone
}
