//go:build unix

// attachvis.go — ↗窓（attach）の表示判定。
//
// ↗窓は reconcile が**他 PC の全セッション**ぶん作る常駐 pane で、ほとんどは
// 隠れた Tab にある。従来は隠れていても relay に繋ぎ続け、source 側の observe が
// 全画面フレームを流し続けていた（Cloud Run の外向き転送の主因・2026-11-03 実測:
// 見ていない ↗窓が 1 PC あたり 50〜60 本）。
//
// 判定: 自 pane の Tab が、その Workspace の**アクティブ Tab**なら「表示中」。
// herdr の購読イベント（tab.focused 等）で追い、周期 poll を backstop にする
// （DESIGN: event は nudge・poll 常設）。Workspace の focused は見ない＝複数の
// client 窓で別 Workspace を出していても、表示中の窓が止まらない側に倒す。
//
// 非表示の間は relay に**接続しない**（PAUSE 命令を増やさず、接続を閉じるだけ。
// source は viewer 切断で bridge/observe を畳む＝source 側の変更なしで効く）。
// 表示に戻ったら従来どおり再接続し、RESIZE で全画面を 1 回描き直す。
// エージェントの状態（agent_status）は Firestore の session doc で別経路＝影響なし。
//
// HERDR_PANE_ID が無い／herdr に繋げない／判定に失敗した場合は「表示中」扱い
// （従来どおり常時接続）。黙って止めない＝attach.log に残す。
package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/4noha/herdr-drover/internal/herdrapi"
)

// visEvents は表示状態が変わりうる herdr の購読名（dot 形）。不正名は購読が即
// エラーで終わる（herdrapi.Subscribe の契約）ので、名は herdr 0.9.5 の
// events.subscribe のエラー応答（全列挙）から写した。
var visEvents = []string{
	"tab.focused", "workspace.focused", "tab.closed", "tab.moved",
	"pane.moved", "layout.updated", "workspace.updated",
}

// visPollInterval は周期 poll（購読が切れている間・event 取りこぼしの backstop）。
const visPollInterval = 30 * time.Second

// visSettle は event 直後に 1 回だけ追加で判定し直す遅延（Tab 切替の event が
// 状態反映より先に届く可能性への保険。実測で必要になったわけではない）。
const visSettle = 500 * time.Millisecond

type visWatcher struct {
	mu      sync.Mutex
	visible bool
	changed chan struct{} // 状態が変わるたび close して張り替える（relay の change と同型）
	paneID  string
	hc      *herdrapi.Client
	lg      *attachLogger
}

// startVisWatcher は表示判定を起動する。paneID が空なら nil（常時接続）。
// 初回判定は同期（起動直後の非表示 ↗窓が 1 回無駄に繋がない）。
func startVisWatcher(ctx context.Context, paneID string, hc *herdrapi.Client, lg *attachLogger) *visWatcher {
	if paneID == "" {
		lg.Printf("表示判定: HERDR_PANE_ID 無し＝常時接続（従来どおり）")
		return nil
	}
	v := &visWatcher{visible: true, changed: make(chan struct{}), paneID: paneID, hc: hc, lg: lg}
	v.refresh("起動")
	visible, _ := v.state()
	lg.Printf("表示判定: 開始 pane=%s 初回=%s（Tab がアクティブな間だけ接続する）", paneID, map[bool]string{true: "表示中", false: "非表示"}[visible])
	go v.run(ctx)
	return v
}

// state は現在の表示状態と、次に変わったとき close されるチャネルを返す。
func (v *visWatcher) state() (visible bool, changed <-chan struct{}) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.visible, v.changed
}

// waitVisible は表示中になるまで待つ。ctx 終了で false。
func (v *visWatcher) waitVisible(ctx context.Context) bool {
	for {
		visible, ch := v.state()
		if visible {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-ch:
		}
	}
}

func (v *visWatcher) set(visible bool, why string) {
	v.mu.Lock()
	if visible == v.visible {
		v.mu.Unlock()
		return
	}
	v.visible = visible
	close(v.changed)
	v.changed = make(chan struct{})
	v.mu.Unlock()
	if visible {
		v.lg.Printf("表示判定: 表示中（%s）＝接続する", why)
	} else {
		v.lg.Printf("表示判定: 非表示（%s）＝表示に戻るまで接続しない", why)
	}
}

func (v *visWatcher) refresh(why string) {
	visible, err := paneVisible(v.hc, v.paneID)
	if err != nil {
		// 判定できない＝表示中扱い（従来どおり繋ぐ）。黙らない。
		v.set(true, why+"・判定失敗: "+err.Error())
		return
	}
	v.set(visible, why)
}

// paneVisible は「pane の Tab が、その Workspace のアクティブ Tab か」。
// pane.get の tab_id と workspace.list の active_tab_id の exact-match のみ。
func paneVisible(hc *herdrapi.Client, paneID string) (bool, error) {
	pi, err := hc.PaneGet(paneID)
	if err != nil {
		return false, fmt.Errorf("pane.get: %w", err)
	}
	wss, err := hc.WorkspaceList()
	if err != nil {
		return false, fmt.Errorf("workspace.list: %w", err)
	}
	for _, ws := range wss {
		if ws.WorkspaceID == pi.WorkspaceID {
			return ws.ActiveTabID == pi.TabID, nil
		}
	}
	return false, fmt.Errorf("pane %s の workspace %s が workspace.list に無い", paneID, pi.WorkspaceID)
}

func (v *visWatcher) run(ctx context.Context) {
	ch := make(chan herdrapi.Event, 64)
	ready := make(chan struct{}, 1)
	go func() {
		if err := v.hc.SubscribeReady(ctx, visEvents, ch, ready); err != nil && ctx.Err() == nil {
			// 購読名不正等。周期 poll だけで続ける（30s 遅れで追従）。
			v.lg.Printf("表示判定: 購読終了（周期 %s の判定だけで続ける）: %v", visPollInterval, err)
		}
	}()
	poll := time.NewTicker(visPollInterval)
	defer poll.Stop()
	settle := time.NewTimer(time.Hour)
	settle.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ready:
			v.refresh("購読確立")
		case ev := <-ch:
			v.refresh(ev.Name)
			settle.Reset(visSettle)
		case <-settle.C:
			v.refresh("event 後の再確認")
		case <-poll.C:
			v.refresh("周期")
		}
	}
}
