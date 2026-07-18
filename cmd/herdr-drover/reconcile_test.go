//go:build unix

package main

// reconcile（リモート pane 注入）の検証。**herdr 側は実 herdr 隔離サーバ**で
// pane の生成/list-by-metadata/close/dedup/冪等/fail-safe を機械検証し、リモート
// データ（他 PC のセッション行）だけ fake remoteSource で注入する（合成でなく実
// キーパス＝リスクの本体である herdr 挙動は実物で担保。Firestore 統合は state/
// producer の emulator テストが別途担保）。attach の relay 接続は別（要 2 クラウド）。

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/4noha/herdr-drover/internal/herdrapi"
	"github.com/4noha/herdr-drover/internal/wsmap"
)

type fakeRemote struct {
	pcs      []string
	sessions map[string][]map[string]any
	pcsErr   error
	sessErr  error
}

func (f *fakeRemote) DroverPCs(context.Context) ([]string, error) { return f.pcs, f.pcsErr }
func (f *fakeRemote) ListSessions(_ context.Context, pc string) ([]map[string]any, error) {
	return f.sessions[pc], f.sessErr
}

func fakeSess(sid, dir string) map[string]any {
	return map[string]any{"key": sid, "session_id": sid, "short_dir": dir}
}

// reconcileStub は注入 pane が実行する無害な stub（argv 無視で生存＝pane が
// 消えないので出現/消滅を安定に観測できる。実 attach の接続は別テスト）。
func reconcileStub(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "stub")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nsleep 300\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func injectedPanes(t *testing.T, api *herdrapi.Client) map[string][2]string {
	t.Helper()
	panes, err := api.PaneList()
	if err != nil {
		t.Fatalf("pane.list: %v", err)
	}
	out := map[string][2]string{}
	for i := range panes {
		p := &panes[i]
		if pc, sid := p.Tokens[injTokPC], p.Tokens[injTokSID]; pc != "" && sid != "" {
			out[p.PaneID] = [2]string{pc, sid}
		}
	}
	return out
}

func hasInj(m map[string][2]string, pc, sid string) bool {
	for _, v := range m {
		if v[0] == pc && v[1] == sid {
			return true
		}
	}
	return false
}

func TestReconcileRemoteInjectAndSelfHeal(t *testing.T) {
	sock := startHerdrForTest(t)
	api := herdrapi.New(sock)
	lg := log.New(io.Discard, "", 0)
	stub := reconcileStub(t)
	ctx := context.Background()

	fr := &fakeRemote{
		pcs: []string{"self-herdr", "remoteA"},
		sessions: map[string][]map[string]any{
			"remoteA": {fakeSess("w9:pA", "projA"), fakeSess("w9:pB", "projB")},
		},
	}
	const selfPC = "self-herdr"

	// 1 周目: 他 PC(remoteA) の 2 セッションが注入 pane として出現（自 PC は除外）。
	reconcileRemote(ctx, api, fr, Cloud{PCName: selfPC}, stub, lg)
	waitCond(t, 15*time.Second, "他 PC の 2 セッションが注入 pane として出現", func() bool {
		inj := injectedPanes(t, api)
		return len(inj) == 2 && hasInj(inj, "remoteA", "w9:pA") && hasInj(inj, "remoteA", "w9:pB")
	})

	// 2 周目: 同一 state → 冪等（定常 CREATE=0＝pane 数不変・M8f2 教訓の機械確認）。
	before := len(injectedPanes(t, api))
	reconcileRemote(ctx, api, fr, Cloud{PCName: selfPC}, stub, lg)
	time.Sleep(700 * time.Millisecond)
	if got := len(injectedPanes(t, api)); got != before {
		t.Fatalf("冪等でない（2 周目で注入 pane 数が %d→%d）", before, got)
	}

	// remoteA の 1 本消滅 → その注入 pane だけ close（もう 1 本は維持）。
	fr.sessions["remoteA"] = []map[string]any{fakeSess("w9:pA", "projA")}
	reconcileRemote(ctx, api, fr, Cloud{PCName: selfPC}, stub, lg)
	waitCond(t, 15*time.Second, "消滅セッションの注入 pane が close・残りは維持", func() bool {
		inj := injectedPanes(t, api)
		return len(inj) == 1 && hasInj(inj, "remoteA", "w9:pA") && !hasInj(inj, "remoteA", "w9:pB")
	})

	// 全消滅 → 注入 pane ゼロ。
	fr.sessions["remoteA"] = nil
	reconcileRemote(ctx, api, fr, Cloud{PCName: selfPC}, stub, lg)
	waitCond(t, 15*time.Second, "全リモートセッション消滅で注入 pane ゼロ", func() bool {
		return len(injectedPanes(t, api)) == 0
	})
}

