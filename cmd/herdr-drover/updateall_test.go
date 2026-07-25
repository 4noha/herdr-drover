package main

// update-all（Web のワンボタン）の回帰テスト。
// **本機能の正しさは「順序」と「逐次性」に尽きる**ので、そこを機械検証する:
//   - claude 更新→自己更新 の順に必ず実行される（自身の再起動は呼び手が最後に行う）
//   - claude 段が失敗したら自己更新へ進まない・restart を返さない
//   - 二重起動は黙って直列化せず loud に拒否

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/4noha/herdr-drover/internal/herdrapi"
)

// setupUpdateAllPane は実 herdr に claude pane を 1 枚用意して api を返す。
func setupUpdateAllPane(t *testing.T, from, to string) (*herdrapi.Client, string) {
	t.Helper()
	sock := startHerdrForTest(t)
	api := herdrapi.New(sock)
	bin, _ := writeUpdatableStub(t, t.TempDir(), from, to)
	cwd := t.TempDir()
	wsID, err := currentWorkspaceID(api)
	if err != nil {
		t.Fatalf("currentWorkspaceID: %v", err)
	}
	pane, err := applyClaudeTab(api, wsID, "proj", []string{bin}, cwd)
	if err != nil {
		t.Fatalf("layout.apply: %v", err)
	}
	if _, err := renameClaudePaneTo(api, pane, "claude"); err != nil {
		t.Fatalf("agent.rename: %v", err)
	}
	// 実運用の pane は integration hook が会話 ref を報告している。安全網
	// （ref が取れない pane は既定で触らない）に引っかからないよう実態に合わせる。
	if err := api.ReportAgentSession(pane, "herdr:claude", "claude",
		"33333333-4444-4555-8666-777777777777"); err != nil {
		t.Fatalf("report_agent_session: %v", err)
	}
	return api, pane
}

// 正常系: claude 段 → 自己更新段 の順で走り、restart=true が返る
// （呼び手＝CommandRunner が Ack 後に exit する契約）。
func TestUpdateAllRunsClaudeThenSelfInOrder(t *testing.T) {
	api, _ := setupUpdateAllPane(t, "2.1.214", "2.1.219")

	var order []string
	var mu sync.Mutex
	selfUpdate := func() (string, bool, error) {
		mu.Lock()
		order = append(order, "self")
		mu.Unlock()
		return "v9.9.9", true, nil
	}

	var log bytes.Buffer
	res, restart, err := runUpdateAll(context.Background(), api, restartOptions{}, selfUpdate, &log)
	if err != nil {
		t.Fatalf("runUpdateAll: %v（log=%s）", err, log.String())
	}
	if !restart {
		t.Fatalf("restart=false（成功時は自身の再起動を指示するはず）")
	}
	if !strings.Contains(res.Claude, "更新 2.1.214 → 2.1.219") {
		t.Fatalf("claude 段の結果が入っていない: %+v", res)
	}
	if !strings.Contains(res.Claude, "再起動 1 件") {
		t.Fatalf("claude セッション再起動が走っていない: %+v", res)
	}
	if res.Self != "更新 v9.9.9" {
		t.Fatalf("self 段の結果 = %q", res.Self)
	}
	// 順序: claude 段のログが self 段より前に出ていること。
	s := log.String()
	ci, si := strings.Index(s, "[1/2]"), strings.Index(s, "[2/2]")
	if ci < 0 || si < 0 || ci > si {
		t.Fatalf("段の順序が [1/2]→[2/2] でない: %s", s)
	}
	if len(order) != 1 || order[0] != "self" {
		t.Fatalf("selfUpdate 呼び出し = %v（ちょうど 1 回のはず）", order)
	}
	if got := summarizeUpdateAll(res); !strings.Contains(got, "claude:") || !strings.Contains(got, "drover:") {
		t.Fatalf("要約に両段が出ない: %q", got)
	}
}

// claude 段が失敗したら自己更新へ進まない（古い claude のまま daemon だけ
// 新しくして再起動する、という原因の霞む状態を作らない）。
func TestUpdateAllStopsWhenClaudeStepFails(t *testing.T) {
	sock := startHerdrForTest(t)
	api := herdrapi.New(sock)
	work := t.TempDir()
	bin := filepath.Join(work, "claude")
	script := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"  --version) echo '2.1.214'; exit 0 ;;\n" +
		"  update) echo 'network unreachable' >&2; exit 1 ;;\n" +
		"esac\n" +
		"exec sleep 300\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("stub 作成: %v", err)
	}
	cwd := t.TempDir()
	wsID, _ := currentWorkspaceID(api)
	pane, err := applyClaudeTab(api, wsID, "proj", []string{bin}, cwd)
	if err != nil {
		t.Fatalf("layout.apply: %v", err)
	}
	if _, err := renameClaudePaneTo(api, pane, "claude"); err != nil {
		t.Fatalf("agent.rename: %v", err)
	}
	reportTestSession(t, api, pane, "claude", "aaaaaaaa-1111-4111-8111-293692086369")

	selfCalled := false
	var log bytes.Buffer
	_, restart, err := runUpdateAll(context.Background(), api, restartOptions{},
		func() (string, bool, error) { selfCalled = true; return "v9", true, nil }, &log)
	if err == nil {
		t.Fatalf("claude 段の失敗が error にならなかった")
	}
	if selfCalled {
		t.Fatalf("claude 段が失敗したのに自己更新へ進んだ")
	}
	if restart {
		t.Fatalf("失敗時に restart=true（再起動すると原因が霞む）")
	}
}

// 二重起動は loud に拒否（逐次実行が前提なので黙って直列化しない）。
func TestUpdateAllRejectsConcurrentRun(t *testing.T) {
	api, _ := setupUpdateAllPane(t, "2.1.219", "2.1.219")

	entered := make(chan struct{})
	release := make(chan struct{})
	var log1, log2 bytes.Buffer
	var firstErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, firstErr = runUpdateAll(context.Background(), api, restartOptions{},
			func() (string, bool, error) {
				close(entered)
				<-release // 1 本目を段 2 で止めておく
				return "v9", false, nil
			}, &log1)
	}()

	<-entered
	_, restart, err := runUpdateAll(context.Background(), api, restartOptions{},
		func() (string, bool, error) { t.Error("2 本目が段 2 まで進んだ"); return "", false, nil }, &log2)
	if err == nil {
		t.Fatalf("同時実行が拒否されなかった")
	}
	if !strings.Contains(err.Error(), "既に実行中") {
		t.Fatalf("拒否理由が読めない: %v", err)
	}
	if restart {
		t.Fatalf("拒否時に restart=true")
	}
	close(release)
	<-done
	if firstErr != nil {
		t.Fatalf("1 本目が失敗: %v（log=%s）", firstErr, log1.String())
	}
}
