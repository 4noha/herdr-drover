//go:build !windows

package e2e

// claude シムの実 TTY e2e（Gate フェーズの pty ハーネス検証）。
//
// cmd/herdr-drover/claudeshim_test.go は exec seam で「exec に渡る引数列」を
// 機械検証するが、herdr terminal attach は TTY 必須（pipe だと ratatui init
// panic exit=101 の実測）のため attach 成立そのものは seam では検証できない。
// ここでは python3 pty ハーネス（openpty＋TIOCSWINSZ＝0x0 だと UI が立たない
// 実測への対処）配下で **実バイナリ herdr-drover claude** を走らせ、
//   1. attach 成立（stub claude の marker が ANSI strip 後の pty 出力に出現）
//      → Ctrl+B q detach → プロセス正常終了・pane は生存（detach≠kill）
//   2. 同 cwd 複数候補の picker で "2\r" → 表示上の 2 番目へ attach
//   3. 非 TTY（pipe）は attach せず既存を報告・dup 非生成（backstop）
// を実 herdr で機械検証する（合成ストリームなし。herdr/python3 不在は Skip）。
//
// ハーネスをテスト自身が一時 dir へ書き出すのは「リポジトリに増えるのは本
// テストファイルだけ」を保つため（実行時生成であって合成 fixture ではない）。
//
// 実測知見（このテストが依存する一次事実）:
//   - herdr は agent の cwd を物理パスで登録する。シム側 os.Getwd は PWD が
//     現 dir と同 inode なら PWD を返すため、/tmp（symlink）経路の cwd や
//     stale PWD だと cwd 完全一致が偽陰性になる（cm start と同根の quirk）。
//     → work dir は EvalSymlinks 済み物理パス・ハーネス env の PWD も明示。
//   - os/exec は Stdin=nil を /dev/null にする。/dev/null は char device＝
//     旧 CharDevice 判定の stdinIsTTY は真になり attach を試みて panic した
//     （実測）。この実バグは termios 判定へ修正済み（/dev/null=false・
//     回帰は cmd 側 TestStdinIsTTYRealDevices）。本テストの pipe 渡しは
//     「pipe=非 TTY」の実 stdin 経路検証として維持する。
//   - agent.list の並びは作成順と一致しない（実測: p1,p3,p2）。picker の
//     「2 番目」は表示された [2] 行を capture から exact に読み取って判定する
//     （作成順から推測しない＝ヒューリスティック禁止の鉄則）。

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/4noha/herdr-drover/internal/herdrapi"
)

