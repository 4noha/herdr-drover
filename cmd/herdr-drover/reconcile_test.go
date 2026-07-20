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
	"github.com/4noha/herdr-drover/internal/injectindex"
	"github.com/4noha/herdr-drover/internal/wsmap"
)

// newTestIndex はテスト用の一時 injectindex（TempDir 上・test 終了で消える）。
// reconcile_test の全ケースが独立した index を持つ（テスト間の状態漏れ回避）。
func newTestIndex(t *testing.T) *injectindex.Index {
	t.Helper()
	path := filepath.Join(t.TempDir(), "inject-index.json")
	idx, err := injectindex.Open(path)
	if err != nil {
		t.Fatalf("injectindex.Open: %v", err)
	}
	return idx
}

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
	reconcileRemote(ctx, api, fr, Cloud{PCName: selfPC}, stub, newTestIndex(t), lg)
	waitCond(t, 15*time.Second, "他 PC の 2 セッションが注入 pane として出現", func() bool {
		inj := injectedPanes(t, api)
		return len(inj) == 2 && hasInj(inj, "remoteA", "w9:pA") && hasInj(inj, "remoteA", "w9:pB")
	})

	// 2 周目: 同一 state → 冪等（定常 CREATE=0＝pane 数不変・M8f2 教訓の機械確認）。
	before := len(injectedPanes(t, api))
	reconcileRemote(ctx, api, fr, Cloud{PCName: selfPC}, stub, newTestIndex(t), lg)
	time.Sleep(700 * time.Millisecond)
	if got := len(injectedPanes(t, api)); got != before {
		t.Fatalf("冪等でない（2 周目で注入 pane 数が %d→%d）", before, got)
	}

	// remoteA の 1 本消滅 → その注入 pane だけ close（もう 1 本は維持）。
	fr.sessions["remoteA"] = []map[string]any{fakeSess("w9:pA", "projA")}
	reconcileRemote(ctx, api, fr, Cloud{PCName: selfPC}, stub, newTestIndex(t), lg)
	waitCond(t, 15*time.Second, "消滅セッションの注入 pane が close・残りは維持", func() bool {
		inj := injectedPanes(t, api)
		return len(inj) == 1 && hasInj(inj, "remoteA", "w9:pA") && !hasInj(inj, "remoteA", "w9:pB")
	})

	// 全消滅 → 注入 pane ゼロ。
	fr.sessions["remoteA"] = nil
	reconcileRemote(ctx, api, fr, Cloud{PCName: selfPC}, stub, newTestIndex(t), lg)
	waitCond(t, 15*time.Second, "全リモートセッション消滅で注入 pane ゼロ", func() bool {
		return len(injectedPanes(t, api)) == 0
	})
}

// fakeSessAgent は agent_status / window_name 付きの session 行（producer が同期
// する生値の部分集合）。agent を持つリモート pane の転記経路を検証するのに使う。
func fakeSessAgent(sid, dir, name, status string) map[string]any {
	return map[string]any{
		"key":          sid,
		"session_id":   sid,
		"short_dir":    dir,
		"window_name":  name,
		"agent_status": status,
	}
}

// injPaneStatus は (pc,sid) の注入 pane の agent_status を実 herdr の pane.list から
// 読む（report_agent が pane.agent_status に反映されることの検証点）。
func injPaneStatus(t *testing.T, api *herdrapi.Client, pc, sid string) (string, bool) {
	t.Helper()
	panes, err := api.PaneList()
	if err != nil {
		t.Fatalf("pane.list: %v", err)
	}
	for i := range panes {
		p := &panes[i]
		if p.Tokens[injTokPC] == pc && p.Tokens[injTokSID] == sid {
			return p.AgentStatus, true
		}
	}
	return "", false
}

