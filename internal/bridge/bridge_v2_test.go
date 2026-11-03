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

	"github.com/4noha/herdr-drover/internal/herdrapi"
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
	// 120 行 < 1000 行上限＝truncated=0・予算内に取得＝hist=1・遡りなし。hlines は
	// 実際に scrollback へ落ちた行数（shell のプロンプト等も含む）なので ≥1 を見る。
	metaPrefix := fmt.Sprintf("%sv=2;cols=%d;rows=%d;trunc=0;hist=1;hlines=", metaOSC, cols, rows)
	if !bytes.HasPrefix(buf, []byte(metaPrefix)) {
		t.Fatalf("ストリーム先頭が META でない: want prefix=%q got=%.80q", metaPrefix, buf)
	}
	metaLen := bytes.Index(buf, []byte(metaST)) + len(metaST)
	var hlines, scrolled int
	if _, err := fmt.Sscanf(string(buf[len(metaPrefix):metaLen]), "%d;scrolled=%d", &hlines, &scrolled); err != nil || hlines < 1 || scrolled != 0 {
		t.Fatalf("META の hlines/scrolled が不正: %q (err=%v)", buf[:metaLen], err)
	}
	iEnd := bytes.Index(buf, MetaHistEnd())
	iFrame := bytes.Index(buf, []byte("\x1b[?2026h"))
	if iEnd < 0 || iFrame < 0 || iEnd > iFrame {
		t.Fatalf("順序異常: hist_end=%d frame=%d", iEnd, iFrame)
	}
	hist := string(buf[metaLen:iEnd])
	if got := strings.Count(hist, "\r\n") - rows; got != hlines {
		t.Fatalf("hlines=%d だが履歴本文は %d 行", hlines, got)
	}
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

// TestBridgeV2HistoryBudgetFallsBackThenCaches — 履歴読取が予算内に終わらなければ
// hist=0 で先に描画し（META→frame が予算程度で届く）、読取はキャッシュ側で継続、
// 次の接続（別 Bridge・同じキャッシュ）では hist=1 で履歴が載る。本番実測
// （長寿命 pane で pane.read 4〜16s）の再現を seam で行う。
func TestBridgeV2HistoryBudgetFallsBackThenCaches(t *testing.T) {
	_, hc := startHerdr(t)
	ws, err := hc.WorkspaceCreate()
	if err != nil {
		t.Fatalf("workspace.create: %v", err)
	}
	paneID := ws.RootPane.PaneID
	if err := hc.PaneSendText(paneID, "for i in $(seq 1 120); do echo HD_SLOW_$i; done\r"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 15*time.Second, "HD_SLOW_120", func() (bool, error) {
		rd, err := hc.PaneRead(paneID, "visible")
		return err == nil && strings.Contains(rd.Text, "HD_SLOW_120"), err
	})
	cache := NewHistoryCache()
	slowRead := func() (*herdrapi.PaneReadInfo, error) {
		time.Sleep(1500 * time.Millisecond) // 本番の遅い pane.read を模す
		return hc.PaneReadLines(paneID, "recent", HistoryLines)
	}
	connect := func() (*Bridge, *connSink, func()) {
		viewerConn, bridgeConn := net.Pipe()
		sink := newConnSink(viewerConn)
		b := New(paneID, bridgeConn, hc)
		b.Idle = -1
		b.Logf = t.Logf
		b.HistoryCache = cache
		b.HistoryBudget = 300 * time.Millisecond
		b.readHistory = slowRead
		ctx, cancel := context.WithCancel(context.Background())
		go func() { _ = b.Run(ctx) }()
		if _, err := viewerConn.Write(append(helloMagic(), resizeMagic(500, 160)...)); err != nil {
			t.Fatal(err)
		}
		return b, sink, func() { cancel(); viewerConn.Close() }
	}

	// --- 1 接続目: 予算超過 → hist=0 で即 META＋frame ---
	t0 := time.Now()
	_, sink1, stop1 := connect()
	waitFor(t, 10*time.Second, "META(hist=0)", func() (bool, error) {
		return sink1.containsFrom(0, []byte(";hist=0;")), nil
	})
	waitFor(t, 10*time.Second, "frame", func() (bool, error) {
		return sink1.containsFrom(0, []byte("\x1b[?2026h")), nil
	})
	if d := time.Since(t0); d > 1200*time.Millisecond {
		t.Fatalf("予算 300ms なのに初回描画まで %s", d)
	}
	if sink1.textContainsFrom(0, "HD_SLOW_1\r") {
		t.Fatalf("hist=0 なのに履歴本文が流れた")
	}
	stop1()

	// --- 読取完了を待って 2 接続目: キャッシュから hist=1 で履歴が載る ---
	time.Sleep(1800 * time.Millisecond)
	_, sink2, stop2 := connect()
	defer stop2()
	waitFor(t, 10*time.Second, "META(hist=1)＋履歴", func() (bool, error) {
		return sink2.containsFrom(0, []byte(";hist=1;")) &&
			regexp.MustCompile(`(?m)^HD_SLOW_1\r$`).Match(sink2.bytesCopy()), nil
	})
}

// TestBridgeV2ScrollBottom — viewer の［最新へ］（SCROLL dy=ScrollBottomDy）で
// 元 pane の実スクロールが最下部へ戻る（pane.scroll 0）。
func TestBridgeV2ScrollBottom(t *testing.T) {
	_, hc := startHerdr(t)
	ws, err := hc.WorkspaceCreate()
	if err != nil {
		t.Fatalf("workspace.create: %v", err)
	}
	paneID := ws.RootPane.PaneID
	if err := hc.PaneSendText(paneID, "for i in $(seq 1 120); do echo HD_SCR_$i; done\r"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 15*time.Second, "HD_SCR_120", func() (bool, error) {
		rd, err := hc.PaneRead(paneID, "visible")
		return err == nil && strings.Contains(rd.Text, "HD_SCR_120"), err
	})
	if err := hc.PaneScroll(paneID, 5); err != nil {
		t.Fatalf("pane.scroll 5: %v", err)
	}
	pi, err := hc.PaneGet(paneID)
	if err != nil || pi.Scroll.OffsetFromBottom != 5 {
		t.Fatalf("前提: ローカル遡り 5 行にできない: %+v %v", pi, err)
	}

	viewerConn, bridgeConn := net.Pipe()
	defer viewerConn.Close()
	sink := newConnSink(viewerConn)
	b := New(paneID, bridgeConn, hc)
	b.Idle = -1
	b.Logf = t.Logf
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = b.Run(ctx) }()
	if _, err := viewerConn.Write(append(helloMagic(), resizeMagic(500, 160)...)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 15*time.Second, "META に scrolled=5", func() (bool, error) {
		return sink.containsFrom(0, []byte(";scrolled=5")), nil
	})
	if _, err := viewerConn.Write(scrollBytes(ScrollBottomDy)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "pane.scroll 0 が効く", func() (bool, error) {
		pi, err := hc.PaneGet(paneID)
		return err == nil && pi.Scroll.OffsetFromBottom == 0, err
	})
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
