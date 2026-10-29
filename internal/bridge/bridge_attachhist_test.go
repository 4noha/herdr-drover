//go:build !windows

// bridge_attachhist_test.go — ↗窓 attach 向け「履歴のみ HELLO」の実 herdr 統合テスト。
// 鉄則どおり合成ストリームで緑にしない: 隔離 herdr に**2 つの実 pane**を作る。
//   - source pane: 画面より多い行を出して scrollback を作る（リモート側のセッション）
//   - viewer pane : FIFO を cat するだけの pane（↗窓 pane の代役。bridge の出力を
//     そのまま端末として解釈する＝本物の attach と同じく herdr の端末エミュレータを通る）
//
// 検証は viewer pane の herdr scrollback（pane.read recent）に、source の**可視画面より前の
// 行**が遡れる形で入っているか。HELLO を送らない従来経路（対照）では入らないことも見る。
package bridge

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/4noha/herdr-drover/internal/herdrapi"
)

func helloHistMagic() []byte { return scrollBytes(HelloHistDy) }

// viewerPane は FIFO を cat する pane を作り、その FIFO への書込口を返す。
func viewerPane(t *testing.T, hc *herdrapi.Client) (paneID string, w io.WriteCloser, rows, cols int) {
	t.Helper()
	ws, err := hc.WorkspaceCreate()
	if err != nil {
		t.Fatalf("workspace.create: %v", err)
	}
	paneID = ws.RootPane.PaneID
	fifo := filepath.Join(t.TempDir(), "v.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	if err := hc.PaneSendText(paneID, "stty -echo; cat "+fifo+"\r"); err != nil {
		t.Fatalf("send_text: %v", err)
	}
	f, err := os.OpenFile(fifo, os.O_WRONLY, 0) // cat が開くまでブロックする
	if err != nil {
		t.Fatalf("open fifo: %v", err)
	}
	pi, err := hc.PaneGet(paneID)
	if err != nil {
		t.Fatalf("pane.get: %v", err)
	}
	lay, err := hc.PaneLayout(paneID)
	if err != nil {
		t.Fatalf("pane.layout: %v", err)
	}
	for _, p := range lay.Panes {
		if p.PaneID == paneID {
			cols = p.Rect.Width
		}
	}
	return paneID, f, pi.Scroll.ViewportRows, cols
}

func runAttachBridge(t *testing.T, hc *herdrapi.Client, src string, hello bool, out io.Writer, rows, cols int) (stop func()) {
	t.Helper()
	viewerConn, bridgeConn := net.Pipe()
	b := New(src, bridgeConn, hc)
	b.Idle = -1
	b.Logf = t.Logf
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.Run(ctx) }()
	go func() { _, _ = io.Copy(out, viewerConn) }()
	var hs []byte
	if hello {
		hs = helloHistMagic()
	}
	if _, err := viewerConn.Write(append(hs, resizeMagic(rows, cols)...)); err != nil {
		t.Fatalf("write HELLO+RESIZE: %v", err)
	}
	return func() { cancel(); viewerConn.Close(); <-done }
}