// TestReconcileDoesNotReapTokenlessInjectWorkspacePanes は「注入 workspace 内の
// token 無し pane を reconcile が掃除してはならない」不変条件の見張り。注入 workspace
// には WorkspaceCreate 由来の**構造 root pane（token 無し）**が常駐する（実 herdr 0.7.4
// で実測）。「token 無し＝孤児」で一括掃除すると root pane を毎周 kill する退行になる
// （敵対的再レビューで危うく導入しかけた穴）。再起動で token を失った pane は attach の
// 自己再表明で治すのが設計＝reconcile は token 無し pane に手を出さない。
func TestReconcileDoesNotReapTokenlessInjectWorkspacePanes(t *testing.T) {
	sock := startHerdrForTest(t)
	api := herdrapi.New(sock)
	lg := log.New(io.Discard, "", 0)
	stub := reconcileStub(t)
	ctx := context.Background()

	// 注入 workspace に token 無し pane を置く（構造 root pane / 再起動で token を
	// 失った復元 pane を模す）。
	wsID, err := wsmap.ResolveWorkspaceID(api, injWorkspace)
	if err != nil {
		t.Fatalf("resolve inject ws: %v", err)
	}
	tokenless, err := applyInjectPane(api, wsID, injTabName("rootlike"), []string{stub}, nil)
	if err != nil {
		t.Fatalf("token 無し pane 生成: %v", err)
	}

	// desired 空で reconcile → token 無し pane は kill してはならない。
	fr := &fakeRemote{pcs: []string{"remoteA"}, sessions: map[string][]map[string]any{}}
	reconcileRemote(ctx, api, fr, Cloud{PCName: "self"}, stub, lg)
	time.Sleep(700 * time.Millisecond)

	panes, err := api.PaneList()
	if err != nil {
		t.Fatalf("pane.list: %v", err)
	}
	for i := range panes {
		if panes[i].PaneID == tokenless {
			return // 生存＝OK
		}
	}
	t.Fatalf("token 無し pane %s が掃除された（注入 ws の構造 root pane を殺す退行）", tokenless)
}

// ListPCs 失敗周は既存注入 pane を kill しない（fail-safe＝desired 空誤認で全 kill
// する破壊を防ぐ。旧来の「list 失敗＝ゼロ誤認」runaway の逆＝破壊の防止）。
func TestReconcileRemoteAbortKeepsPanesOnError(t *testing.T) {
	sock := startHerdrForTest(t)
	api := herdrapi.New(sock)
	lg := log.New(io.Discard, "", 0)
	stub := reconcileStub(t)
	ctx := context.Background()

	fr := &fakeRemote{
		pcs:      []string{"remoteA"},
		sessions: map[string][]map[string]any{"remoteA": {fakeSess("w9:pA", "projA")}},
	}
	reconcileRemote(ctx, api, fr, Cloud{PCName: "self"}, stub, lg)
	waitCond(t, 15*time.Second, "注入 pane 出現", func() bool { return len(injectedPanes(t, api)) == 1 })

	fr.pcsErr = fmt.Errorf("firestore down")
	reconcileRemote(ctx, api, fr, Cloud{PCName: "self"}, stub, lg)
	time.Sleep(700 * time.Millisecond)
	if n := len(injectedPanes(t, api)); n != 1 {
		t.Fatalf("ListPCs エラー周に注入 pane が %d になった（fail-safe 違反＝kill してはならない）", n)
	}
}
