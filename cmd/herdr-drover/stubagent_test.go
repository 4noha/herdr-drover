package main

// 偽エージェント（stub claude/codex/gemini 等）の OS 差を吸収する。
//
//   - unix: 従来どおり sh スクリプトをそのまま書く（バイト同一＝既存テストの挙動不変）。
//   - Windows: sh スクリプトは CreateProcessW で起動できず（os error 193）、拡張子
//     無しは exec.LookPath にも見つからない。そこで**このテストバイナリ自身**を
//     `<name>.exe` として複製し、隣に置いた spec（JSON）を TestMain が見つけたら
//     テストを走らせず stub として振る舞う。
//
// なぜテストバイナリの複製か（2026-10-17 実測・TestWindowsStubAgentIsRealAgent）:
//   - herdr 0.9.x のエージェント検出は前景プロセスの basename 基準＝`claude.exe`
//     という名前の実プロセスが前景に居続ける必要がある（cmd.exe 複製は argv が
//     cmd の解釈に化ける）。複製なら前景 argv＝launch argv のまま。
//   - Smart App Control は新規ビルドの未署名 exe を弾き得るが、go test が作る
//     テストバイナリは実行できている＝同一内容の複製も通る（別途 go build しない）。
//   - ハードリンクは不可: 実行中のテストバイナリと実体を共有するため、後で消せない。
//
// Windows 固有の後始末（実測）:
//   - 実行中の exe は消せない → exe は t.TempDir() ではなくパッケージ共通の
//     stubRoot に置き、全テスト終了後に TestMain が消す。
//   - 実行中プロセスの cwd も消せない。stub は cwd を動かせない（herdr は pane cwd を
//     **前景プロセスの実 cwd** から報告する＝chdir すると pane cwd が変わる・実測）。
//     pane の cwd に使った TempDir の削除順は startHerdrForTest が保証する（herdr 停止＋
//     孤児回収の後に削除）。それとは別に、t.Context()（Cleanup 直前に cancel）で stop
//     ファイルを置き、herdr 外で起動された分も含め全 stub インスタンスを自ら終了させる
//     （テスト後に 300 秒 sleep の stub が残らない）。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/4noha/herdr-drover/internal/agentid"
)

// stubReply は argv 条件に一致したときの応答（sh の case 節 1 個に相当）。
type stubReply struct {
	Stdout     string `json:"stdout,omitempty"`
	StdoutFile string `json:"stdout_file,omitempty"` // 内容を stdout へ（cat）＋改行
	Stderr     string `json:"stderr,omitempty"`
	WriteFile  string `json:"write_file,omitempty"` // WriteBody で上書き（printf > file）
	WriteBody  string `json:"write_body,omitempty"`
	Exit       int    `json:"exit"`
}

// stubSpec は Windows stub の振る舞い。評価順は sh 版に合わせて
// Stdout → AppendRun → AnyArg → Arg1 → Shell/Sleep。
type stubSpec struct {
	Stdout    string               `json:"stdout,omitempty"`     // 起動時に 1 行出力（echo）
	AppendRun string               `json:"append_run,omitempty"` // 起動ごとに "run" を 1 行追記
	AnyArg    map[string]stubReply `json:"any_arg,omitempty"`    // argv のどこかに一致（for a in "$@"）
	Arg1      map[string]stubReply `json:"arg1,omitempty"`       // $1 に一致（case "$1"）
	Shell     bool                 `json:"shell,omitempty"`      // 最後に素シェル（%ComSpec%）を子として起動して待つ（exec sh 相当）
	SleepSec  int                  `json:"sleep_sec,omitempty"`  // 最後に生存する秒数（sleep N）
	StopFile  string               `json:"stop_file,omitempty"`  // writeAgentStub が設定。出現したら即終了
}

const stubSpecSuffix = ".stubspec.json"

var stubRoot string

func TestMain(m *testing.M) {
	if code, ok := runAsStubIfConfigured(); ok {
		os.Exit(code)
	}
	if runtime.GOOS == "windows" {
		d, err := os.MkdirTemp("", "hdstub")
		if err != nil {
			fmt.Fprintln(os.Stderr, "stubRoot:", err)
			os.Exit(2)
		}
		stubRoot = d
	}
	code := m.Run()
	if stubRoot != "" {
		removeStubRoot(stubRoot)
	}
	os.Exit(code)
}