// TestReconcileMirrorsRemoteAgentStatus は「リモート session の agent_status を注入
// pane へ転記して herdr に agent 検出させる」機能の検証（実 herdr）。pane.report_agent
// が pane.agent_status に効くこと、working↔idle↔blocked の追随、リモート agent 終了
// （unknown）での release_agent による stale 解消を機械確認する。
func TestReconcileMirrorsRemoteAgentStatus(t *testing.T) {
	sock := startHerdrForTest(t)
	api := herdrapi.New(sock)
	lg := log.New(io.Discard, "", 0)
	stub := reconcileStub(t)
	ctx := context.Background()
	idx := newTestIndex(t)
	reported := map[string]string{} // release 追跡（runRemoteInject 相当）

	fr := &fakeRemote{
		pcs: []string{"self-herdr", "remoteA"},
		sessions: map[string][]map[string]any{
			"remoteA": {fakeSessAgent("w9:pA", "projA", "claude", "working")},
		},
	}
	const selfPC = "self-herdr"

	// step はリモート agent_status を status に変えて 1 周 reconcile し、注入 pane の
	// herdr 表示 agent_status が want になるまで待つ。want が status と違うのは herdr の
	// seen 意味論による: report_agent の --state は idle/working/blocked/unknown の 4 値で、
	// 内部 AgentState::Idle は「未 seen」で "done"、"seen" で "idle" と表示される
	// （pane_agent_status(state, seen)）。本テストは pane を view しない＝seen=false なので
	// リモートの idle も done も転記先では "done" と出る（状態としては同一の Idle）。
	step := func(status, want string) {
		t.Helper()
		fr.sessions["remoteA"] = []map[string]any{fakeSessAgent("w9:pA", "projA", "claude", status)}
		reconcileRemote(ctx, api, fr, Cloud{PCName: selfPC}, stub, idx, lg, reported)
		waitCond(t, 15*time.Second, fmt.Sprintf("report=%q → 注入 pane の agent_status が %q になる", status, want), func() bool {
			s, ok := injPaneStatus(t, api, "remoteA", "w9:pA")
			return ok && s == want
		})
	}

	// CREATE 周: working が注入 pane に転記される。
	step("working", "working")
	// 既存 pane で done → blocked → idle → working と追随（done/idle は未 seen で "done" 表示）。
	step("done", "done")
	step("blocked", "blocked")
	step("idle", "done")
	step("working", "working")
	// リモート agent 終了（unknown）→ release_agent で stale が消え unknown に戻る
	// （session 自体は残すので pane は close されない＝release 経路のみを分離検証）。
	step("unknown", "unknown")
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
	reconcileRemote(ctx, api, fr, Cloud{PCName: "self"}, stub, newTestIndex(t), lg)
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
	reconcileRemote(ctx, api, fr, Cloud{PCName: "self"}, stub, newTestIndex(t), lg)
	waitCond(t, 15*time.Second, "注入 pane 出現", func() bool { return len(injectedPanes(t, api)) == 1 })

	fr.pcsErr = fmt.Errorf("firestore down")
	reconcileRemote(ctx, api, fr, Cloud{PCName: "self"}, stub, newTestIndex(t), lg)
	time.Sleep(700 * time.Millisecond)
	if n := len(injectedPanes(t, api)); n != 1 {
		t.Fatalf("ListPCs エラー周に注入 pane が %d になった（fail-safe 違反＝kill してはならない）", n)
	}
}

// TestReconcileMoveTabToOtherWorkspace は「注入 pane を別 workspace へ mv-tab で
// 動かしても reconcile が冪等」の見張り。判定の権威が workspace label / workspace_id
// から token+injectindex に移った不変条件を機械確認する。
// 旧コード（label 権威）は「動かされた pane を cur で認識できず二重作成」で FAIL する。
func TestReconcileMoveTabToOtherWorkspace(t *testing.T) {
	sock := startHerdrForTest(t)
	api := herdrapi.New(sock)
	lg := log.New(io.Discard, "", 0)
	stub := reconcileStub(t)
	ctx := context.Background()
	idx := newTestIndex(t)

	fr := &fakeRemote{
		pcs:      []string{"remoteA"},
		sessions: map[string][]map[string]any{"remoteA": {fakeSess("w9:pM", "projMove")}},
	}
	// 1 周目: 注入 pane 出現
	reconcileRemote(ctx, api, fr, Cloud{PCName: "self"}, stub, idx, lg)
	waitCond(t, 15*time.Second, "注入 pane 出現", func() bool { return len(injectedPanes(t, api)) == 1 })

	// 注入 pane を別 workspace（label 自由）へ pane.move で動かす。判定が label 依存
	// なら以降 reconcile は cur で認識できず二重作成に走る（旧コードの実バグ）。
	inj := injectedPanes(t, api)
	var movedPane string
	for pid := range inj {
		movedPane = pid
		break
	}
	if movedPane == "" {
		t.Fatal("注入 pane が見つからない")
	}
	// 別 workspace を新規作成し（label は任意・↗ prefix 無し）、pane.move new_workspace で移動。
	created, err := api.WorkspaceCreate()
	if err != nil {
		t.Fatalf("workspace.create: %v", err)
	}
	res, err := paneMoveNewTab(api, movedPane, created.Workspace.WorkspaceID, "moved")
	if err != nil {
		t.Fatalf("pane.move new_tab: %v", err)
	}
	// pane.move で pane_id が変わる（実 herdr 挙動）→ index も追随できないと cur から漏れる。
	// ただし本 reconcile は「pane.list に token あり」なので新 pane_id でも cur に載る。
	// AdoptToken で index が新 pane_id を取り込む挙動を保証する。
	newPane := res.Pane.PaneID
	if newPane == "" {
		t.Fatal("pane.move 応答に新 pane_id が無い")
	}

	// 2 周目: 動かされた注入 pane を cur で認識できるか。冪等 = 二重作成しない。
	reconcileRemote(ctx, api, fr, Cloud{PCName: "self"}, stub, idx, lg)
	time.Sleep(700 * time.Millisecond)
	inj2 := injectedPanes(t, api)
	if len(inj2) != 1 {
		t.Fatalf("mv-tab 後に注入 pane が %d 個（冪等違反＝二重作成）: %v", len(inj2), inj2)
	}
	if !hasInj(inj2, "remoteA", "w9:pM") {
		t.Fatalf("mv-tab 後に元の (pc,sid) が cur から漏れた: %v", inj2)
	}
	// index にも新 pane_id が反映されている（AdoptToken 経路）。
	if e, ok := idx.Get(newPane); !ok || e.PC != "remoteA" || e.SID != "w9:pM" {
		t.Fatalf("index に新 pane_id %s が取り込まれていない: entry=%+v ok=%v", newPane, e, ok)
	}
}

