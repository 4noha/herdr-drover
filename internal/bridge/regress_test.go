//go:build !windows

// regress_test.go — 敵対的レビューで実 herdr 再現された 3 欠陥の回帰テスト。
// いずれも修正前の旧コードで FAIL することを確認済み（鉄則2）:
//
//  1. stalled viewer への frame 書込中に RESIZE → Run が proc.stop() 内
//     <-p.done で無期限凍結（cm は renderClientLocked の 2s write deadline で
//     同クラスを解いている＝本修正も同じ 2s deadline）
//  2. conn read 境界が UTF-8 rune を跨ぐ → sendInput が fallback（control
//     bytes）へ落ち、control attach の実測副作用で実 PTY が 120x40 に resize
//     される（relay の 32KB chunk 中継で現実に発生する）
//  3. observe の stderr を io.Discard で捨てる → spawn 即死の真因（socket
//     不達等は stderr にしか出ない・実測）が構造的に失われる（cm tmux.go
//     new-window silent fail の教訓の再演）
package bridge

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"context"

	"github.com/4noha/herdr-drover/internal/herdrapi"
)

// logRecorder は Logf 出力を並行安全に蓄積する（Run goroutine が書き、
// テスト本体が読む）。
type logRecorder struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logRecorder) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(&l.b, format+"\n", args...)
}

func (l *logRecorder) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestStalledViewerResizeDoesNotWedge — 欠陥1 の回帰。
//
// viewer が読まなくなった（TCP backpressure/net.Pipe 無バッファ）状態で
// frame writer が Conn.Write にブロック中、RESIZE を受けた Run が
// proc.stop() の <-p.done で凍結しないこと。修正後は per-frame write
// deadline（2s）が Write を解いて connErr（データ線死）として Run が
// 有限時間で戻る。旧コードは Idle 無効だと永遠に戻らず本テストが FAIL する
// （実運用は Idle=30s が唯一の弁＝復帰がデータ線全切断になっていた）。
func TestStalledViewerResizeDoesNotWedge(t *testing.T) {
	_, hc := startHerdr(t)
	ws, err := hc.WorkspaceCreate()
	if err != nil {
		t.Fatalf("workspace.create: %v", err)
	}
	viewerConn, bridgeConn := net.Pipe()
	defer viewerConn.Close()

	b := New(ws.RootPane.PaneID, bridgeConn, hc)
	b.Idle = -1 // idle 弁を切る＝write deadline だけで凍結が解けることを証明する
	b.Logf = t.Logf
	runDone := make(chan error, 1)
	go func() { runDone <- b.Run(context.Background()) }()

	// 初回 frame まで手動 drain → 以後読むのをやめる（stall）。
	if _, err := viewerConn.Write(resizeMagic(24, 80)); err != nil {
		t.Fatalf("write RESIZE: %v", err)
	}
	var got []byte
	buf := make([]byte, 64*1024)
	deadline := time.Now().Add(15 * time.Second)
	for !bytes.Contains(got, []byte("\x1b[?2026h")) {
		if time.Now().After(deadline) {
			t.Fatalf("初回 frame が来ない (got=%d bytes)", len(got))
		}
		_ = viewerConn.SetReadDeadline(time.Now().Add(15 * time.Second))
		n, rerr := viewerConn.Read(buf)
		if n > 0 {
			got = append(got, buf[:n]...)
		}
		if rerr != nil {
			t.Fatalf("read: %v", rerr)
		}
	}
	pid1 := b.observePID()

	// 入力で pane を変化させ frame を生成 → writer が Conn.Write でブロック。
	if _, err := viewerConn.Write([]byte("x")); err != nil {
		t.Fatalf("write input: %v", err)
	}
	time.Sleep(1 * time.Second)

	// RESIZE → 旧コードはここで Run が proc.stop() 内に凍結する。
	if _, err := viewerConn.Write(resizeMagic(33, 100)); err != nil {
		t.Fatalf("write RESIZE2: %v", err)
	}

	// 修正後: write deadline(2s) → connErr → Run が有限時間で error 復帰。
	select {
	case err := <-runDone:
		if err == nil {
			t.Fatalf("stalled viewer では connErr（データ線死）で戻るべき（nil で戻った）")
		}
		t.Logf("Run 復帰: %v", err)
	case <-time.After(8 * time.Second):
		t.Fatalf("RESIZE 後 8s 経っても Run が戻らない＝proc.stop 凍結（旧バグ）")
	}
	// observe サブプロセスが残らない（リーク禁止）。
	waitFor(t, 5*time.Second, "observe 掃除", func() (bool, error) {
		return !pidAlive(pid1) && b.observePID() == 0, nil
	})
}