// removeStubRoot は stub の終了が僅かに遅れ得るので短く再試行する。消せなくても
// 結果は変えない（OS の temp 掃除に任せる）がログは出す。
func removeStubRoot(dir string) {
	var err error
	for i := 0; i < 20; i++ {
		if err = os.RemoveAll(dir); err == nil {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	fmt.Fprintf(os.Stderr, "stubRoot %s を消せなかった（残置）: %v\n", dir, err)
}

func stubSpecPathFor(exe string) string {
	return strings.TrimSuffix(exe, filepath.Ext(exe)) + stubSpecSuffix
}

// runAsStubIfConfigured は自分の隣に spec があれば stub として実行し終了コードを返す。
func runAsStubIfConfigured() (int, bool) {
	exe, err := os.Executable()
	if err != nil {
		return 0, false
	}
	b, err := os.ReadFile(stubSpecPathFor(exe))
	if err != nil {
		return 0, false
	}
	var s stubSpec
	if err := json.Unmarshal(b, &s); err != nil {
		fmt.Fprintln(os.Stderr, "stub spec:", err)
		return 2, true
	}
	return runStub(s, os.Args[1:], os.Stdout, os.Stderr), true
}

func runStub(s stubSpec, args []string, stdout, stderr io.Writer) int {
	if s.Stdout != "" {
		fmt.Fprintln(stdout, s.Stdout)
	}
	if s.AppendRun != "" {
		f, err := os.OpenFile(s.AppendRun, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			fmt.Fprintln(stderr, "append:", err)
			return 2
		}
		fmt.Fprintln(f, "run")
		f.Close()
	}
	reply := func(r stubReply) int {
		if r.Stdout != "" {
			fmt.Fprintln(stdout, r.Stdout)
		}
		if r.StdoutFile != "" {
			b, _ := os.ReadFile(r.StdoutFile)
			fmt.Fprintln(stdout, string(b))
		}
		if r.Stderr != "" {
			fmt.Fprintln(stderr, r.Stderr)
		}
		if r.WriteFile != "" {
			if err := os.WriteFile(r.WriteFile, []byte(r.WriteBody), 0o644); err != nil {
				fmt.Fprintln(stderr, "write:", err)
				return 2
			}
		}
		return r.Exit
	}
	for _, a := range args {
		if r, ok := s.AnyArg[a]; ok {
			return reply(r)
		}
	}
	if len(args) > 0 {
		if r, ok := s.Arg1[args[0]]; ok {
			return reply(r)
		}
	}

	stopped := make(chan struct{})
	if s.StopFile != "" {
		go func() {
			for {
				if _, err := os.Stat(s.StopFile); err == nil {
					close(stopped)
					return
				}
				time.Sleep(100 * time.Millisecond)
			}
		}()
	}
	if s.Shell {
		c := exec.Command(os.Getenv("ComSpec"))
		c.Stdin, c.Stdout, c.Stderr = os.Stdin, stdout, stderr
		if err := c.Start(); err != nil {
			fmt.Fprintln(stderr, "shell:", err)
			return 1
		}
		done := make(chan error, 1)
		go func() { done <- c.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				return 1
			}
			return 0
		case <-stopped:
			_ = c.Process.Kill() // 自分が起こした子だけ
			<-done
			return 0
		}
	}
	select {
	case <-time.After(time.Duration(s.SleepSec) * time.Second):
	case <-stopped:
	}
	return 0
}

var stubSeq atomic.Int64

// writeAgentStub は path に偽エージェントを作り、実際に起動すべきパスを返す。
//   - unix: unixScript を path へ 0755 で書いて path を返す（従来と同一）。
//   - Windows: stubRoot 配下の固有 dir に `<basename(path)>.exe`（テストバイナリの
//     複製）と spec を置き、その exe のパスを返す（dir は path と異なる＝上記理由）。
//     テスト終了時（Cleanup 直前）に全インスタンスを停止させる。
//
// 呼び出し側は**返り値**を argv/PATH に使うこと（path を直接使うと Windows で壊れる）。
func writeAgentStub(t *testing.T, path, unixScript string, win stubSpec) string {
	t.Helper()
	if runtime.GOOS != "windows" {
		if err := os.WriteFile(path, []byte(unixScript), 0o755); err != nil {
			t.Fatalf("stub 作成 %s: %v", path, err)
		}
		return path
	}
	if stubRoot == "" {
		t.Fatalf("stubRoot 未設定（TestMain 経由で走っていない）")
	}
	dir := filepath.Join(stubRoot, fmt.Sprintf("s%d", stubSeq.Add(1)))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("stub dir: %v", err)
	}
	exe := filepath.Join(dir, filepath.Base(path)+".exe")
	if err := copySelfExecutable(exe); err != nil {
		t.Fatalf("stub exe 複製: %v", err)
	}
	win.StopFile = exe + ".stop"
	b, err := json.Marshal(win)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stubSpecPathFor(exe), b, 0o644); err != nil {
		t.Fatalf("stub spec: %v", err)
	}
	stopOnTestEnd(t.Context(), win.StopFile)
	return exe
}

