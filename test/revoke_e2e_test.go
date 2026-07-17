//go:build !windows

package e2e

// 失効（ペアリング解除）時のドーマント検証（cm parity・レビュー指摘の再発
// 防止）。cm の runOneCloud は起動時 IsSelfRevoked なら RegisterPCVersion を
// スキップし、tick 冒頭でも IsSelfRevoked で push/delete を止める。drover が
// これを欠くと、owner が Web「端末ペアリング解除」（SetRevoked＋
// DeletePCByID）をしても稼働中 agent の次 tick が session doc と pcs 親 doc
// を再作成し続け、管理 UI 対 agent の削除合戦になる。
// 本テストは旧コード（失効無検査）で FAIL することを確認済み（鉄則2）。

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

// rvkPCID は本テスト専用の PC id（TestE2EAgentLifecycle の pcID と分離し、
// revoked/{pc} の状態がテスト間で漏れないようにする）。
const rvkPCID = "e2e-rvk-herdr"

// pcListed は pcs/* に pc が居るか。
func pcListed(t *testing.T, st *state.Client, pc string) bool {
	t.Helper()
	pcs, err := st.ListPCs(context.Background())
	if err != nil {
		t.Fatalf("ListPCs: %v", err)
	}
	for _, p := range pcs {
		if p == pc {
			return true
		}
	}
	return false
}

func TestE2ERevokedAgentDormant(t *testing.T) {
	if os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
		t.Skip("SKIP: gcloud / Java21+ 不在のため Firestore emulator 検証不可")
	}
	srv, hc := startHerdr(t)
	bin := buildBinary(t)
	ctx := context.Background()

	st, err := state.New(ctx, projectID, rvkPCID)
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	t.Cleanup(func() {
		// 後続テストへ revoked 状態を漏らさない
		_ = st.ClearRevoked(ctx, rvkPCID)
		_ = st.DeletePCByID(ctx, rvkPCID)
		st.Close()
	})

	// 起動前に失効させておく＝「解除済み端末の agent が launchd KeepAlive で
	// 再起動してくる」現実の状況。
	if err := st.SetRevoked(ctx, rvkPCID); err != nil {
		t.Fatalf("SetRevoked: %v", err)
	}

	// producer に push させる材料の pane を 1 つ
	ws, err := hc.WorkspaceCreate()
	if err != nil {
		t.Fatalf("workspace.create: %v", err)
	}
	paneID := ws.RootPane.PaneID

	tmpHome := t.TempDir()
	env := []string{
		"HOME=" + tmpHome,
		"PATH=" + os.Getenv("PATH"),
		"GCP_PROJECT=" + projectID,
		"PC_ID=" + rvkPCID,
		"DROVER_TICK=500ms",
		"FIRESTORE_EMULATOR_HOST=" + os.Getenv("FIRESTORE_EMULATOR_HOST"),
		"HERDR_SOCKET_PATH=" + srv.sock,
	}
	agent := exec.Command(bin, "agent")
	agent.Env = env
	var stderr bytes.Buffer
	agent.Stderr = &stderr
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
			t.Fatalf("agent が早期終了: %v\nstderr:\n%s", err, stderr.String())
			return true
		default:
			return false
		}
	}

	// --- 起動時 dormant: RegisterPC も push もしない（tick=500ms を 4s 観測
	// ＝8 tick 分）。旧コードは無条件 RegisterPCVersion で 1 tick 以内に
	// pcs/{pc} が出現し、ここで FAIL する。 ---
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if dead() {
			return
		}
		if pcListed(t, st, rvkPCID) {
			t.Fatalf("失効中に pcs/%s が登録された（起動時 dormant 不成立）\nstderr:\n%s", rvkPCID, stderr.String())
		}
		if d := sessionDoc(t, st, rvkPCID, paneID); d != nil {
			t.Fatalf("失効中に session doc が作られた: %v\nstderr:\n%s", d, stderr.String())
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !strings.Contains(stderr.String(), "dormant") {
		t.Fatalf("dormant 移行のログが無い（失効を検出していない疑い）:\n%s", stderr.String())
	}

	// --- ClearRevoked → 次 tick から自然復帰（owner の再 enroll 相当）---
	if err := st.ClearRevoked(ctx, rvkPCID); err != nil {
		t.Fatalf("ClearRevoked: %v", err)
	}
	waitFor(t, 15*time.Second, "doc appears after ClearRevoked", func() (bool, error) {
		if dead() {
			return false, nil
		}
		return sessionDoc(t, st, rvkPCID, paneID) != nil, fmt.Errorf("doc not yet")
	})

	// --- 稼働中の失効＋端末削除（Web「端末ペアリング解除」の実手順）---
	if err := st.SetRevoked(ctx, rvkPCID); err != nil {
		t.Fatalf("SetRevoked(2): %v", err)
	}
	// 失効が tick に観測されるのを待ってから消す（SetRevoked 直前に走り
	// 出した in-flight tick の push と DeletePCByID が交差する境界 race を
	// テストから排除する。実運用でも Web 側は revoke→削除の順で書く）。
	time.Sleep(1500 * time.Millisecond)
	if err := st.DeletePCByID(ctx, rvkPCID); err != nil {
		t.Fatalf("DeletePCByID: %v", err)
	}
	// 旧コード: 次 tick の PushStatus が doc 不在→changed→Set で再作成し、
	// 消したはずの端末が一覧に蘇る（削除合戦）＝ここで FAIL する。
	deadline = time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if dead() {
			return
		}
		if d := sessionDoc(t, st, rvkPCID, paneID); d != nil {
			t.Fatalf("ペアリング解除後に session doc が再作成された（削除合戦）: %v", d)
		}
		if pcListed(t, st, rvkPCID) {
			t.Fatalf("ペアリング解除後に pcs/%s が再作成された（削除合戦）", rvkPCID)
		}
		time.Sleep(200 * time.Millisecond)
	}

	// --- graceful 終了は失効中でも保たれる ---
	if err := agent.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	select {
	case err := <-waitCh:
		agentDead = true
		if err != nil {
			t.Fatalf("SIGTERM で exit 0 にならない: %v\nstderr:\n%s", err, stderr.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("SIGTERM 後 15s 経っても agent が終了しない\nstderr:\n%s", stderr.String())
	}
}
