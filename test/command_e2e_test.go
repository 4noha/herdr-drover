//go:build !windows

// command_e2e_test — Phase 4「遠隔命令」の restart-proxy gate（常設・実環境）:
//
//	実 herdr 隔離サーバ ＋ ローカル実 relay（cm 無改変 build）＋ 実 Firestore
//	エミュレータ ＋ 実 agent バイナリ ＋ 機械 viewer で
//
//	  bridge 稼働中に commands/{pc}/q へ restart-proxy を書込
//	  → agent の CommandRunner が claim → webterm respawn（旧 bridge 停止
//	    → 新 bridge dial）→ Ack done 監査 read-back
//	  → 新 viewer の RESIZE で observe が**別 PID**で再 spawn（respawn の実証）
//	  → 未知 sid の restart-proxy は status=error で Ack（pending 滞留させない）
//
// を検証する。鉄則どおり合成 relay/合成 Firestore は使わない。
// 旧コード（CommandRunner 未配線の agent）では命令が pending のまま滞留し
// 「Ack done read-back」の waitFor で FAIL する（実装前に FAIL を実確認した）。
package e2e

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/4noha/herdr-drover/internal/cloud/state"
)

// cmdPCID は本ゲート専用の PC id（他 e2e と分離＝wake/doc/commands が
// 干渉しない）。-herdr サフィックスは DESIGN 決定事項。
const cmdPCID = "e2e-cmd-herdr"