// stopOnTestEnd は ctx（t.Context＝Cleanup の直前に cancel）終了で stop ファイルを置く。
func stopOnTestEnd(ctx context.Context, stopFile string) {
	go func() {
		<-ctx.Done()
		_ = os.WriteFile(stopFile, nil, 0o644)
	}()
}

// copySelfExecutable はテストバイナリを dst へ複製する。
func copySelfExecutable(dst string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	in, err := os.Open(self)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// sleepStub は「生存し続けるだけ」の stub（unixScript はテスト毎に従来の文面を渡す）。
func sleepStub(t *testing.T, path, unixScript string, sec int) string {
	t.Helper()
	return writeAgentStub(t, path, unixScript, stubSpec{SleepSec: sec})
}

// sleeperArgv は「エージェントではない」生存プロセスの argv（unix は従来どおり
// /bin/sleep）。Windows は basename が sleep の stub＝herdr の agent 表に無い名前。
func sleeperArgv(t *testing.T, sec int) []string {
	t.Helper()
	if runtime.GOOS != "windows" {
		return []string{"/bin/sleep", fmt.Sprint(sec)}
	}
	return []string{sleepStub(t, filepath.Join(t.TempDir(), "sleep"), "", sec)}
}

// wrapperArgv は「シェル経由で stub を起動する」argv（argv[0] がエージェントでない
// pane の再現）。unix は従来どおり /bin/sh -c、Windows は %ComSpec% /c。
func wrapperArgv(stub string) []string {
	if runtime.GOOS != "windows" {
		return []string{"/bin/sh", "-c", stub}
	}
	return []string{os.Getenv("ComSpec"), "/c", stub}
}

// hideRealAgentBins は PATH から**実エージェント**（InstallSpec の BinNames）を含む
// dir を外す。update 系テストは pane の argv[0]（stub）を権威に更新を走らせるが、
// 解決が PATH に退行すると開発機の**本物の `claude update`** を実行してしまう
// （この Windows 機は ~/.local/bin/claude.exe が PATH にある＝実測）。外した dir は
// ログに出す（silent に環境を変えない）。stub の dir は呼び手が後から足す。
func hideRealAgentBins(t *testing.T) {
	t.Helper()
	var names []string
	for label := range agentid.CanonicalLabels {
		if sp, ok := agentid.Install(label); ok {
			names = append(names, sp.BinNames...)
		}
	}
	exts := []string{""}
	if runtime.GOOS == "windows" {
		exts = append(exts, ".exe", ".cmd", ".bat", ".ps1")
	}
	var keep []string
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		hit := ""
		for _, n := range names {
			for _, e := range exts {
				if fi, err := os.Stat(filepath.Join(dir, n+e)); err == nil && !fi.IsDir() {
					hit = n + e
				}
			}
		}
		if hit != "" {
			t.Logf("PATH から実エージェントの dir を除外: %s（%s）", dir, hit)
			continue
		}
		keep = append(keep, dir)
	}
	t.Setenv("PATH", strings.Join(keep, string(os.PathListSeparator)))
}

// prependPATH は dir を PATH 先頭へ足す。
func prependPATH(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}
