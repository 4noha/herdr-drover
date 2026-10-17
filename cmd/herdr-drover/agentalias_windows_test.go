package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/4noha/herdr-drover/internal/agentid"
	"github.com/4noha/herdr-drover/internal/herdrapi"
)

// TestWindowsHerdrAliasDetection は agentid の herdr ミラー（lookup_agent の alias 表と
// 名前の正規化）が**実 herdr 0.9.5 の検出と一致する**ことを確かめる。表は herdr の
// 実ソースから写しているが、写し間違い・herdr 側の変更は実機でしか分からない。
//
//   - 全 canonical agent の全 alias（＋muse の版付き実体 `muse-bin-<版>`）を
//     `<alias>.exe` という名前の偽エージェント（テストバイナリの複製）で起動し、
//     pane.agent がその canonical label になること（agentid.HerdrAgentForExec と同値）。
//   - npm global shim 形: argv[0] が `qwen.cmd`（中身は `"<非 agent 名の exe>" %*`）でも
//     herdr は qwen と検出し、**付けた引数が本体まで届く**こと＝IsDirectInvocation が
//     `.cmd` を直接起動に数える根拠（herdr 0.9.5 自身も cursor を `cursor-agent.cmd
//     --resume <v>` で再開する）。
func TestWindowsHerdrAliasDetection(t *testing.T) {
	sock := startHerdrForTest(t)
	api := herdrapi.New(sock)
	work := t.TempDir()
	wsID, err := currentWorkspaceID(api)
	if err != nil {
		t.Fatalf("currentWorkspaceID: %v", err)
	}

	type probe struct {
		name, want, pane string
	}
	var probes []*probe
	var labels []string
	for label := range agentid.CanonicalLabels {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	for _, label := range labels {
		names := append([]string(nil), agentid.HerdrExecAliases(label)...)
		if label == "muse" {
			names = append(names, "muse-bin-0.1.0-R708.1")
		}
		for _, n := range names {
			probes = append(probes, &probe{name: n, want: label})
		}
	}

	// layout.apply は spawn まで同期で、新しい exe ごとに ~2 秒かかる（56 本で ~110 秒・
	// 実測。並列に投げても縮まない＝herdr／Defender 側で直列化される）。
	launch := func(p *probe, argv []string) {
		pane, err := applyClaudeTab(api, wsID, p.name, argv, work)
		if err != nil {
			t.Fatalf("%s: layout.apply %q: %v", p.name, argv, err)
		}
		p.pane = pane
	}
	for _, p := range probes {
		if got := agentid.HerdrAgentForExec(p.name + ".exe"); got != p.want {
			t.Fatalf("ミラー自体の不一致: HerdrAgentForExec(%q)=%q want %q", p.name+".exe", got, p.want)
		}
		exe := writeAgentStub(t, filepath.Join(work, p.name), "", stubSpec{SleepSec: 300})
		launch(p, []string{exe})
	}

	// npm shim 形。本体は agent 名でない exe（node 相当）＝検出は .cmd の名前経由のみ。
	// ⚠herdr 0.9.5 の前景一覧には cmd.exe しか出ない（子の本体は出ない・実測）ので、
	// 引数の到達は別 pane で確かめる: 本体が --hd-probe を受けたら marker を書いて終わる。
	writeShim := func(dir string, inner string) string {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		shim := filepath.Join(dir, "qwen.cmd")
		if err := os.WriteFile(shim, []byte("@\""+inner+"\" %*\r\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return shim
	}
	shim := writeShim(filepath.Join(work, "npm"),
		writeAgentStub(t, filepath.Join(work, "hdinner"), "", stubSpec{SleepSec: 300}))
	if !agentid.IsDirectInvocation("qwen", []string{shim}) {
		t.Fatalf("IsDirectInvocation(qwen, %q)=false", shim)
	}
	shimProbe := &probe{name: "qwen.cmd(npm shim)", want: "qwen"}
	launch(shimProbe, []string{shim})
	probes = append(probes, shimProbe)

	marker := filepath.Join(work, "shim-arg-reached")
	argShim := writeShim(filepath.Join(work, "npm2"),
		writeAgentStub(t, filepath.Join(work, "hdinner2"), "", stubSpec{
			AnyArg:   map[string]stubReply{"--hd-probe": {WriteFile: marker, WriteBody: "ok"}},
			SleepSec: 300,
		}))
	launch(&probe{name: "qwen.cmd(arg)"}, []string{argShim, "--resume", "R", "--hd-probe"})

	deadline := time.Now().Add(30 * time.Second)
	got := map[*probe]string{}
	for time.Now().Before(deadline) {
		pending := 0
		for _, p := range probes {
			if got[p] == p.want {
				continue
			}
			if pg, err := api.PaneGet(p.pane); err == nil {
				got[p] = pg.Agent
			}
			if got[p] != p.want {
				pending++
			}
		}
		if pending == 0 {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	var bad []string
	for _, p := range probes {
		if got[p] != p.want {
			fps := ""
			if pi, err := api.PaneProcessInfo(p.pane); err == nil && pi != nil {
				fps = fmt.Sprintf("%+v", pi.ForegroundProcesses)
			}
			bad = append(bad, fmt.Sprintf("%q: pane.agent=%q want %q fg=%s", p.name, got[p], p.want, fps))
		}
	}
	if len(bad) > 0 {
		t.Fatalf("実 herdr の検出がミラーと食い違う（%d/%d）:\n  %s", len(bad), len(probes), strings.Join(bad, "\n  "))
	}
	t.Logf("実 herdr で %d 名すべて検出一致", len(probes))

	// shim 越しに付けた引数が本体へ届いたか（resume 引数の付け直しが効く根拠）。
	for time.Now().Before(deadline.Add(10 * time.Second)) {
		if b, err := os.ReadFile(marker); err == nil && string(b) == "ok" {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("qwen.cmd に付けた --hd-probe が本体へ届いていない（%s が無い）", marker)
}