// ptyHarnessPy は python3 pty ハーネス本体。openpty→TIOCSWINSZ→setsid＋
// TIOCSCTTY で実 TTY 配下にコマンドを spawn し、ANSI strip 後の累積出力に
// --expect が出現したら対応する --send を pty へ書く（expect/send 逐次消化）。
// 終了時に raw を --out へ、HARNESS_STEPS/HARNESS_EXIT を stdout へ出す。
const ptyHarnessPy = `#!/usr/bin/env python3
# pty harness: run cmd under a real TTY and drive expect/send steps.
# TIOCSWINSZ is mandatory (0x0 leaves the ratatui UI unstarted; measured).
import argparse, codecs, errno, fcntl, os, re, select, struct, subprocess, sys, termios, time

ANSI_RE = re.compile(
    rb"\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)"  # OSC
    rb"|\x1b\[[0-9;?<=>]*[ -/]*[@-~]"      # CSI
    rb"|\x1b[@-Z\\-_]"                     # bare ESC finals
    rb"|\x1b[()][0-9A-Za-z]")              # charset designation

def strip_ansi(data):
    return ANSI_RE.sub(b"", data)

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--timeout", type=float, default=30.0)
    ap.add_argument("--rows", type=int, default=40)
    ap.add_argument("--cols", type=int, default=120)
    ap.add_argument("--out", required=True)
    ap.add_argument("--expect", action="append", default=[])
    ap.add_argument("--send", action="append", default=[])
    ap.add_argument("cmd", nargs="+")
    args = ap.parse_args()
    if len(args.expect) != len(args.send):
        print("expect/send must pair up", file=sys.stderr)
        return 3
    steps = list(zip(args.expect, args.send))
    master, slave = os.openpty()
    fcntl.ioctl(slave, termios.TIOCSWINSZ,
                struct.pack("HHHH", args.rows, args.cols, 0, 0))

    def preexec():
        os.setsid()
        fcntl.ioctl(0, termios.TIOCSCTTY, 0)  # fd0 = pty slave (already dup2ed)

    child = subprocess.Popen(args.cmd, stdin=slave, stdout=slave, stderr=slave,
                             preexec_fn=preexec, close_fds=True)
    os.close(slave)
    raw, stripped = bytearray(), bytearray()
    step_i, exit_code = 0, None
    deadline = time.monotonic() + args.timeout
    try:
        while True:
            if time.monotonic() > deadline:
                print("HARNESS_TIMEOUT step=%d/%d" % (step_i, len(steps)), file=sys.stderr)
                child.kill(); child.wait(); exit_code = 2
                break
            r, _, _ = select.select([master], [], [], 0.1)
            if master in r:
                try:
                    chunk = os.read(master, 65536)
                except OSError as e:
                    if e.errno == errno.EIO:  # all slave fds closed = child gone
                        chunk = b""
                    else:
                        raise
                if chunk:
                    raw.extend(chunk); stripped.extend(strip_ansi(chunk))
                else:
                    child.wait()
            while step_i < len(steps):
                expect, send = steps[step_i]
                if expect.encode() not in bytes(stripped):
                    break
                if send:
                    os.write(master, codecs.escape_decode(send.encode())[0])
                step_i += 1
            if child.poll() is not None:
                end = time.monotonic() + 0.5  # drain leftovers best-effort
                while time.monotonic() < end:
                    r, _, _ = select.select([master], [], [], 0.05)
                    if master not in r:
                        continue
                    try:
                        chunk = os.read(master, 65536)
                    except OSError:
                        break
                    if not chunk:
                        break
                    raw.extend(chunk); stripped.extend(strip_ansi(chunk))
                if exit_code is None:
                    exit_code = 0 if step_i == len(steps) else 4
                break
    finally:
        with open(args.out, "wb") as f:
            f.write(bytes(raw))
        os.close(master)
    print("HARNESS_STEPS=%d/%d" % (step_i, len(steps)))
    print("HARNESS_EXIT=%s" % child.returncode)
    return exit_code if exit_code else 0

if __name__ == "__main__":
    sys.exit(main())
`

// capture の ANSI 除去は webterm_e2e_test.go の stripANSI（既存・同 package）を
// 再利用する（marker は英数字とアンダースコアのみ＝この粒度で十分）。

// writePtyHarness はハーネスを一時 dir へ書き出す。python3 不在環境は Skip
// （herdr 不在 Skip と同じ粒度＝握り潰さない）。
func writePtyHarness(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed; skipping pty harness e2e")
	}
	p := filepath.Join(t.TempDir(), "pty_harness.py")
	if err := os.WriteFile(p, []byte(ptyHarnessPy), 0o755); err != nil {
		t.Fatalf("harness 書出し: %v", err)
	}
	return p
}

// physicalDir は EvalSymlinks 済みの一時 dir（cwd 完全一致の偽陰性対策。
// 冒頭コメントの「物理パス」実測知見を参照）。
func physicalDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	return dir
}

// installStubClaudeAt は dir/claude に stub（marker echo + sleep）を置く。
// 実 claude は隔離不能な副作用（~/.claude 書込等）があるため使わない。
// marker が pty 出力に出る＝「attach が pane の実画面を届けた」の直接物証。
func installStubClaudeAt(t *testing.T, dir, marker string) string {
	t.Helper()
	stub := filepath.Join(dir, "claude")
	script := "#!/bin/sh\necho " + marker + "\nexec sleep 300\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("stub claude 作成: %v", err)
	}
	return stub
}

