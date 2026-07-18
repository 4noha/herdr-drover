//go:build unix

package main

// localview の検証（鉄則: 合成で緑にしない。実 raw-mode TTY 出入りは pty
// ハーネス必須のため seam の外＝ここでは①prefix 純関数②detach 経路
// ③実 herdr での observe frame 流入・stdin→pane 到達・**observe が pane 実
// サイズを変えない（＝リサイズ権限を App に残す＝ロックフリーの核）**を検証）。

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/4noha/herdr-drover/internal/herdrapi"
)

// TestApplyPrefix は Ctrl-B プレフィックス状態機械（detach/リテラル/透過/
// read 境界跨ぎ）を機械検証する純関数テスト。
func TestApplyPrefix(t *testing.T) {
	const cb = "\x02"
	cases := []struct {
		name       string
		prefixIn   bool
		data       string
		wantFwd    string
		wantDetach bool
		prefixOut  bool
	}{
		{"素の文字は透過", false, "abc", "abc", false, false},
		{"Ctrl-B q は detach（以降破棄）", false, cb + "qXYZ", "", true, false},
		{"detach 前のバイトは転送してから detach", false, "ab" + cb + "q", "ab", true, false},
		{"Ctrl-B Ctrl-B はリテラル Ctrl-B 1 個", false, cb + cb, cb, false, false},
		{"Ctrl-B <他> は両方転送（キーを飲まない）", false, cb + "x", cb + "x", false, false},
		{"末尾 Ctrl-B は次 read へ保留", false, "a" + cb, "a", false, true},
		{"前 read 保留 Ctrl-B + q = detach", true, "q", "", true, false},
		{"前 read 保留 Ctrl-B + 通常 = 両方転送", true, "z", cb + "z", false, false},
		{"矢印キー ESC[A は透過", false, "\x1b[A", "\x1b[A", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			prefix := c.prefixIn
			fwd, det := applyPrefix(&prefix, []byte(c.data))
			if string(fwd) != c.wantFwd || det != c.wantDetach || prefix != c.prefixOut {
				t.Fatalf("applyPrefix(%v, %q) = (fwd=%q, detach=%v, prefixOut=%v), want (%q, %v, %v)",
					c.prefixIn, c.data, fwd, det, prefix, c.wantFwd, c.wantDetach, c.prefixOut)
			}
		})
	}
}

// TestReadStdinLoopDetach は Ctrl-B q で detach チャネルが閉じ readStdinLoop が
// 戻ることを検証する（PaneSendText は呼ばれない＝api は不使用ゆえ herdr 不要）。
func TestReadStdinLoopDetach(t *testing.T) {
	detach := make(chan struct{})
	done := make(chan struct{})
	go func() {
		readStdinLoop(strings.NewReader("\x02q"), herdrapi.New("/nonexistent-sock"), "p", detach, io.Discard)
		close(done)
	}()
	select {
	case <-detach:
	case <-time.After(3 * time.Second):
		t.Fatal("Ctrl-B q で detach が通知されない")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("detach 後に readStdinLoop が戻らない")
	}
}

// TestLocalViewObserveFramesAndLockFree は実 herdr で:
//  1. spawnObserveGen が observe（attach/control ではない）で frame を流す
//  2. readStdinLoop の stdin→pane.send_text が pane に到達し実行される
//  3. observe が pane の実サイズ（viewport_rows）を変えない＝リサイズ権限を
//     App に残す（＝直接アタッチのロックを張らない核心）
//
// を機械検証する。3 は「observe を pane 現寸と別サイズで起動しても pane.get の
// viewport_rows が不変」で示す（TerminalAttach なら pane を掴んで実サイズを
// attach サイズへ変える＝ここが観測可能な差）。
func TestLocalViewObserveFramesAndLockFree(t *testing.T) {
	sock := startHerdrForTest(t)
	api := herdrapi.New(sock)

	ws, err := api.WorkspaceCreate()
	if err != nil {
		t.Fatalf("workspace.create: %v", err)
	}
	paneID := ws.RootPane.PaneID

	// pane の初期 viewport_rows を確定（>0 になるまで待つ）。
	var r0 int
	waitCond(t, 15*time.Second, "pane の viewport_rows が確定", func() bool {
		p, e := api.PaneGet(paneID)
		if e != nil || p.Scroll.ViewportRows <= 0 {
			return false
		}
		r0 = p.Scroll.ViewportRows
		return true
	})

	// pane 現寸と必ず異なる observe サイズ（+10 行）。ここで pane が resize
	// されるなら TerminalAttach（ロック）＝我々の非採用経路。
	obsRows := r0 + 10
	out := &capBuf{} // limit 0＝無制限・スレッド安全（frame writer goroutine が書く）
	gen, err := spawnStreamGen("herdr", streamObserve, paneID, sock, obsRows, 100, out)
	if err != nil {
		t.Fatalf("spawnStreamGen(observe): %v", err)
	}
	defer gen.stop()

	// 構造的保証: 起動した子は observe（attach/control ではない）。
	args := strings.Join(gen.cmd.Args, " ")
	if !strings.Contains(args, " session observe ") {
		t.Fatalf("observe 以外で起動している（ロックを張る経路の疑い）: %q", args)
	}
	if strings.Contains(args, " attach") || strings.Contains(args, " control ") {
		t.Fatalf("attach/control（TerminalAttach＝ロック）で起動している: %q", args)
	}

	// 1) frame 流入（全 frame は DECSET 2026 括り＝実測の機械判定）。
	waitCond(t, 15*time.Second, "observe frame（DECSET 2026）がローカルへ流れる", func() bool {
		return strings.Contains(out.String(), "\x1b[?2026h")
	})

	// 2) stdin→pane.send_text 到達（クォート越しマーカー: 実行出力
	// HD_LV_OK の一致＝シェルが実行した証拠。send_text の \r 透過を兼ねる）。
	readStdinLoop(strings.NewReader("echo HD_LV_'OK'\r"), api, paneID, make(chan struct{}, 1), io.Discard)
	waitCond(t, 15*time.Second, "stdin 入力が pane で実行される", func() bool {
		rd, e := api.PaneRead(paneID, "visible")
		return e == nil && strings.Contains(rd.Text, "HD_LV_OK")
	})

	// 3) observe は pane 実サイズを変えない（リサイズ権限を App に残す）。
	// frame も入力往復も済んだ後に確認＝observe が十分に動いた状態での不変性。
	p, err := api.PaneGet(paneID)
	if err != nil {
		t.Fatalf("pane.get: %v", err)
	}
	if p.Scroll.ViewportRows != r0 {
		t.Fatalf("observe（%d 行）で pane の viewport_rows が %d→%d に変わった＝リサイズ権限を奪っている（ロックを張る経路の疑い）",
			obsRows, r0, p.Scroll.ViewportRows)
	}
}