func TestAttachHistoryOnlyReachesViewerScrollback(t *testing.T) {
	_, hc := startHerdr(t)

	// --- source: 可視画面より多い行（scrollback あり）---
	ws, err := hc.WorkspaceCreate()
	if err != nil {
		t.Fatalf("workspace.create: %v", err)
	}
	src := ws.RootPane.PaneID
	if err := hc.PaneSendText(src, "for i in $(seq 1 150); do echo AT_HIST_$i; done\r"); err != nil {
		t.Fatalf("send_text: %v", err)
	}
	waitFor(t, 15*time.Second, "source に履歴が出る", func() (bool, error) {
		rd, err := hc.PaneRead(src, "visible")
		if err != nil {
			return false, err
		}
		return strings.Contains(rd.Text, "AT_HIST_150"), nil
	})

	oneLine := func(id string) *regexp.Regexp { return regexp.MustCompile(`(?m)^` + id + `\s*$`) }

	// --- 対照: HELLO なし（従来の attach）。viewer pane の scrollback に過去行は入らない ---
	vpOld, wOld, rowsOld, colsOld := viewerPane(t, hc)
	stopOld := runAttachBridge(t, hc, src, false, wOld, rowsOld, colsOld)
	waitFor(t, 20*time.Second, "従来経路: viewer に可視画面が描かれる", func() (bool, error) {
		rd, err := hc.PaneRead(vpOld, "visible")
		if err != nil {
			return false, err
		}
		return strings.Contains(rd.Text, "AT_HIST_150"), nil
	})
	rdOld, err := hc.PaneReadLines(vpOld, "recent", HistoryLines)
	if err != nil {
		t.Fatalf("pane.read recent(従来): %v", err)
	}
	if oneLine("AT_HIST_1").MatchString(rdOld.Text) {
		t.Fatalf("対照が成立していない: HELLO なしでも viewer の scrollback に AT_HIST_1 がある")
	}
	stopOld()
	wOld.Close()

	// --- 履歴のみ HELLO: viewer pane の scrollback に過去の行が入る ---
	vp, w, rows, cols := viewerPane(t, hc)
	stop := runAttachBridge(t, hc, src, true, w, rows, cols)
	defer func() { stop(); w.Close() }()
	waitFor(t, 20*time.Second, "viewer の scrollback に AT_HIST_1 が遡れる形で入る", func() (bool, error) {
		rd, err := hc.PaneReadLines(vp, "recent", HistoryLines)
		if err != nil {
			return false, err
		}
		return oneLine("AT_HIST_1").MatchString(rd.Text), nil
	})
	// 画面（可視）には source の現在の画面が描かれている。
	waitFor(t, 15*time.Second, "viewer の可視画面に source の現在の画面が描かれる", func() (bool, error) {
		vis, err := hc.PaneRead(vp, "visible")
		if err != nil {
			return false, err
		}
		return strings.Contains(vis.Text, "AT_HIST_150"), nil
	})
	rd, _ := hc.PaneReadLines(vp, "recent", HistoryLines)
	// 重複なし・欠落なし: 1..(150 - 可視行) の各行がちょうど 1 回、順番に並ぶ。
	last := 0
	for _, l := range strings.Split(rd.Text, "\n") {
		if m := regexp.MustCompile(`^AT_HIST_(\d+)\s*$`).FindStringSubmatch(l); m != nil {
			var n int
			fmt.Sscanf(m[1], "%d", &n)
			if n <= last && n != 150 { // 最終画面ぶんは scrollback と可視で隣接してよい
				t.Fatalf("履歴が重複/逆順: %d の後に %d", last, n)
			}
			last = n
		}
	}
	if strings.Contains(rd.Text, "\x1b]7707") {
		t.Fatalf("履歴のみ HELLO なのに OSC 7707（META）が流れている")
	}
}

func TestHistoryPayloadPadSeparatesDropAndPad(t *testing.T) {
	var sb strings.Builder
	for i := 1; i <= 10; i++ {
		fmt.Fprintf(&sb, "L%d\n", i)
	}
	// 元 pane の可視 4 行を落とし、attach 側 7 行ぶんを詰める。
	got := string(HistoryPayloadPad(sb.String(), 4, 7))
	want := "L1\r\nL2\r\nL3\r\nL4\r\nL5\r\nL6\r\n" + strings.Repeat("\r\n", 7)
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if HistoryPayloadPad(sb.String(), 10, 7) != nil || HistoryPayloadPad("", 4, 7) != nil || HistoryPayloadPad(sb.String(), 4, 0) != nil {
		t.Fatal("履歴なし/可視以下/pad 0 は nil のはず")
	}
	if string(HistoryPayload(sb.String(), 4)) != string(HistoryPayloadPad(sb.String(), 4, 4)) {
		t.Fatal("HistoryPayload は drop=pad の特殊形のはず")
	}
}
