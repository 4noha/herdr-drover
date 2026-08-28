//go:build unix

package main

// 注入 pane の **client 生存**（pane の存在ではない）に関する検証。
//
// 実障害（2026-08-28・4 PC 12 枚）: マシン再起動でレイアウトが復元されると、
// 注入 pane は token を保ったまま素のシェルで戻り、attach client が居ない。
// それでも旧 reconcile は cur に数えて「健全」と報告し続け、自己修復しなかった。
// ここでは実 herdr（隔離ソケット）で「pane は在るが前景がシェル」を作り、
// reconcile が **同じ pane のまま** client を復活させることを機械検証する。

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/4noha/herdr-drover/internal/herdrapi"
)

// liveStub は「起動を marker に 1 行足してから <tail> を exec する」stub を作る。
// tail に "sh" を渡すと**復元後の素シェル**（client 不在）を、"sleep 300" を渡すと
// **シェルではない前景**を再現できる。marker の行数＝stub が起動した回数。
func liveStub(t *testing.T, tail string) (stub, marker string) {
	t.Helper()
	dir := t.TempDir()
	stub = filepath.Join(dir, "stub")
	marker = filepath.Join(dir, "runs")
	body := "#!/bin/sh\necho run >> " + marker + "\nexec " + tail + "\n"
	if err := os.WriteFile(stub, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return stub, marker
}

func markerRuns(t *testing.T, marker string) int {
	t.Helper()
	b, err := os.ReadFile(marker)
	if err != nil {
		return 0
	}
	return len(strings.Fields(string(b)))
}

// waitRuns は marker の行数（stub の起動回数）が want 以上になるまで待つ。
func waitRuns(t *testing.T, marker string, want int) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if markerRuns(t, marker) >= want {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("stub 起動回数が %d に届かない（現在 %d）", want, markerRuns(t, marker))
}

// waitDeadClient は「client 不在 かつ 前景が既知シェル」＝復元後に観測された
// 状態になるまで待つ（名前で待たない: /bin/sh は macOS で bash に解決される）。
func waitDeadClient(t *testing.T, api *herdrapi.Client, paneID, selfExe, pc, sid string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var last []herdrapi.PaneProcess
	for time.Now().Before(deadline) {
		info, err := api.PaneProcessInfo(paneID)
		if err == nil {
			last = info.ForegroundProcesses
			if !injClientAlive(last, selfExe, pc, sid) && injForegroundShell(last) != "" {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("client 不在＋前景シェルにならない（最後に見た前景: %+v）", last)
}

// waitForegroundName は前景プロセス名が want になるまで待つ。
func waitForegroundName(t *testing.T, api *herdrapi.Client, paneID, want string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		info, err := api.PaneProcessInfo(paneID)
		if err == nil && len(info.ForegroundProcesses) > 0 {
			got = info.ForegroundProcesses[0].Name
			if got == want {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("前景が %q にならない（最後に見たのは %q）", want, got)
}

func onlyInjectedPane(t *testing.T, api *herdrapi.Client) string {
	t.Helper()
	inj := injectedPanes(t, api)
	if len(inj) != 1 {
		t.Fatalf("注入 pane は 1 枚のはず: %v", inj)
	}
	for id := range inj {
		return id
	}
	return ""
}

func newLiveFakeRemote() *fakeRemote {
	return &fakeRemote{
		pcs:      []string{"remoteA"},
		sessions: map[string][]map[string]any{"remoteA": {fakeSess("w9:pA", "projA")}},
	}
}

// TestReconcileRevivesDeadInjectClient: 前景がシェル＝client 不在なら、
// **同じ pane のまま** client を打ち直す（pane を作り直さない）。
// 旧コード（ensureInjectClient 呼び出し無し）ではここが FAIL する。
func TestReconcileRevivesDeadInjectClient(t *testing.T) {
	sock := startHerdrForTest(t)
	api := herdrapi.New(sock)
	stub, marker := liveStub(t, "sh")

	prev := injLivenessEvery
	injLivenessEvery = 0 // 間引き無効（テスト内で即座に判定させる）
	injLivenessAt = map[string]time.Time{}
	t.Cleanup(func() { injLivenessEvery = prev; injLivenessAt = map[string]time.Time{} })

	fr := newLiveFakeRemote()
	lg := log.New(os.Stderr, "[t] ", 0)
	idx := newTestIndex(t)
	ctx := context.Background()

	reconcileRemote(ctx, api, fr, Cloud{PCName: "selfPC"}, stub, idx, lg)
	paneID := onlyInjectedPane(t, api)
	waitRuns(t, marker, 1)                                   // stub が 1 回起動した
	waitDeadClient(t, api, paneID, stub, "remoteA", "w9:pA") // exec 後＝復元後の再現

	reconcileRemote(ctx, api, fr, Cloud{PCName: "selfPC"}, stub, idx, lg)

	waitRuns(t, marker, 2) // ← 旧コードはここで止まる（復活しない）
	if after := onlyInjectedPane(t, api); after != paneID {
		t.Fatalf("pane を作り直している: %s -> %s（同じ pane のまま復活すべき）", paneID, after)
	}
}

// TestReconcileLeavesUnknownForegroundAlone: 前景がシェルでない（ユーザーが
// 何か動かしている）pane には**打ち込まない**。
func TestReconcileLeavesUnknownForegroundAlone(t *testing.T) {
	sock := startHerdrForTest(t)
	api := herdrapi.New(sock)
	stub, marker := liveStub(t, "sleep 300")

	prev := injLivenessEvery
	injLivenessEvery = 0
	injLivenessAt = map[string]time.Time{}
	t.Cleanup(func() { injLivenessEvery = prev; injLivenessAt = map[string]time.Time{} })

	fr := newLiveFakeRemote()
	lg := log.New(os.Stderr, "[t] ", 0)
	idx := newTestIndex(t)
	ctx := context.Background()

	reconcileRemote(ctx, api, fr, Cloud{PCName: "selfPC"}, stub, idx, lg)
	paneID := onlyInjectedPane(t, api)
	waitRuns(t, marker, 1)
	waitForegroundName(t, api, paneID, "sleep")

	reconcileRemote(ctx, api, fr, Cloud{PCName: "selfPC"}, stub, idx, lg)
	time.Sleep(2 * time.Second)
	if got := markerRuns(t, marker); got != 1 {
		t.Fatalf("シェル以外の前景に打ち込んでいる（stub 起動回数=%d・期待 1）", got)
	}
}

// TestInjClientAliveExactMatch: 生存判定は argv の exact-match（推測しない）。
func TestInjClientAliveExactMatch(t *testing.T) {
	self, pc, sid := "/opt/hd/herdr-drover", "pcA", "w1:p9"
	alive := herdrapi.PaneProcess{Name: "herdr-drover", Argv: []string{self, "attach", pc, sid}}
	cases := []struct {
		name string
		fps  []herdrapi.PaneProcess
		want bool
	}{
		{"完全一致", []herdrapi.PaneProcess{alive}, true},
		{"別 sid", []herdrapi.PaneProcess{{Argv: []string{self, "attach", pc, "w1:pZ"}}}, false},
		{"別 pc", []herdrapi.PaneProcess{{Argv: []string{self, "attach", "pcB", sid}}}, false},
		{"別バイナリ", []herdrapi.PaneProcess{{Argv: []string{"/usr/bin/other", "attach", pc, sid}}}, false},
		{"余分な引数", []herdrapi.PaneProcess{{Argv: []string{self, "attach", pc, sid, "--x"}}}, false},
		{"argv nil", []herdrapi.PaneProcess{{Name: "zsh"}}, false},
		{"前景なし", nil, false},
		{"複数のうち 1 つが一致", []herdrapi.PaneProcess{{Name: "zsh", Argv: []string{"-zsh"}}, alive}, true},
	}
	for _, c := range cases {
		if got := injClientAlive(c.fps, self, pc, sid); got != c.want {
			t.Errorf("%s: injClientAlive=%v want %v", c.name, got, c.want)
		}
	}
}

// TestInjForegroundShell: 触ってよいのは「前景が既知シェルだけ」のときに限る。
func TestInjForegroundShell(t *testing.T) {
	cases := []struct {
		name string
		fps  []herdrapi.PaneProcess
		want string
	}{
		{"login zsh", []herdrapi.PaneProcess{{Name: "zsh", Argv: []string{"-zsh"}}}, "zsh"},
		{"先頭ハイフン付き name", []herdrapi.PaneProcess{{Name: "-bash"}}, "bash"},
		{"sh", []herdrapi.PaneProcess{{Name: "sh"}}, "sh"},
		{"シェル以外", []herdrapi.PaneProcess{{Name: "sleep"}}, ""},
		{"シェル＋非シェル混在", []herdrapi.PaneProcess{{Name: "zsh"}, {Name: "vim"}}, ""},
		{"前景なし", nil, ""},
	}
	for _, c := range cases {
		if got := injForegroundShell(c.fps); got != c.want {
			t.Errorf("%s: injForegroundShell=%q want %q", c.name, got, c.want)
		}
	}
}

// TestCloseReasonDistinguishesTeardownFromRemoteLoss:
// desired が空になる経路は「リモートが消えた」と「撤去指示（注入 off / 版数更新後の
// attachrefresh）」の 2 つあり、意味が正反対。ログが両者を取り違えると、正常な
// 作り直しを障害と誤診断する（2026-08-28 に実際に踏んだ）。旧実装は撤去指示でも
// 「リモート消滅」と出していたので、このテストは旧コードで FAIL する。
func TestCloseReasonDistinguishesTeardownFromRemoteLoss(t *testing.T) {
	sock := startHerdrForTest(t)
	api := herdrapi.New(sock)
	stub := reconcileStub(t)
	idx := newTestIndex(t)
	ctx := context.Background()
	const selfPC = "self-herdr"

	newRemote := func() *fakeRemote {
		return &fakeRemote{
			pcs:      []string{selfPC, "remoteA"},
			sessions: map[string][]map[string]any{"remoteA": {fakeSess("w9:pA", "projA")}},
		}
	}

	// ① 撤去指示（emptyRemoteSource）: リモートは健在なのに撤去される経路。
	var buf bytes.Buffer
	lg := log.New(&buf, "", 0)
	reconcileRemote(ctx, api, newRemote(), Cloud{PCName: selfPC}, stub, idx, lg)
	waitCond(t, 15*time.Second, "注入 pane 出現", func() bool {
		return len(injectedPanes(t, api)) == 1
	})
	buf.Reset()
	reconcileRemote(ctx, api, emptyRemoteSource{}, Cloud{PCName: selfPC}, stub, idx, lg)
	waitCond(t, 15*time.Second, "撤去される", func() bool {
		return len(injectedPanes(t, api)) == 0
	})
	if out := buf.String(); !strings.Contains(out, "撤去指示") || strings.Contains(out, "リモート消滅") {
		t.Fatalf("撤去指示の CLOSE 理由が誤っている:\n%s", out)
	}

	// ② 本当にリモートが消えた場合は従来どおり「リモート消滅」。
	buf.Reset()
	reconcileRemote(ctx, api, newRemote(), Cloud{PCName: selfPC}, stub, idx, lg)
	waitCond(t, 15*time.Second, "再注入される", func() bool {
		return len(injectedPanes(t, api)) == 1
	})
	buf.Reset()
	gone := &fakeRemote{pcs: []string{selfPC, "remoteA"}, sessions: map[string][]map[string]any{"remoteA": {}}}
	reconcileRemote(ctx, api, gone, Cloud{PCName: selfPC}, stub, idx, lg)
	waitCond(t, 15*time.Second, "消滅で撤去される", func() bool {
		return len(injectedPanes(t, api)) == 0
	})
	if out := buf.String(); !strings.Contains(out, "リモート消滅") {
		t.Fatalf("リモート消滅の CLOSE 理由が出ていない:\n%s", out)
	}
}
