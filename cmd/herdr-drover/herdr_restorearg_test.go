package main

// herdr の「再起動で何が復元起動されるか」の土台になる契約を実測で固定する。
// BUG-7（Tab 倍増）の発生条件はこの契約で決まるので、herdr 側が変わったら
// ここが落ちて気づける（倍増の話が推測に戻らないための杭）。
//
// 実測（herdr 0.7.4・2026-09-04）:
//   - layout.export / session.json の pane.command は **pane 生成時の launch
//     argv だけ**。あとから前景に来た子プロセスは記録されない。
//   - 既定シェル pane（ユーザーが素シェルで `claude` と打つ形）は command を
//     持たず、cwd と agent_session だけが永続化される。
//     ＝この pane の復元では launch argv の再実行は起こり得ず、herdr が
//     agent_session から自前の resume を起こす（対話シェル経由なので
//     `alias claude` が効く＝drover シムが走る）。BUG-7 の入口はこちら。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/4noha/herdr-drover/internal/herdrapi"
)

func TestHerdrPersistsLaunchArgvNotForegroundProcess(t *testing.T) {
	sock := startHerdrForTest(t)
	api := herdrapi.New(sock)
	dir := t.TempDir()

	mk := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// launch argv 用（自身を exec で置き換える＝前景は argv と別物になる）。
	launched := mk("PROBE-LAUNCHARGV", "#!/bin/sh\nexec sleep 600\n")
	// あとから前景に来る子プロセス（本番のシム相当）。
	child := mk("PROBE-SHELLCHILD", "#!/bin/sh\nsleep 600\n")

	ws, err := api.WorkspaceCreate()
	if err != nil {
		t.Fatal(err)
	}
	argvPane, err := applyClaudeTab(api, ws.Workspace.WorkspaceID, "probe", []string{launched, "--resume", "REF"}, dir)
	if err != nil {
		t.Fatal(err)
	}
	shellPane := ws.RootPane.PaneID

	fgOf := func(pane string) string {
		pi, err := api.PaneProcessInfo(pane)
		if err != nil || pi == nil {
			return ""
		}
		var s []string
		for _, f := range pi.ForegroundProcesses {
			s = append(s, f.Name+"["+strings.Join(f.Argv, " ")+"]")
		}
		return strings.Join(s, " | ")
	}
	// ⚠シェル初期化（rbenv init 等）の最中に送ると入力が消える＝「記録されない」と
	// 誤読する。前景がシェルだけになるまで待ってから送る。
	waitCond(t, 30*time.Second, "シェル初期化の完了（前景がシェルだけ）", func() bool {
		pi, err := api.PaneProcessInfo(shellPane)
		return err == nil && pi != nil && len(pi.ForegroundProcesses) == 1
	})
	if err := api.PaneSendText(shellPane, child+" --resume REF\r"); err != nil {
		t.Fatal(err)
	}
	waitCond(t, 30*time.Second, "シェル pane の前景に子プロセスが現れる", func() bool {
		return strings.Contains(fgOf(shellPane), "PROBE-SHELLCHILD")
	})
	waitCond(t, 30*time.Second, "launch argv pane の前景が立ち上がる", func() bool {
		return fgOf(argvPane) != ""
	})

	command := func(pane string) []string {
		p, err := api.PaneGet(pane)
		if err != nil || p == nil {
			t.Fatalf("pane.get %s: %v", pane, err)
		}
		root, err := exportTabLayout(api, p.TabID)
		if err != nil {
			t.Fatalf("layout.export %s: %v", p.TabID, err)
		}
		for _, lf := range root.leaves() {
			if lf.PaneID == pane {
				return lf.Command
			}
		}
		t.Fatalf("pane %s が layout.export に無い", pane)
		return nil
	}

	// (1) launch argv は前景が別物（exec 済み）でもそのまま記録される。
	if got, want := strings.Join(command(argvPane), " "), launched+" --resume REF"; got != want {
		t.Errorf("launch argv が記録されていない: command=%q want=%q（前景=%s）", got, want, fgOf(argvPane))
	}
	// (2) 既定シェル pane は、前景に子プロセスが居ても command を持たない。
	//     ＝素シェルで打った `claude`（alias→シム）は復元 argv にならない。
	if got := command(shellPane); len(got) != 0 {
		t.Errorf("既定シェル pane に command が記録された: %v（前景=%s）"+
			"＝復元の前提が変わった。BUG-7 の発生条件を再調査すること", got, fgOf(shellPane))
	}
}