// shimEnv はハーネス／実バイナリ用の env を組む。srv.env（HERDR_SOCKET_PATH
// ＋XDG_CONFIG_HOME 隔離済み）に PATH 前置と PWD 明示を重ねる（os/exec は
// 重複キー後勝ち）。PWD を work に固定しないと外側 go test の PWD が同 inode
// を指した場合に os.Getwd が symlink 経路を返し cwd 一致が偽陰性になる。
func shimEnv(srv *testServer, stubDir, work string) []string {
	return append(append([]string{}, srv.env...),
		"PATH="+stubDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"PWD="+work)
}

// harnessResult は 1 回のハーネス実行の観測値。
type harnessResult struct {
	stdout   string // ハーネス自身の stdout/stderr（HARNESS_STEPS/EXIT 行）
	raw      []byte // pty から吸った子プロセス出力の生バイト
	stripped string // 同 ANSI strip 後
}

// runPtyHarness は expect/send のペア列でハーネスを実行し、ハーネス exit 0
// （全 step 消化）と子プロセス exit 0（正常終了）まで検証して capture を返す。
func runPtyHarness(t *testing.T, harness, work string, env []string, steps [][2]string, cmd ...string) harnessResult {
	t.Helper()
	capFile := filepath.Join(t.TempDir(), "cap.bin")
	args := []string{harness, "--timeout", "35", "--out", capFile}
	for _, s := range steps {
		args = append(args, "--expect", s[0], "--send", s[1])
	}
	args = append(args, "--")
	args = append(args, cmd...)
	c := exec.Command("python3", args...)
	c.Dir = work
	c.Env = env
	out, err := c.CombinedOutput()
	raw, _ := os.ReadFile(capFile)
	stripped := stripANSI(raw)
	if err != nil {
		t.Fatalf("pty ハーネス失敗: %v\nharness out:\n%s\ncapture(strip):\n%s", err, out, stripped)
	}
	if !strings.Contains(string(out), "HARNESS_EXIT=0") {
		t.Fatalf("attach プロセスが正常終了していない:\n%s\ncapture(strip):\n%s", out, stripped)
	}
	return harnessResult{stdout: string(out), raw: raw, stripped: stripped}
}

// shimAgentsAt は agent.list から name（exact）と cwd（exact）で絞る。
// テストが自分で作った名前だけを数える＝ヒューリスティックなし。
func shimAgentsAt(t *testing.T, api *herdrapi.Client, cwd string, names ...string) []herdrapi.AgentInfo {
	t.Helper()
	agents, err := api.AgentList()
	if err != nil {
		t.Fatalf("agent.list: %v", err)
	}
	nameSet := map[string]bool{}
	for _, n := range names {
		nameSet[n] = true
	}
	var out []herdrapi.AgentInfo
	for _, a := range agents {
		if nameSet[a.Name] && a.Cwd == cwd {
			out = append(out, a)
		}
	}
	return out
}

// waitShimAgents は該当 agent が n 件になるまで poll する（agent.start 直後の
// agent.list は反映が遅延し得る実測＝cmd 側テストと同じ猶予設計）。
func waitShimAgents(t *testing.T, api *herdrapi.Client, cwd string, n int, names ...string) []herdrapi.AgentInfo {
	t.Helper()
	var last []herdrapi.AgentInfo
	waitFor(t, 10*time.Second, fmt.Sprintf("%d claude agents at %s", n, cwd), func() (bool, error) {
		last = shimAgentsAt(t, api, cwd, names...)
		return len(last) == n, fmt.Errorf("now %d agents", len(last))
	})
	return last
}

// ============ 1. 実 TTY: 新規起動 → attach 成立 → Ctrl+B q detach ============