// TestReconcileTokenAuthorityAcrossRename は workspace を rename しても
// reconcile が冪等（label 依存が抜けている）ことの機械確認。ユーザーが herdr UI で
// ↗remote を mac-studio などに rename した場合の rename 耐性。
func TestReconcileTokenAuthorityAcrossRename(t *testing.T) {
	sock := startHerdrForTest(t)
	api := herdrapi.New(sock)
	lg := log.New(io.Discard, "", 0)
	stub := reconcileStub(t)
	ctx := context.Background()
	idx := newTestIndex(t)

	fr := &fakeRemote{
		pcs:      []string{"remoteA"},
		sessions: map[string][]map[string]any{"remoteA": {fakeSess("w9:pR", "projR")}},
	}
	reconcileRemote(ctx, api, fr, Cloud{PCName: "self"}, stub, idx, lg)
	waitCond(t, 15*time.Second, "注入 pane 出現", func() bool { return len(injectedPanes(t, api)) == 1 })

	// 注入 workspace の workspace_id を特定 → rename する。
	inj := injectedPanes(t, api)
	var injPaneID string
	for pid := range inj {
		injPaneID = pid
		break
	}
	pInfo, err := api.PaneGet(injPaneID)
	if err != nil {
		t.Fatalf("pane.get: %v", err)
	}
	injWSID := pInfo.WorkspaceID
	// 別 label へ rename（"mac-studio" などユーザー任意）。
	if _, err := api.Call("workspace.rename", struct {
		WorkspaceID string `json:"workspace_id"`
		Label       string `json:"label"`
	}{injWSID, "mac-studio"}); err != nil {
		t.Fatalf("workspace.rename: %v", err)
	}

	// 2 周目: rename 後も cur に載って冪等（旧 label 依存なら cur=0 で二重作成に走る）。
	reconcileRemote(ctx, api, fr, Cloud{PCName: "self"}, stub, idx, lg)
	time.Sleep(700 * time.Millisecond)
	inj2 := injectedPanes(t, api)
	if len(inj2) != 1 || !hasInj(inj2, "remoteA", "w9:pR") {
		t.Fatalf("rename 後に冪等違反（label 依存が残っている）: %v", inj2)
	}
}

