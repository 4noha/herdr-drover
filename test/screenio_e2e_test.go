//go:build !windows

package e2e

// 画面 I/O 命令（read-pane/send-input/interrupt・Phase 2）の実環境検証。
// 実 herdr 隔離サーバ ＋ 実 agent バイナリ ＋ 実 Firestore エミュレータで、
// commands/{pc}/q への投入 → CommandRunner 配線 → herdr API 実行 → Ack/
// mcp_results read-back までを一本で確認する。relay/bridge は不要（画面 I/O
// は Firestore 命令チャネルのみで完結＝CLOUD_RELAY_URL 未設定のまま）。
//
// 特に screenio.go で実コード未確認のまま採用した2つの仮定をここで実証する:
//   - herdr pane.send_keys が "C-c" を Ctrl-C として受理する（reconcile.go の
//     既存実測は "enter" のみ・"C-c" は本テストが初検証）
//   - pane.read の source="visible" が実テキストを返す
//
// 旧コード（CommandRunner に DoReadPane/DoSendInput/DoInterrupt 未配線）では
// 全命令が "xxx 未配線" で error Ack される＝本テストは FAIL することを
// 実装前に確認済み（鉄則2）。
import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/4noha/drover-cloud/state"
)

const screenioPCID = "e2e-screenio-herdr"

