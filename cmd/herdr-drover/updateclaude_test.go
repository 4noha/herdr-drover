package main

// update-claude の回帰テスト。claude 実バイナリの解決（**PATH 頼みにしない**のが
// 肝）と「更新→セッション反映」の連結を、実 herdr（隔離）＋stub で担保する。

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/4noha/herdr-drover/internal/herdrapi"
)

// writeUpdatableStub は `update` サブコマンドで版を上げ、`--version` で現在版を
// 返す stub claude を作る（版は verFile に保持＝実 claude の symlink 張替えに相当）。
// 引数なし起動は生き続ける（pane のプロセスとして振る舞う）。
func writeUpdatableStub(t *testing.T, dir, from, to string) (bin, verFile string) {
	t.Helper()
	verFile = filepath.Join(dir, "version.txt")
	if err := os.WriteFile(verFile, []byte(from), 0o644); err != nil {
		t.Fatalf("version.txt: %v", err)
	}
	bin = filepath.Join(dir, "claude-stub")
	script := "#!/bin/sh\n" +
		"VF=" + verFile + "\n" +
		"case \"$1\" in\n" +
		"  --version) cat \"$VF\"; echo; exit 0 ;;\n" +
		"  update) echo \"updated to " + to + "\"; printf %s '" + to + "' > \"$VF\"; exit 0 ;;\n" +
		"esac\n" +
		"exec sleep 300\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("stub 作成: %v", err)
	}
	return bin, verFile
}

// 主経路: 稼働 pane の argv[0] を権威に claude を更新し、続けてセッションを
// 作り直す（1 コマンドで両方）。**PATH には stub を置かない**＝PATH 解決に
// 退行したらこのテストが落ちる。
func TestUpdateClaudeUpdatesThenRestarts(t *testing.T) {
	sock := startHerdrForTest(t)
	api := herdrapi.New(sock)
	work := t.TempDir()
	bin, verFile := writeUpdatableStub(t, work, "2.1.214", "2.1.219")
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

	var log bytes.Buffer
	summary, err := updateClaudeAndRestart(context.Background(), api, "", false, false, &log)
	if err != nil {
		t.Fatalf("updateClaudeAndRestart: %v（log=%s）", err, log.String())
	}

	// claude 本体が更新されたこと（stub の版ファイル）。
	got, err := os.ReadFile(verFile)
	if err != nil {
		t.Fatalf("version.txt 読取: %v", err)
	}
	if strings.TrimSpace(string(got)) != "2.1.219" {
		t.Fatalf("更新後の版 = %q（2.1.219 のはず）", got)
	}
	if !strings.Contains(summary, "更新 2.1.214 → 2.1.219") {
		t.Fatalf("要約に版の前後が無い: %q", summary)
	}

	// 続けてセッションが作り直されたこと（1 コマンドで両方＝本機能の目的）。
	if !strings.Contains(summary, "再起動 1 件") {
		t.Fatalf("要約に再起動結果が無い: %q", summary)
	}
	ag, found := paneOfAgent(t, api, "claude")
	if !found {
		t.Fatalf("再起動後に agent claude が消えた。log=%s", log.String())
	}
	if ag.PaneID == pane {
		t.Fatalf("pane_id が変わっていない＝再起動されていない: %s", ag.PaneID)
	}
	// バイナリの解決根拠を必ず出す（silent な選択をしない）。
	if !strings.Contains(log.String(), "argv[0]") {
		t.Fatalf("対象バイナリの根拠が出力されていない: %s", log.String())
	}
}