// waitCmdFinal は commands/{pc}/q の命令 id が done|error に至るのを待って
// 返す（Ack 監査の read-back。cm agent/commands_test.go の waitCmdStatus と
// 同型）。
func waitCmdFinal(t *testing.T, st *state.Client, pc, id string, to time.Duration) state.Command {
	t.Helper()
	dl := time.Now().Add(to)
	for time.Now().Before(dl) {
		cs, _ := st.RecentCommands(context.Background(), pc, 20)
		for _, c := range cs {
			if c.ID == id && (c.Status == "done" || c.Status == "error") {
				return c
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("命令 %s が done/error に至らない（dispatch/Ack 不成立＝pending 滞留）", id)
	return state.Command{}
}

// observePID は observeChildren の行（`pid ppid command...`）から PID を返す。
func observePID(t *testing.T, line string) string {
	t.Helper()
	f := strings.Fields(line)
	if len(f) < 1 {
		t.Fatalf("observe 行が空: %q", line)
	}
	return f[0]
}

func TestE2ERestartProxyRespawnsBridge(t *testing.T) {
	if os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
		t.Skip("SKIP: gcloud / Java21+ 不在のため Firestore emulator 検証不可")
	}
	requireBridge(t)
	srv, hc := startHerdr(t)
	relayURL := startLocalRelay(t, cmRepoRoot(t))
	bin := buildBinary(t)

	// wake/命令/監査 read-back 用の in-process クライアント（owner 側の役）。
	st, err := state.New(context.Background(), projectID, "e2e-cmd-viewer")
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	// --- 実 agent 起動（webterm e2e と同じ隔離 env）---
	agent := exec.Command(bin, "agent")
	agent.Env = []string{
		"HOME=" + t.TempDir(),
		"PATH=" + os.Getenv("PATH"),
		"GCP_PROJECT=" + projectID,
		"PC_ID=" + cmdPCID,
		"DROVER_TICK=1s",
		"FIRESTORE_EMULATOR_HOST=" + os.Getenv("FIRESTORE_EMULATOR_HOST"),
		"HERDR_SOCKET_PATH=" + srv.sock,
		"CLOUD_RELAY_URL=" + relayURL,
	}
	var agentErr, agentOut syncBuf
	agent.Stderr = &agentErr
	agent.Stdout = &agentOut
	if err := agent.Start(); err != nil {
		t.Fatalf("agent start: %v", err)
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- agent.Wait() }()
	agentDead := false
	t.Cleanup(func() {
		if !agentDead {
			_ = agent.Process.Kill()
			<-waitCh
		}
	})
	dead := func() bool {
		select {
		case err := <-waitCh:
			agentDead = true
			t.Fatalf("agent が早期終了: %v\nstderr:\n%s\nstdout:\n%s", err, agentErr.String(), agentOut.String())
			return true
		default:
			return false
		}
	}
	waitFor(t, 20*time.Second, "pcs/"+cmdPCID+" registered", func() (bool, error) {
		if dead() {
			return false, nil
		}
		pcs, err := st.ListPCs(context.Background())
		if err != nil {
			return false, err
		}
		for _, p := range pcs {
			if p == cmdPCID {
				return true, nil
			}
		}
		return false, fmt.Errorf("pcs=%v", pcs)
	})

	// --- 対象 pane＋機械 viewer → RESIZE → wake → フレーム到達（bridge 稼働）---
	ws, err := hc.WorkspaceCreate()
	if err != nil {
		t.Fatalf("workspace.create: %v", err)
	}
	paneID := ws.RootPane.PaneID
	t.Cleanup(func() { _ = hc.WorkspaceClose(ws.Workspace.WorkspaceID) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	viewer1, recv1, _ := dialViewer(t, ctx, relayURL, paneID)
	if _, err := viewer1.Write(resizeFrame(40, 120)); err != nil {
		t.Fatalf("viewer1 RESIZE write: %v", err)
	}
	if err := st.Wake(ctx, cmdPCID, paneID); err != nil {
		t.Fatalf("Wake: %v", err)
	}
	waitFor(t, 30*time.Second, "observe frame が viewer1 に届く（\\x1b[?2026h）", func() (bool, error) {
		if dead() {
			return false, nil
		}
		return bytes.Contains(recv1(), []byte("\x1b[?2026h")), fmt.Errorf(
			"received=%d bytes; agent stderr:\n%s", len(recv1()), agentErr.String())
	})

	// respawn 前の observe PID（agent 直下の exact 親子照合）。
	kids1 := observeChildren(t, agent.Process.Pid)
	if len(kids1) != 1 {
		t.Fatalf("observe subprocess が agent 直下に %d 個（期待 1）: %v", len(kids1), kids1)
	}
	oldPID := observePID(t, kids1[0])

	// --- 遠隔命令: restart-proxy（sid=pane_id）→ claim → respawn → Ack done ---
	id1, err := st.PushCommand(ctx, cmdPCID, "restart-proxy", paneID, "owner@example.com")
	if err != nil {
		t.Fatalf("PushCommand(restart-proxy): %v", err)
	}
	c1 := waitCmdFinal(t, st, cmdPCID, id1, 30*time.Second)
	if c1.Status != "done" || !strings.Contains(c1.Detail, "respawn") {
		t.Fatalf("restart-proxy の Ack が想定外: %+v\nstderr:\n%s", c1, agentErr.String())
	}

	// --- respawn の物証①: bridge 開始ログが 2 回（初回＋respawn 後の再接続）。
	// 旧 viewer1 の切断有無は relay 側の同 sid 再接続 timing（旧 source 死の
	// 掃除 vs 新 source takeover のどちらが先か）に依存して変わるので
	// assert しない（cm relay takeover 修正の semantics。新 viewer の
	// takeover で必ず収束する）。
	startLog := fmt.Sprintf("bridge 開始 sid=%q", paneID)
	waitFor(t, 20*time.Second, "bridge 開始ログ 2 回（respawn）", func() (bool, error) {
		if dead() {
			return false, nil
		}
		n := strings.Count(agentErr.String(), startLog)
		return n == 2, fmt.Errorf("bridge 開始 %d 回\nstderr:\n%s", n, agentErr.String())
	})

	// --- respawn の物証②: 新 viewer の RESIZE で observe が**別 PID**で
	// 再 spawn し、フレームが届く（同 sid の viewer takeover 込み）。
	viewer2, recv2, _ := dialViewer(t, ctx, relayURL, paneID)
	if _, err := viewer2.Write(resizeFrame(40, 120)); err != nil {
		t.Fatalf("viewer2 RESIZE write: %v", err)
	}
	waitFor(t, 30*time.Second, "respawn 後のフレームが viewer2 に届く", func() (bool, error) {
		if dead() {
			return false, nil
		}
		return bytes.Contains(recv2(), []byte("\x1b[?2026h")), fmt.Errorf(
			"received=%d bytes; stderr:\n%s", len(recv2()), agentErr.String())
	})
	waitFor(t, 10*time.Second, "observe が別 PID で 1 個", func() (bool, error) {
		if dead() {
			return false, nil
		}
		kids := observeChildren(t, agent.Process.Pid)
		if len(kids) != 1 {
			return false, fmt.Errorf("observe %d 個: %v", len(kids), kids)
		}
		return observePID(t, kids[0]) != oldPID, fmt.Errorf("PID が変わらない（旧 %s のまま）: %v", oldPID, kids)
	})

	// データ線が本物である物証: echo marker 往復。
	marker := fmt.Sprintf("HDCMD_MARK_%d", time.Now().UnixNano())
	if err := hc.PaneSendText(paneID, "echo "+marker+"\r"); err != nil {
		t.Fatalf("pane.send_text: %v", err)
	}
	waitFor(t, 20*time.Second, "respawn 後の bridge を通って marker が届く", func() (bool, error) {
		if dead() {
			return false, nil
		}
		return strings.Contains(stripANSI(recv2()), marker), fmt.Errorf("marker 未着")
	})

	// --- 未知 sid: 稼働 bridge が無い sid は status=error で Ack（滞留させ
	// ない）。稼働中 bridge には影響しない（bridge 開始ログは 2 回のまま）。
	id2, err := st.PushCommand(ctx, cmdPCID, "restart-proxy", "w9:p9", "owner@example.com")
	if err != nil {
		t.Fatalf("PushCommand(未知 sid): %v", err)
	}
	c2 := waitCmdFinal(t, st, cmdPCID, id2, 30*time.Second)
	if c2.Status != "error" || !strings.Contains(c2.Detail, "稼働 bridge が無い") {
		t.Fatalf("未知 sid の Ack が想定外（error＋理由明示のはず）: %+v", c2)
	}
	if n := strings.Count(agentErr.String(), startLog); n != 2 {
		t.Fatalf("未知 sid の respawn で稼働 bridge が巻き添え（bridge 開始 %d 回）\nstderr:\n%s", n, agentErr.String())
	}

	// --- SIGTERM graceful ---
	if err := agent.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	select {
	case err := <-waitCh:
		agentDead = true
		if err != nil {
			t.Fatalf("SIGTERM で exit 0 にならない: %v\nstderr:\n%s", err, agentErr.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("SIGTERM 後 15s 経っても agent が終了しない\nstderr:\n%s", agentErr.String())
	}
	t.Logf("agent stderr:\n%s", agentErr.String())
}