// TestUTF8SplitRuneCarryStaysPrimary — 欠陥2 の回帰。
//
// 「あ」(E3 81 82) を read 境界で分割着信させても、不完全 rune 先頭断片の
// 繰越し（cmwire の末尾孤立 0xff 繰越しと同じ規律）で primary（send_text）に
// 留まり、fallback（control attach）の実測副作用「実 PTY が 120x40 に
// resize」が起きないこと。旧コードは両断片が utf8.Valid false → fallback →
// viewport_rows 23→40 を実測（FAIL 確認済み）。
func TestUTF8SplitRuneCarryStaysPrimary(t *testing.T) {
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
	go func() { _ = b.Run(ctx) }()

	if _, err := viewerConn.Write(resizeMagic(24, 80)); err != nil {
		t.Fatalf("write RESIZE: %v", err)
	}
	waitFor(t, 15*time.Second, "初回 frame", func() (bool, error) {
		return sink.containsFrom(0, []byte("\x1b[?2026h")), nil
	})

	// raw capture pane（TestSendInputRouting と同一方式＝zle の介在を排除）。
	waitFor(t, 15*time.Second, "pane 準備", func() (bool, error) {
		if err := hc.PaneSendText(paneID, "echo HD_CR_'UP'\r"); err != nil {
			return false, err
		}
		rd, err := hc.PaneRead(paneID, "visible")
		if err != nil {
			return false, err
		}
		return strings.Contains(rd.Text, "HD_CR_UP"), nil
	})
	capFile := filepath.Join(t.TempDir(), "cap.bin")
	if err := hc.PaneSendText(paneID, "stty raw -echo; cat > "+capFile+"\r"); err != nil {
		t.Fatalf("start capture: %v", err)
	}
	waitFor(t, 10*time.Second, "capture ファイル出現", func() (bool, error) {
		_, err := os.Stat(capFile)
		return err == nil, err
	})

	before, err := hc.PaneGet(paneID)
	if err != nil {
		t.Fatalf("PaneGet: %v", err)
	}

	// 「あ」を read 境界で分割着信（net.Pipe は Write 境界を保存＝relay の
	// 32KB chunk 境界シミュレーション）。
	if _, err := viewerConn.Write([]byte{0xE3}); err != nil {
		t.Fatalf("write part1: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if _, err := viewerConn.Write([]byte{0x81, 0x82}); err != nil {
		t.Fatalf("write part2: %v", err)
	}

	// byte-perfect 到達（繰越し再結合後に 1 回で届く）。
	waitFor(t, 20*time.Second, "分割 rune のバイト到達", func() (bool, error) {
		got, err := os.ReadFile(capFile)
		if err != nil {
			return false, err
		}
		return bytes.Equal(got, []byte{0xE3, 0x81, 0x82}), fmt.Errorf("got=%x", got)
	})

	// fallback の副作用（control attach の実 PTY resize）が無いこと。
	after, err := hc.PaneGet(paneID)
	if err != nil {
		t.Fatalf("PaneGet after: %v", err)
	}
	if after.Scroll.ViewportRows != before.Scroll.ViewportRows {
		t.Fatalf("fallback へ落ちて実 PTY が resize された（旧バグ）: viewport_rows %d -> %d",
			before.Scroll.ViewportRows, after.Scroll.ViewportRows)
	}
}

// TestSplitIncompleteRune — 繰越し判定の純関数を機械的に網羅（実キーパスは
// 上の実 herdr テストが担う。ここは境界規則の固定）。
func TestSplitIncompleteRune(t *testing.T) {
	cases := []struct {
		name       string
		in         string
		head, tail string
	}{
		{"空", "", "", ""},
		{"ASCII 完結", "abc", "abc", ""},
		{"3B rune 完結", "あ", "あ", ""},
		{"4B rune 完結", "\U0001F600", "\U0001F600", ""},
		{"2B 先頭のみ", "\xC3", "", "\xC3"},
		{"3B 先頭 1B", "ab\xE3", "ab", "\xE3"},
		{"3B 先頭 2B", "ab\xE3\x81", "ab", "\xE3\x81"},
		{"4B 先頭 3B", "ab\xF0\x9F\x98", "ab", "\xF0\x9F\x98"},
		{"不正先頭 0xFE は繰り越さない", "ab\xFE", "ab\xFE", ""},
		{"孤立継続バイトは繰り越さない", "ab\x80", "ab\x80", ""},
		{"継続バイトのみは繰り越さない", "\x80\x80\x80\x80", "\x80\x80\x80\x80", ""},
		{"先頭断片の後に非継続＝不正列は即送出", "\xC3\x28", "\xC3\x28", ""},
		{"TestSendInputRouting の非UTF8列", "\xC3\x28\x80\xFEEND", "\xC3\x28\x80\xFEEND", ""},
	}
	for _, c := range cases {
		head, tail := SplitIncompleteRune([]byte(c.in))
		if string(head) != c.head || string(tail) != c.tail {
			t.Errorf("%s: SplitIncompleteRune(%q) = (%q, %q), want (%q, %q)",
				c.name, c.in, head, tail, c.head, c.tail)
		}
	}
}

// TestObserveStderrSurfacedOnSpawnFailure — 欠陥3 の回帰。
//
// observe が即死する真因のうち「server へ接続できない」類は stdout の
// terminal.closed でなく stderr にのみ出る（実 herdr 0.7.4 実測:
// `herdr: failed to connect to server: ...` exit=1）。io.Discard で捨てると
// respawn ストーム時に一次情報が構造的に欠落する（cm new-window silent fail
// の教訓）。修正後は stderr 末尾が exit status とともに Logf へ出る。
func TestObserveStderrSurfacedOnSpawnFailure(t *testing.T) {
	if _, err := exec.LookPath("herdr"); err != nil {
		t.Skip("herdr not installed; skipping real-binary test")
	}
	// 実在しない socket を指す client → observe サブプロセスは connect 失敗を
	// stderr に出して exit 1 する（サーバ不要＝実バイナリの実失敗経路）。
	hc := herdrapi.New(filepath.Join(t.TempDir(), "no.sock"))

	viewerConn, bridgeConn := net.Pipe()
	defer viewerConn.Close()
	_ = newConnSink(viewerConn)

	rec := &logRecorder{}
	b := New("w1:p1", bridgeConn, hc)
	b.Idle = -1
	b.Logf = rec.logf
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- b.Run(ctx) }()

	if _, err := viewerConn.Write(resizeMagic(24, 80)); err != nil {
		t.Fatalf("write RESIZE: %v", err)
	}
	// 旧コード: 「observe 終了（…後に respawn）」だけが繰り返され stderr の
	// 真因（failed to connect）はどこにも残らない＝ここで FAIL。
	waitFor(t, 15*time.Second, "stderr が Logf に現れる", func() (bool, error) {
		return strings.Contains(rec.String(), "failed to connect"), fmt.Errorf("logs=%q", rec.String())
	})
	cancel()
	select {
	case <-runDone:
	case <-time.After(10 * time.Second):
		t.Fatal("cancel 後 10s で Run が戻らない")
	}
}