// 更新が無くても再起動する（「ディスクは最新だがセッションは旧版」を直すのが目的
// ＝本コマンドの存在理由。ここを skip する実装に退行したら落ちる）。
func TestUpdateClaudeRestartsEvenWhenAlreadyLatest(t *testing.T) {
	sock := startHerdrForTest(t)
	api := herdrapi.New(sock)
	work := t.TempDir()
	bin, _ := writeUpdatableStub(t, work, "2.1.219", "2.1.219")
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

	var log bytes.Buffer
	summary, err := updateClaudeAndRestart(context.Background(), api, "", false, false, &log)
	if err != nil {
		t.Fatalf("updateClaudeAndRestart: %v（log=%s）", err, log.String())
	}
	if !strings.Contains(summary, "既に最新") {
		t.Fatalf("要約が「既に最新」でない: %q", summary)
	}
	if !strings.Contains(summary, "再起動 1 件") {
		t.Fatalf("更新が無いとき再起動していない（本コマンドの目的を果たさない）: %q", summary)
	}
	ag, found := paneOfAgent(t, api, "claude")
	if !found || ag.PaneID == pane {
		t.Fatalf("pane が作り直されていない: found=%v pane=%s", found, ag.PaneID)
	}
}

// 対象バイナリは稼働 pane の argv[0]（PATH ではない）。複数種類が混在したら
// **推測せず loud に error**。
func TestResolveClaudeBin(t *testing.T) {
	sock := startHerdrForTest(t)
	api := herdrapi.New(sock)
	cwd := t.TempDir()
	wsID, err := currentWorkspaceID(api)
	if err != nil {
		t.Fatalf("currentWorkspaceID: %v", err)
	}

	// ⚠stub は**外側スコープ**で作る: subtest の t.TempDir() は subtest 終了時に
	// 消えるため、1 本目の pane のプロセスがそこで死んで 2 本目しか残らず
	// 「混在」を作れない（実測。テスト側の寿命バグで偽陰性になっていた）。
	binA := writeStubClaude(t)
	binB := writeStubClaude(t)

	// 1 種類だけ稼働 → その argv[0]。
	paneA, err := applyClaudeTab(api, wsID, "a", []string{binA}, cwd)
	if err != nil {
		t.Fatalf("layout.apply(a): %v", err)
	}
	if _, err := renameClaudePaneTo(api, paneA, "claude"); err != nil {
		t.Fatalf("agent.rename(a): %v", err)
	}
	got, source, err := resolveClaudeBin(api)
	if err != nil {
		t.Fatalf("resolveClaudeBin: %v", err)
	}
	if got != binA {
		t.Fatalf("bin = %q, want %q（PATH ではなく pane の argv[0] が権威）", got, binA)
	}
	if !strings.Contains(source, "argv[0]") {
		t.Fatalf("source = %q（pane 由来と分かる必要がある）", source)
	}

	// 食い違う 2 種類が同時に稼働 → 推測せず loud に error。
	paneB, err := applyClaudeTab(api, wsID, "b", []string{binB}, cwd)
	if err != nil {
		t.Fatalf("layout.apply(b): %v", err)
	}
	if _, err := renameClaudePaneTo(api, paneB, "claude-2"); err != nil {
		t.Fatalf("agent.rename(b): %v", err)
	}
	bins, berr := claudeBinsFromPanes(api)
	if berr != nil || len(bins) != 2 {
		t.Fatalf("前提が崩れている（2 種類が同時に稼働しているはず）: bins=%v err=%v", bins, berr)
	}
	if _, _, err = resolveClaudeBin(api); err == nil {
		t.Fatalf("2 種類のバイナリが混在しているのに error にならなかった")
	} else if !strings.Contains(err.Error(), "一意に決められない") {
		t.Fatalf("error 文面が曖昧さを説明していない: %v", err)
	}
}

// 更新に失敗したらセッション再起動へ進まない（古いまま作り直しても目的を
// 達さず pane を無駄に壊すだけ）。
func TestUpdateClaudeDoesNotRestartWhenUpdateFails(t *testing.T) {
	sock := startHerdrForTest(t)
	api := herdrapi.New(sock)
	work := t.TempDir()
	bin := filepath.Join(work, "claude-stub")
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

	var log bytes.Buffer
	if _, err := updateClaudeAndRestart(context.Background(), api, "", false, false, &log); err == nil {
		t.Fatalf("update 失敗が error にならなかった。log=%s", log.String())
	}
	// pane はそのまま（作り直していない）。
	if p, gerr := api.PaneGet(pane); gerr != nil || p.PaneID != pane {
		t.Fatalf("更新失敗なのに pane が作り直された: %v %+v", gerr, p)
	}
}