func TestE2EClaudeShimRealTTYAttachDetach(t *testing.T) {
	srv, api := startHerdr(t, "HOME="+t.TempDir(), "XDG_STATE_HOME="+t.TempDir())
	bin := buildBinary(t)
	harness := writePtyHarness(t)
	work := physicalDir(t)
	const marker = "HD_E2E_TTY_MARK_ONE"
	stubDir := t.TempDir()
	installStubClaudeAt(t, stubDir, marker)
	env := shimEnv(srv, stubDir, work)

	// 候補なし cwd からシム実行 → 新規 agent.start → attach。attach が pane の
	// 実画面を pty へ届けた物証は「strip 後出力に stub marker が出現」。
	// marker を見たら Ctrl+B(\x02) q で detach（herdr の detach キー実測）。
	res := runPtyHarness(t, harness, work, env,
		[][2]string{{marker, `\x02q`}}, bin, "claude")

	if !strings.Contains(res.stripped, "claude セッションを新規起動しました") {
		t.Fatalf("新規起動経路を通っていない:\ncapture(strip):\n%s", res.stripped)
	}
	if !strings.Contains(res.stripped, marker) {
		t.Fatalf("attach 後の pty 出力に stub marker が無い（attach 不成立）:\n%s", res.stripped)
	}

	// detach≠kill: シム終了後も agent/pane は生存している（detach の物証）。
	// 反映遅延の猶予を置いてから見る（即読みだと消滅を見逃す偽陰性）。
	time.Sleep(1 * time.Second)
	ags := shimAgentsAt(t, api, work, "claude")
	if len(ags) != 1 {
		t.Fatalf("detach 後に agent が生存していない/増えている: %d 件 %+v", len(ags), ags)
	}
}

// ============ 2. 実 TTY: 同 cwd 2 候補 → picker で "2" → 2 番目へ attach ============

func TestE2EClaudeShimPickerSecondCandidate(t *testing.T) {
	srv, api := startHerdr(t, "HOME="+t.TempDir(), "XDG_STATE_HOME="+t.TempDir())
	bin := buildBinary(t)
	harness := writePtyHarness(t)
	work := physicalDir(t)

	// 各候補の marker を変えて「どの pane に attach したか」を判別可能にする。
	// stub は別 dir に置く（PATH 解決用 stub とは独立。argv は絶対パス指定）。
	const markA = "HD_E2E_PICK_MARK_A"
	const markB = "HD_E2E_PICK_MARK_B"
	stubA := installStubClaudeAt(t, t.TempDir(), markA)
	stubB := installStubClaudeAt(t, t.TempDir(), markB)
	agA, err := api.AgentStart("claude", []string{stubA}, &herdrapi.AgentStartOptions{Cwd: work})
	if err != nil {
		t.Fatalf("agent.start A: %v", err)
	}
	agB, err := api.AgentStart("claude-2", []string{stubB}, &herdrapi.AgentStartOptions{Cwd: work})
	if err != nil {
		t.Fatalf("agent.start B: %v", err)
	}
	markerByPane := map[string]string{agA.PaneID: markA, agB.PaneID: markB}
	waitShimAgents(t, api, work, 2, "claude", "claude-2")

	// シムの PATH 解決（lookupClaude）は候補有無に関わらず走る＝stub を PATH へ。
	env := shimEnv(srv, filepath.Dir(stubA), work)

	// picker プロンプトに "2\r"（pty slave の ICRNL で \n 化）→ 2 番目に
	// attach → marker（共通接頭辞で待つ。listing には出ない文字列）→ detach。
	res := runPtyHarness(t, harness, work, env, [][2]string{
		{"番号を選択", `2\r`},
		{"HD_E2E_PICK_MARK_", `\x02q`},
	}, bin, "claude")

	// 「2 番目」は表示された [2] 行から exact に読む（agent.list の並びは
	// 作成順でない実測＝並び推測はヒューリスティックになるので禁止）。
	m := regexp.MustCompile(`\[2\] (\S+) pane=(\S+) terminal=`).FindStringSubmatch(res.stripped)
	if m == nil {
		t.Fatalf("picker の [2] 行が capture に無い:\n%s", res.stripped)
	}
	secondPane := m[2]
	wantMark, ok := markerByPane[secondPane]
	if !ok {
		t.Fatalf("[2] の pane=%s がテスト作成の agent でない（listing 汚染?）:\n%s", secondPane, res.stripped)
	}
	otherMark := markA
	if wantMark == markA {
		otherMark = markB
	}
	if !strings.Contains(res.stripped, wantMark) {
		t.Fatalf("2 番目(%s) の marker %s が出力に無い:\n%s", secondPane, wantMark, res.stripped)
	}
	if strings.Contains(res.stripped, otherMark) {
		t.Fatalf("選んでいない候補の marker %s が出力に混入（attach 先誤り）:\n%s", otherMark, res.stripped)
	}

	// picker 経由の attach は新規を作らない（件数不変）。
	time.Sleep(1 * time.Second)
	if got := len(shimAgentsAt(t, api, work, "claude", "claude-2", "claude-3")); got != 2 {
		t.Fatalf("picker attach で件数が変わった: %d 件", got)
	}
}