// TestSelfHealAdoptsTokenPane は起動時 (a) 分岐: pane.list に token 付き pane が
// 居るが index に無い → AdoptToken で index に取り込む挙動の見張り。
// attach プロセスの自己再表明で先に token が付いた状態（drover 単独再起動）を再現。
func TestSelfHealAdoptsTokenPane(t *testing.T) {
	sock := startHerdrForTest(t)
	api := herdrapi.New(sock)
	lg := log.New(io.Discard, "", 0)
	stub := reconcileStub(t)
	idx := newTestIndex(t)

	// 注入 pane を「index に載せずに」作る（attach 自己再表明が先に走った状態を模す）。
	wsID, err := wsmap.ResolveWorkspaceID(api, injWorkspace)
	if err != nil {
		t.Fatalf("resolve inject ws: %v", err)
	}
	pid, err := applyInjectPane(api, wsID, injTabName("adopted"), []string{stub}, nil)
	if err != nil {
		t.Fatalf("applyInjectPane: %v", err)
	}
	if err := api.PaneReportMetadata(pid, injSource, herdrapi.ReportMetadata{
		Tokens: map[string]string{injTokPC: "remoteA", injTokSID: "w9:pAdopt"},
	}); err != nil {
		t.Fatalf("token 付与: %v", err)
	}

	// self-heal 実行 → index に取り込まれる。
	panes, err := api.PaneList()
	if err != nil {
		t.Fatalf("pane.list: %v", err)
	}
	_, adopted, _ := selfHealOnStartup(api, idx, panes, lg)
	if adopted != 1 {
		t.Fatalf("adopted=%d want 1", adopted)
	}
	e, ok := idx.Get(pid)
	if !ok || e.PC != "remoteA" || e.SID != "w9:pAdopt" || e.Pending {
		t.Fatalf("index への Adopt が不完全: entry=%+v ok=%v", e, ok)
	}
}

// TestSelfHealRestoresLostTokens は起動時 (b) 分岐: pane.list に token 無し pane が
// 居るが index には entry あり → token 再表明で復元する挙動の見張り。
// herdr サーバ単独再起動で token が落ちた pane を drover 単独 self-heal で復元する経路。
func TestSelfHealRestoresLostTokens(t *testing.T) {
	sock := startHerdrForTest(t)
	api := herdrapi.New(sock)
	lg := log.New(io.Discard, "", 0)
	stub := reconcileStub(t)
	idx := newTestIndex(t)

	// 注入 pane を作る（token 無し状態＝herdr 再起動で消失した状態を模す）。
	wsID, err := wsmap.ResolveWorkspaceID(api, injWorkspace)
	if err != nil {
		t.Fatalf("resolve inject ws: %v", err)
	}
	pid, err := applyInjectPane(api, wsID, injTabName("lost"), []string{stub}, nil)
	if err != nil {
		t.Fatalf("applyInjectPane: %v", err)
	}
	// index には Live entry を入れる（reconcile が過去に Commit した状態）。
	if err := idx.Commit(pid, "remoteA", "w9:pLost"); err != nil {
		t.Fatalf("idx.Commit: %v", err)
	}
	// pane 側は token 無し（実 pane.list で確認）。
	p, err := api.PaneGet(pid)
	if err != nil {
		t.Fatalf("pane.get: %v", err)
	}
	if p.Tokens[injTokPC] != "" || p.Tokens[injTokSID] != "" {
		t.Fatalf("前提: pane に token 無しであるべきだが %v が残っている", p.Tokens)
	}

	// self-heal 実行 → token が復元される。
	panes, err := api.PaneList()
	if err != nil {
		t.Fatalf("pane.list: %v", err)
	}
	healed, _, _ := selfHealOnStartup(api, idx, panes, lg)
	if healed != 1 {
		t.Fatalf("healed=%d want 1", healed)
	}
	p2, err := api.PaneGet(pid)
	if err != nil {
		t.Fatalf("pane.get(after heal): %v", err)
	}
	if p2.Tokens[injTokPC] != "remoteA" || p2.Tokens[injTokSID] != "w9:pLost" {
		t.Fatalf("self-heal で token が復元されていない: %v", p2.Tokens)
	}
}

// TestSelfHealDropsStaleIndexEntry は起動時 (c) 分岐: index に entry あるが pane.list
// に該当 pane_id 無し → Forget（stale 掃除）の見張り。drover 停止中に close された
// 注入 pane の残骸削除。
func TestSelfHealDropsStaleIndexEntry(t *testing.T) {
	sock := startHerdrForTest(t)
	api := herdrapi.New(sock)
	lg := log.New(io.Discard, "", 0)
	idx := newTestIndex(t)

	// index に stale entry を仕込む（実 pane は存在しない）。
	if err := idx.Commit("w99:pGhost", "ghostPC", "w1:pGhost"); err != nil {
		t.Fatalf("idx.Commit: %v", err)
	}

	panes, err := api.PaneList()
	if err != nil {
		t.Fatalf("pane.list: %v", err)
	}
	_, _, dropped := selfHealOnStartup(api, idx, panes, lg)
	if dropped != 1 {
		t.Fatalf("dropped=%d want 1", dropped)
	}
	if _, ok := idx.Get("w99:pGhost"); ok {
		t.Fatalf("stale entry が index に残っている")
	}
}