// TestPickStreamMode は自動 min の決定的分岐（純関数）を機械検証する。
// ローカルが grid より縦に小さいときだけ control（縮小＋ロック）、それ以外は
// observe（ロック非取得）、grid 不明(0)は安全側 observe。
func TestPickStreamMode(t *testing.T) {
	cases := []struct {
		name                string
		localRows, gridRows int
		want                string
	}{
		{"ローカルが grid より小さい→control（縮小）", 30, 112, streamControl},
		{"ローカルが grid より大きい→observe（メイン優先）", 120, 112, streamObserve},
		{"同値→observe（余白ゼロ・ロック不要）", 40, 40, streamObserve},
		{"grid 不明(0)→observe（安全側・ロック非取得）", 30, 0, streamObserve},
		{"grid 負値→observe（安全側）", 30, -1, streamObserve},
	}
	for _, c := range cases {
		if got := pickStreamMode(c.localRows, c.gridRows); got != c.want {
			t.Errorf("%s: pickStreamMode(%d,%d)=%q want %q", c.name, c.localRows, c.gridRows, got, c.want)
		}
	}
}

// TestLocalViewControlShrinksGridWhenLocalSmaller は実 herdr で「自動 min の
// 縮小側」を機械検証する: ローカル端末が grid より小さいとき control モードが
// pane を local 実寸へ**実際に縮小＋ロック**する（＝下部入力までローカルに
// 収まる）。observe（上寄せクリップ）では viewport_rows が変わらないので、旧
// コードなら FAIL する差分＝この shrink が min ポリシーの核心。
func TestLocalViewControlShrinksGridWhenLocalSmaller(t *testing.T) {
	sock := startHerdrForTest(t)
	api := herdrapi.New(sock)

	ws, err := api.WorkspaceCreate()
	if err != nil {
		t.Fatalf("workspace.create: %v", err)
	}
	paneID := ws.RootPane.PaneID

	var r0 int
	waitCond(t, 15*time.Second, "pane の viewport_rows が確定", func() bool {
		p, e := api.PaneGet(paneID)
		if e != nil || p.Scroll.ViewportRows <= 0 {
			return false
		}
		r0 = p.Scroll.ViewportRows
		return true
	})
	if r0 < 12 {
		t.Skipf("初期 grid が小さすぎて縮小差分を作れない（r0=%d）", r0)
	}

	// pane より確実に小さいローカルサイズ。pickStreamMode は control を選ぶ。
	localRows := r0 - 6
	if got := pickStreamMode(localRows, r0); got != streamControl {
		t.Fatalf("pickStreamMode(%d,%d)=%q want control", localRows, r0, got)
	}

	out := &capBuf{}
	gen, err := spawnStreamGen("herdr", streamControl, paneID, sock, localRows, 100, out)
	if err != nil {
		t.Fatalf("spawnStreamGen(control): %v", err)
	}
	defer gen.stop()

	// 構造的保証: control（ControlTerminal＝ロック経路）で起動している。
	args := strings.Join(gen.cmd.Args, " ")
	if !strings.Contains(args, " session control ") {
		t.Fatalf("control 以外で起動している: %q", args)
	}

	// frame 流入（observe と同一 envelope＝表示コード共通の裏取り）。
	waitCond(t, 15*time.Second, "control frame（DECSET 2026）がローカルへ流れる", func() bool {
		return strings.Contains(out.String(), "\x1b[?2026h")
	})

	// 核心: pane 実サイズ（viewport_rows）が local へ縮む＝下部までローカルに収まる。
	waitCond(t, 15*time.Second, "control が pane grid を local 実寸へ縮小＋ロック", func() bool {
		p, e := api.PaneGet(paneID)
		return e == nil && p.Scroll.ViewportRows == localRows
	})

	// stop でロック解除（切断）＝リーク/固定を残さない。
	gen.stop()
}

// waitCond は fn が true になるまで poll（実 herdr の非同期反映を待つ）。
func waitCond(t *testing.T, timeout time.Duration, desc string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timeout: %s", desc)
}
