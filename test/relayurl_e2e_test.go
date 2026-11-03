//go:build !windows

// relayurl_e2e_test.go — settings/relay（Firestore）で WS 中継の接続先を一括切替できることの
// 通し試験。agent（source）と ↗窓 attach（viewer）の設定 URL を**誰も居ないアドレス**にし、
// settings/relay に実 relay を書く。画面が出るなら両方が切替先で繋いでいる。
package e2e

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestE2EAgentAndAttachFollowRelayURLSetting(t *testing.T) {
	const dead = "ws://127.0.0.1:9" // 設定の URL（繋がらない）
	src := startSourceRigRelayEnv(t, "e2e-ru-src-herdr", dead)
	ctx := context.Background()
	t.Cleanup(func() { _ = src.st.SetRelayURL(ctx, "") })
	if err := src.st.SetRelayURL(ctx, src.relayURL); err != nil {
		t.Fatal(err)
	}
	src.tick(t, "RU")

	_, vhc := startHerdr(t)
	ws, err := vhc.WorkspaceCreate()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = vhc.WorkspaceClose(ws.Workspace.WorkspaceID) })
	src.attachRelayURL = dead
	v := startViewer(t, src, vhc, ws.RootPane.PaneID, "e2e-ru-viewer")
	waitTick(t, v, "RU", 1, 40*time.Second, "設定の relay は死んでいるが settings/relay の relay で画面が出る")
	if !strings.Contains(v.log(), "relay="+src.relayURL) {
		t.Fatalf("attach が切替先を使った記録が無い:\n%s", v.log())
	}
	if !strings.Contains(src.agentLog.String(), "relay URL 切替: "+dead+" → "+src.relayURL) {
		t.Fatalf("agent が切替した記録が無い:\n%s", src.agentLog.String())
	}
}
