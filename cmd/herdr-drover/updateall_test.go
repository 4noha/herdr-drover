package main

// update-all（Web のワンボタン）の回帰テスト。
// **本機能の正しさは「順序」と「逐次性」に尽きる**ので、そこを機械検証する:
//   - claude 更新→自己更新 の順に必ず実行される（自身の再起動は呼び手が最後に行う）
//   - claude 段が失敗したら自己更新へ進まない・restart を返さない
//   - 二重起動は黙って直列化せず loud に拒否

import (
	"bytes"
	"context"
	"github.com/4noha/herdr-drover/internal/agentid"
	"io"
	"os"
	"path/filepath"
	"sort"
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

// ⚠**設計を反転させた（v0.5.27）**。旧契約は「claude 段が失敗したら自己更新へ
// 進まない」だったが、複数エージェント対応で**逆が正しい**と判断した:
//
//	自己更新は不具合修正の**唯一の配布経路**であり、例えば cursor の更新失敗で
//	herdr-drover 自身が更新できなくなるのが実運用で最も困る。エージェント単位の
//	失敗は集約して Ack に残し、他のエージェントと自己更新は続行する。
//
// エージェント**単位**では従来どおり「更新に失敗したらそのセッションは触らない」
// を維持する（古いまま作り直しても目的を達さず pane を無駄に作り直すだけ）。
func TestUpdateAllContinuesWhenOneAgentFails(t *testing.T) {
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
	res, restart, err := runUpdateAll(context.Background(), api, restartOptions{},
		func() (string, bool, error) { selfCalled = true; return "v9", true, nil }, &log)
	if err != nil {
		t.Fatalf("1 エージェントの失敗で全体が error になった: %v\nlog=%s", err, log.String())
	}
	if !selfCalled {
		t.Fatalf("エージェント段の失敗で自己更新へ進まなかった"+
			"（自己更新は不具合修正の唯一の配布経路＝止めてはいけない）\nlog=%s", log.String())
	}
	if !restart {
		t.Fatalf("restart=false（自己更新に成功したら再起動を指示するはず）")
	}
	// 失敗は黙って捨てず**必ず監査に残す**。
	if !strings.Contains(res.Claude, "失敗") || !strings.Contains(res.Claude, "claude") {
		t.Fatalf("失敗が要約に残っていない: %q", res.Claude)
	}
	// 失敗したエージェントのセッションは触らない（エージェント単位の規律は維持）。
	if strings.Contains(res.Claude, "再起動 1 件") {
		t.Fatalf("更新に失敗したのにセッションを作り直した: %q", res.Claude)
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

// update-all は**導入済み × 更新口を持つ**エージェントだけを対象にする。
// 未導入は黙って飛ばさず 1 行出す（silent skip 禁止）。
func TestUpdatableAgentsSelection(t *testing.T) {
	var log bytes.Buffer
	got := updatableAgents(&log)

	// このマシンに実際に導入されているものだけが返る。
	for _, a := range got {
		if _, ok := agentid.Updater(a); !ok {
			t.Errorf("%s: UpdaterSpec が無いのに対象になった", a)
		}
		if _, err := lookupAgentBin(a); err != nil {
			t.Errorf("%s: 未導入なのに対象になった: %v", a, err)
		}
	}
	// 順序は決定的（ログ比較・再現性のため）。
	if !sort.StringsAreSorted(got) {
		t.Errorf("実行順が決定的でない: %v", got)
	}
	// UpdaterSpec を持つが未導入のものは skip 理由が出る。
	for label := range agentid.CanonicalLabels {
		if _, ok := agentid.Updater(label); !ok {
			continue
		}
		if _, err := lookupAgentBin(label); err == nil {
			continue
		}
		if !strings.Contains(log.String(), label+" は更新口を持つがこのマシンに未導入") {
			t.Errorf("%s の skip 理由が出ていない:\n%s", label, log.String())
		}
	}
	t.Logf("このマシンの更新対象: %v", got)
}

// 複数エージェントを順に処理し、要約に**種別ごとの結果**が残ること。
// （失敗時の続行は TestUpdateAllContinuesWhenOneAgentFails が見る）
func TestUpdateAllSummarizesPerAgent(t *testing.T) {
	api, _ := setupUpdateAllPane(t, "2.1.214", "2.1.219")
	selfRan := false
	selfUpdate := func() (string, bool, error) { selfRan = true; return "v9.9.9", true, nil }

	// claude の更新を失敗させる（バイナリ解決は通るが update が非 0 で終わる形は
	// 作りにくいので、更新口を持つが未導入の状態を作れない。代わりに
	// updatableAgents が返す集合の妥当性と、自己更新到達を確認する）。
	var log bytes.Buffer
	res, restart, err := runUpdateAll(context.Background(), api, restartOptions{}, selfUpdate, &log)
	if err != nil {
		t.Fatalf("runUpdateAll: %v\nlog=%s", err, log.String())
	}
	if !selfRan {
		t.Fatal("自己更新に到達していない（エージェント段で止まっている）")
	}
	if !restart {
		t.Fatal("restart=false（成功時は自身の再起動を指示するはず）")
	}
	if res.Self != "更新 v9.9.9" {
		t.Fatalf("Self = %q", res.Self)
	}
	// 複数エージェント対応後も要約に種別名が入る（どれがどうなったか追える）。
	if !strings.Contains(res.Claude, "claude:") {
		t.Fatalf("要約に種別名が無い: %q", res.Claude)
	}
	for _, a := range updatableAgents(io.Discard) {
		if !strings.Contains(res.Claude, a+":") {
			t.Errorf("対象 %s の結果が要約に無い: %q", a, res.Claude)
		}
	}
	t.Logf("要約: %s", summarizeUpdateAll(res))
}