func TestE2EScreenIOCommands(t *testing.T) {
	if os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
		t.Skip("SKIP: gcloud / Java21+ 不在のため Firestore emulator 検証不可")
	}
	srv, hc := startHerdr(t)
	bin := buildBinary(t)
	ctx := context.Background()

	st, err := state.New(ctx, projectID, screenioPCID)
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	ws, err := hc.WorkspaceCreate()
	if err != nil {
		t.Fatalf("workspace.create: %v", err)
	}
	paneID := ws.RootPane.PaneID
	t.Cleanup(func() { _ = hc.WorkspaceClose(ws.Workspace.WorkspaceID) })

	agent := exec.Command(bin, "agent")
	agent.Env = []string{
		"HOME=" + t.TempDir(),
		"PATH=" + os.Getenv("PATH"),
		"GCP_PROJECT=" + projectID,
		"PC_ID=" + screenioPCID,
		"DROVER_TICK=500ms",
		"FIRESTORE_EMULATOR_HOST=" + os.Getenv("FIRESTORE_EMULATOR_HOST"),
		"HERDR_SOCKET_PATH=" + srv.sock,
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
	waitFor(t, 20*time.Second, "pcs/"+screenioPCID+" registered", func() (bool, error) {
		if dead() {
			return false, nil
		}
		pcs, err := st.ListPCs(ctx)
		if err != nil {
			return false, err
		}
		for _, p := range pcs {
			if p == screenioPCID {
				return true, nil
			}
		}
		return false, fmt.Errorf("pcs=%v", pcs)
	})

	push := func(cmd string, args map[string]string) string {
		id, err := st.PushCommandArgs(ctx, screenioPCID, cmd, paneID, "", "owner@example.com", "mcp", args)
		if err != nil {
			t.Fatalf("PushCommandArgs(%s): %v", cmd, err)
		}
		return id
	}
	waitAck := func(id string) state.Command {
		var c state.Command
		waitFor(t, 20*time.Second, "command "+id+" done/error", func() (bool, error) {
			if dead() {
				return false, nil
			}
			cs, err := st.RecentCommands(ctx, screenioPCID, 20)
			if err != nil {
				return false, err
			}
			for _, x := range cs {
				if x.ID == id && (x.Status == "done" || x.Status == "error") {
					c = x
					return true, nil
				}
			}
			return false, fmt.Errorf("まだ pending/running")
		})
		return c
	}
	future := func() string { return time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339Nano) }

	// --- send-input: テキスト入力＋submit（既定）。require_status=any で
	// bare shell（agent_status=unknown 想定）でも拒否されないようにする。
	marker1 := fmt.Sprintf("HDSCR_MARK_%d", time.Now().UnixNano())
	c := waitAck(push("send-input", map[string]string{
		"text": "echo " + marker1, "require_status": "any", "expires_at": future(),
	}))
	if c.Status != "done" {
		t.Fatalf("send-input(marker1) が done にならない: %+v\nstderr:\n%s", c, agentErr.String())
	}

	// --- read-pane: 直前の echo 出力が画面に見えること（source=visible 実証）。
	readID := push("read-pane", map[string]string{"source": "visible"})
	c = waitAck(readID)
	if c.Status != "done" {
		t.Fatalf("read-pane が done にならない: %+v\nstderr:\n%s", c, agentErr.String())
	}
	res, ok, err := st.GetCommandResult(ctx, readID)
	if err != nil || !ok {
		t.Fatalf("GetCommandResult(%s): ok=%v err=%v", readID, ok, err)
	}
	if !strings.Contains(res.Text, marker1) {
		t.Fatalf("read-pane の結果に marker1 が無い: %q", res.Text)
	}

	// --- interrupt: 実行中の長い sleep を Ctrl-C（"C-c"）で本当に止められる
	// ことの実証。sleep 中に別命令で echo を投げ、それが**待たずに**出力
	// されることで「interrupt が効いた」ことを確定させる（効かなければ
	// shell は sleep 終了までこの echo を実行しない＝タイムアウトで FAIL）。
	c = waitAck(push("send-input", map[string]string{
		"text": "sleep 30", "require_status": "any", "expires_at": future(),
	}))
	if c.Status != "done" {
		t.Fatalf("send-input(sleep 30) が done にならない: %+v", c)
	}
	time.Sleep(500 * time.Millisecond) // sleep がシェルの foreground job になるのを待つ

	c = waitAck(push("interrupt", map[string]string{"expires_at": future()}))
	if c.Status != "done" {
		t.Fatalf("interrupt が done にならない（\"C-c\" が herdr に拒否された可能性）: %+v\nstderr:\n%s", c, agentErr.String())
	}

	marker2 := fmt.Sprintf("HDSCR_AFTER_%d", time.Now().UnixNano())
	c = waitAck(push("send-input", map[string]string{
		"text": "echo " + marker2, "require_status": "any", "expires_at": future(),
	}))
	if c.Status != "done" {
		t.Fatalf("send-input(marker2) が done にならない: %+v", c)
	}
	readID2 := push("read-pane", map[string]string{"source": "visible"})
	c = waitAck(readID2)
	if c.Status != "done" {
		t.Fatalf("read-pane(2回目) が done にならない: %+v", c)
	}
	// 10秒以内に marker2 が見えること＝sleep 30 を待たずに次の echo が実行
	// された証拠（interrupt が sleep を実際に止めた）。本物の sleep 残存なら
	// ここで FAIL する（タイムアウトではなく「marker2 が無い」で落ちる）。
	res2, ok, err := st.GetCommandResult(ctx, readID2)
	if err != nil || !ok {
		t.Fatalf("GetCommandResult(%s): ok=%v err=%v", readID2, ok, err)
	}
	if !strings.Contains(res2.Text, marker2) {
		t.Fatalf("interrupt が sleep を止めていない（marker2 未着＝sleep 継続中の可能性）: %q\nstderr:\n%s",
			res2.Text, agentErr.String())
	}

	// --- 未知 sid は status=error（pending 滞留させない）。push ヘルパは
	// 常に paneID を使うため、ここだけ直接 PushCommandArgs で別 sid を渡す。
	idBadSID, err := st.PushCommandArgs(ctx, screenioPCID, "read-pane", "w9:p9", "", "owner@example.com", "mcp",
		map[string]string{"source": "visible"})
	if err != nil {
		t.Fatalf("PushCommandArgs(未知sid): %v", err)
	}
	c = waitAck(idBadSID)
	if c.Status != "error" {
		t.Fatalf("未知 sid の read-pane が error にならない: %+v", c)
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