// ============ 3. 非 TTY（pipe）スモーク: 非 attach 終了・dup 非生成 ============

func TestE2EClaudeShimNonTTYPipeSmoke(t *testing.T) {
	srv, api := startHerdr(t, "HOME="+t.TempDir(), "XDG_STATE_HOME="+t.TempDir())
	bin := buildBinary(t)
	work := physicalDir(t)
	stubDir := t.TempDir()
	installStubClaudeAt(t, stubDir, "HD_E2E_PIPE_MARK")
	env := shimEnv(srv, stubDir, work)

	// ⚠Stdin は必ず pipe を渡す。nil だと os/exec が /dev/null（char device）を
	// 与え、シムの stdinIsTTY が真＝attach を試みて panic する（実測）。
	// これは seam 差替の cmd テストでは踏めない「実 stdin 判定」の経路。
	runShim := func() (string, string) {
		c := exec.Command(bin, "claude")
		c.Dir = work
		c.Env = env
		c.Stdin = strings.NewReader("")
		var ob, eb strings.Builder
		c.Stdout, c.Stderr = &ob, &eb
		if err := c.Run(); err != nil {
			t.Fatalf("シム非 TTY 実行失敗: %v\nstdout=%s\nstderr=%s", err, ob.String(), eb.String())
		}
		return ob.String(), eb.String()
	}

	// 1 回目: 候補なし → 新規 pane を作るが attach はしない（pane_id 報告のみ）。
	out1, _ := runShim()
	if !strings.Contains(out1, "pane_id=") || !strings.Contains(out1, "terminal_id=") {
		t.Fatalf("非 TTY 出力に pane_id/terminal_id が無い:\n%s", out1)
	}
	if strings.Contains(out1, "attach します") {
		t.Fatalf("非 TTY なのに attach を試みた:\n%s", out1)
	}
	ags := waitShimAgents(t, api, work, 1, "claude")
	pane := ags[0].PaneID

	// 2 回目: 既存 1 件 → 同じ pane を報告して終了。dup を作らない backstop。
	out2, _ := runShim()
	if !strings.Contains(out2, "既存 claude セッションへ接続します") ||
		!strings.Contains(out2, "pane_id="+pane) {
		t.Fatalf("既存 pane (%s) の報告が無い:\n%s", pane, out2)
	}
	if strings.Contains(out2, "attach します") {
		t.Fatalf("非 TTY なのに attach を試みた:\n%s", out2)
	}
	// 「作られていない」ことは反映遅延の猶予後に件数で確認（即読みは偽陰性）。
	time.Sleep(1 * time.Second)
	if got := len(shimAgentsAt(t, api, work, "claude", "claude-2")); got != 1 {
		t.Fatalf("非 TTY 再実行で dup が作られた: %d 件", got)
	}
}
