package main

// memvault status / whoami の実 daemon 検証（鉄則②: 合成で緑にしない）。
// harness は status_test.go の startHerdrForTest と同じ流儀:
//   - 短い /tmp dir 必須（UNIX socket の sun_path 104B 制約）
//   - **`--socket` を必ず明示**する。省略すると memvault は
//     $MEMVAULT_SOCKET（既定 $HOME/.memvault.sock）を back-compat の
//     catch-all として併せて listen し、**本番運用中の daemon の socket を
//     奪う**（実測。DESIGN_MEMVAULT.md §5.4(b)）。テストが実 HOME を触るのは
//     恒久禁止なので、3 本すべて TempDir 配下に置く。
//   - 停止は自分が spawn した PID のみ（裸の pkill は恒久禁止）
//
// memvault が入っていない環境（大半の CI / owner Mac）では Skip する。

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/4noha/herdr-drover/internal/memvaultclient"
)

// mvSockets は 1 daemon 分の socket 3 本（legacy/ctrl/use）。
type mvSockets struct {
	legacy string
	ctrl   string
	use    string
}

// startMemvaultForTest は隔離 memvault daemon を起動し socket 群を返す。
func startMemvaultForTest(t *testing.T) mvSockets {
	t.Helper()
	bin, err := exec.LookPath("memvault")
	if err != nil {
		// 開発機では ~/works/tools/memvault をビルドした /tmp/mv-test を
		// 使う運用もあるので、環境変数での指定も許す。
		if p := os.Getenv("MEMVAULT_TEST_BIN"); p != "" {
			bin = p
		} else {
			t.Skip("memvault not installed; skipping real-daemon test (set MEMVAULT_TEST_BIN to override)")
		}
	}
	dir, err := os.MkdirTemp("/tmp", "mv")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	s := mvSockets{
		legacy: filepath.Join(dir, "l.sock"),
		ctrl:   filepath.Join(dir, "c.sock"),
		use:    filepath.Join(dir, "u.sock"),
	}
	cmd := exec.Command(bin, "serve",
		"--socket", s.legacy, // ⚠省略禁止（上記コメント参照）
		"--ctrl-socket", s.ctrl,
		"--use-socket", s.use,
		"--ttl", "10m", "--hard-cap", "20m")
	var logBuf bytes.Buffer
	cmd.Stdout = &logBuf
	cmd.Stderr = &logBuf
	if err := cmd.Start(); err != nil {
		t.Fatalf("start memvault serve: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt) // 自分が spawn した PID のみ
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		os.RemoveAll(dir)
		if t.Failed() {
			t.Logf("memvault daemon log:\n%s", logBuf.String())
		}
	})
	// ctrl socket が繋がるまで待つ（実測 ~1s。余裕 15s）
	deadline := time.Now().Add(15 * time.Second)
	for {
		conn, derr := net.Dial("unix", s.ctrl)
		if derr == nil {
			conn.Close()
			return s
		}
		if time.Now().After(deadline) {
			t.Fatalf("memvault daemon did not become ready at %s\nlog:\n%s", s.ctrl, logBuf.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// mvInject は operator laptop 役として材料を daemon へ流し込む。
// テスト用のダミー値のみ（実鍵は絶対に使わない）。
func mvInject(t *testing.T, s mvSockets, kind, payload string, extraArgs ...string) {
	t.Helper()
	bin, err := exec.LookPath("memvault")
	if err != nil {
		bin = os.Getenv("MEMVAULT_TEST_BIN")
	}
	args := append([]string{"inject", kind}, extraArgs...)
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "MEMVAULT_SOCKET="+s.ctrl)
	cmd.Stdin = strings.NewReader(payload)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("memvault inject %s: %v\n%s", kind, err, out)
	}
}

// ===================== ① /status のキー欠落 =====================

// TestMemvaultStatusShowsEveryDaemonField は「daemon が返したキーが 1 つも
// 落ちない」ことを実 daemon 相手に検証する。
//
// 旧コードでの FAIL 確認（鉄則②）: pretty-print が typed struct
// (json.MarshalIndent(st,...)) だった版では、struct が宣言していない
// git_loaded / git_hosts / github_app_loaded / kind_ttl_remain_sec / routes
// が silent に落ち、本テストは
//
//	status 出力に daemon の返したキー git_loaded が無い（silent に落ちている）
//
// で FAIL する（実測）。
func TestMemvaultStatusShowsEveryDaemonField(t *testing.T) {
	s := startMemvaultForTest(t)
	t.Setenv("MEMVAULT_CTRL_SOCKET", s.ctrl)
	t.Setenv("MEMVAULT_USE_SOCKET", s.use)
	os.Unsetenv("MEMVAULT_SOCKET")

	// git kind を入れて git_loaded=true / git_hosts 非空を作る（GitHub 連携の
	// 状態が status に出るかが本題なので、この kind でないと意味がない）。
	mvInject(t, s, "git", `{"github.com":"TEST_DUMMY_TOKEN_NOT_REAL"}`)

	// daemon の生の応答（＝期待値の権威）
	c := memvaultclient.New(s.ctrl)
	st, err := c.Status()
	if err != nil {
		t.Fatalf("Status(): %v", err)
	}
	if len(st.Raw) == 0 {
		t.Fatal("Status().Raw が空＝raw 保持が効いていない")
	}

	var stdout, stderr bytes.Buffer
	if err := memvaultStatus(nil, &stdout, &stderr); err != nil {
		t.Fatalf("memvaultStatus: %v", err)
	}
	var shown map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &shown); err != nil {
		t.Fatalf("status 出力が JSON でない: %v\n%s", err, stdout.String())
	}
	for k := range st.Raw {
		if _, ok := shown[k]; !ok {
			t.Errorf("status 出力に daemon の返したキー %s が無い（silent に落ちている）", k)
		}
	}
	// GitHub 連携の状態が top-level に出ることを名指しで固定する（これが
	// 元の実害。将来 struct 表示に戻したら必ず落ちる）。
	for _, k := range []string{"git_loaded", "github_app_loaded", "kind_ttl_remain_sec"} {
		if _, ok := shown[k]; !ok {
			t.Errorf("top-level に %s が出ていない", k)
		}
	}
	if v, ok := shown["git_loaded"].(bool); !ok || !v {
		t.Errorf("git_loaded=true を期待したが %v", shown["git_loaded"])
	}
}

// TestStatusRawKeepsUnknownFields は struct が知らないキーも Raw に残ることを
// 実 daemon の応答で確かめる（①の再発防止の芯）。
func TestStatusRawKeepsUnknownFields(t *testing.T) {
	s := startMemvaultForTest(t)
	c := memvaultclient.New(s.ctrl)
	st, err := c.Status()
	if err != nil {
		t.Fatalf("Status(): %v", err)
	}
	// struct に無い＝以前落ちていたキー群。daemon 側が古くて持たない場合は
	// 判定できないので、1 つも無いときだけ Skip する（silent 緑を避ける）。
	want := []string{"git_loaded", "git_hosts", "github_app_loaded", "kind_ttl_remain_sec", "routes"}
	found := 0
	for _, k := range want {
		if _, ok := st.Raw[k]; ok {
			found++
		}
	}
	if found == 0 {
		t.Skip("この daemon は git/github_app/kind_ttl を /status に出さない旧版")
	}
	if found != len(want) {
		t.Errorf("Raw に %d/%d キーしか無い: %v", found, len(want), st.Raw)
	}
}

// ===================== ② slot ズレの警告 =====================

// TestStatusWarnsOnSlotMismatch は「claim 済みなのに --owner 無しで inject
// した」実際の踏み方を再現し、警告が出ることを検証する。
//
// 旧コードでの FAIL 確認（鉄則②）: 警告実装前は stderr が空で
//
//	slot ズレの警告が stderr に出ていない
//
// で FAIL する（実測）。
func TestStatusWarnsOnSlotMismatch(t *testing.T) {
	s := startMemvaultForTest(t)
	t.Setenv("MEMVAULT_CTRL_SOCKET", s.ctrl)
	os.Unsetenv("MEMVAULT_SOCKET")

	// default slot に材料を入れる（--owner を付け忘れた状態＝実際の踏み方）
	mvInject(t, s, "git", `{"github.com":"TEST_DUMMY_TOKEN_NOT_REAL"}`)
	// alice が claim ＝参照は alice の slot（空）へ向く
	t.Setenv("MEMVAULT_OPERATOR", "alice")
	var cs, ce bytes.Buffer
	if err := memvaultClaim(nil, &cs, &ce); err != nil {
		t.Fatalf("claim: %v\n%s", err, ce.String())
	}

	var stdout, stderr bytes.Buffer
	if err := memvaultStatus(nil, &stdout, &stderr); err != nil {
		t.Fatalf("memvaultStatus: %v", err)
	}
	if !strings.Contains(stderr.String(), "slot ズレ") {
		t.Errorf("slot ズレの警告が stderr に出ていない。stderr=%q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "git") {
		t.Errorf("警告に「どの材料が default 側にあるか」が無い: %q", stderr.String())
	}
	// stdout は素の JSON のまま（警告を混ぜて機械可読性を壊さない）
	var shown map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &shown); err != nil {
		t.Errorf("stdout が JSON として壊れた: %v\n%s", err, stdout.String())
	}

	// whoami でも同じ警告が出る（「今どの slot か」を答えるコマンドだから）
	var ws, we bytes.Buffer
	if err := memvaultWhoami(nil, &ws, &we); err != nil {
		t.Fatalf("whoami: %v", err)
	}
	if !strings.Contains(we.String(), "slot ズレ") {
		t.Errorf("whoami で警告が出ていない。stderr=%q", we.String())
	}
	if !strings.Contains(ws.String(), "alice") {
		t.Errorf("whoami の stdout に active operator が無い: %q", ws.String())
	}
}

// TestStatusNoWarningWhenSlotsAgree は誤検知しないことの対照実験。
// 警告が「常に出る」実装でも ↑ のテストは緑になってしまうので、これが要る。
func TestStatusNoWarningWhenSlotsAgree(t *testing.T) {
	s := startMemvaultForTest(t)
	t.Setenv("MEMVAULT_CTRL_SOCKET", s.ctrl)
	os.Unsetenv("MEMVAULT_SOCKET")

	// (a) claim せず default slot だけ使う＝ズレようがない
	mvInject(t, s, "git", `{"github.com":"TEST_DUMMY_TOKEN_NOT_REAL"}`)
	var so, se bytes.Buffer
	if err := memvaultStatus(nil, &so, &se); err != nil {
		t.Fatalf("memvaultStatus: %v", err)
	}
	if strings.Contains(se.String(), "slot ズレ") {
		t.Errorf("active operator が居ないのに警告が出た: %q", se.String())
	}

	// (b) claim した上で **その slot に** inject ＝正しい運用。警告は出ない。
	t.Setenv("MEMVAULT_OPERATOR", "bob")
	var cs, ce bytes.Buffer
	if err := memvaultClaim(nil, &cs, &ce); err != nil {
		t.Fatalf("claim: %v\n%s", err, ce.String())
	}
	mvInject(t, s, "git", `{"github.com":"TEST_DUMMY_TOKEN_NOT_REAL"}`, "--owner", "bob")
	var so2, se2 bytes.Buffer
	if err := memvaultStatus(nil, &so2, &se2); err != nil {
		t.Fatalf("memvaultStatus: %v", err)
	}
	if strings.Contains(se2.String(), "slot ズレ") {
		t.Errorf("正しく --owner 付きで inject したのに警告が出た: %q", se2.String())
	}
}

// TestSlotMismatchWarningSilentCases は警告してはいけない状態を固定する。
//
// 「slots にエントリが無い」は "空" 扱いであって "不明" ではない（memvault は
// slot を lazy 生成し、生成直後は空。実測: claim は slot を生成しないので
// claim 直後の status.slots に active slot のキーは無いが、参照はもう 404）。
// 一方 slots オブジェクト自体が無い応答＝multi-owner 前の daemon は本当に
// 判定不能なので黙る（鉄則③）。
func TestSlotMismatchWarningSilentCases(t *testing.T) {
	cases := []struct {
		name string
		st   *memvaultclient.Status
	}{
		{"nil", nil},
		{"active 空（default slot を見ている）", &memvaultclient.Status{ActiveSlot: ""}},
		{"slots 自体が無い＝multi-owner 前の daemon", &memvaultclient.Status{ActiveSlot: "alice"}},
		{"default が空なので警告する材料が無い", &memvaultclient.Status{
			ActiveSlot: "alice",
			Slots:      map[string]map[string]any{"alice": {"git_loaded": false}},
		}},
		{"active に材料がある＝正常運用", &memvaultclient.Status{
			ActiveSlot: "alice",
			Slots: map[string]map[string]any{
				"":      {"git_loaded": true},
				"alice": {"git_loaded": true},
			},
		}},
		{"どちらも空＝ズレていない", &memvaultclient.Status{
			ActiveSlot: "alice",
			Slots: map[string]map[string]any{
				"":      {"git_loaded": false},
				"alice": {"git_loaded": false},
			},
		}},
	}
	for _, tc := range cases {
		if w := slotMismatchWarning(tc.st); w != "" {
			t.Errorf("%s: 警告してはいけないのに出た: %q", tc.name, w)
		}
	}
}

// TestSlotMismatchWarningDetects は確定的にズレている状態で必ず警告することを
// 固定する。2 例目（active slot のキー自体が無い）が claim 直後の実 daemon の
// 形＝ここを黙らせると実害が検出できない。
func TestSlotMismatchWarningDetects(t *testing.T) {
	cases := []*memvaultclient.Status{
		{
			ActiveSlot: "alice",
			Slots: map[string]map[string]any{
				"":      {"git_loaded": true, "aws_loaded": true},
				"alice": {"git_loaded": false, "aws_loaded": false},
			},
		},
		{
			// claim 直後の実形: alice のキーがまだ生えていない
			ActiveSlot: "alice",
			Slots: map[string]map[string]any{
				"": {"git_loaded": true, "aws_loaded": true},
			},
		},
	}
	for i, st := range cases {
		w := slotMismatchWarning(st)
		if w == "" {
			t.Fatalf("case %d: 確定的にズレているのに警告が出ない", i)
		}
		for _, want := range []string{"alice", "git", "aws", "release"} {
			if !strings.Contains(w, want) {
				t.Errorf("case %d: 警告文に %q が無い: %q", i, want, w)
			}
		}
	}
}

// TestSlotLoadedKindsIgnoresUnknownKindGracefully は daemon が新しい kind を
// 足したときに壊れないこと、および古い daemon（キーが無い）を「false」と
// 誤認しないことを固定する。
func TestSlotLoadedKindsIgnoresUnknownKindGracefully(t *testing.T) {
	st := &memvaultclient.Status{Slots: map[string]map[string]any{
		"": {
			"git_loaded":         true,
			"gcp_loaded":         false,
			"future_kind_loaded": true, // 未知 kind＝無視される（落ちない）
			"github_app_loaded":  true,
		},
	}}
	kinds, ok := st.SlotLoadedKinds("")
	if !ok {
		t.Fatal("slot オブジェクトがあるのに ok=false")
	}
	got := strings.Join(kinds, ",")
	if got != "git,github_app" {
		t.Errorf("kinds=%q（git,github_app を期待）", got)
	}
	// 存在しない slot は「空」(ok=true, kinds 空)。memvault は slot を lazy 生成
	// するので、キー不在＝まだ何も入っていない、で確定する。
	nk, ok := st.SlotLoadedKinds("nobody")
	if !ok {
		t.Error("slots がある応答で未生成 slot が ok=false になった（＝不明扱い）")
	}
	if len(nk) != 0 {
		t.Errorf("未生成 slot が材料を持っている扱いになった: %v", nk)
	}
	// slots オブジェクト自体が無い応答だけが「不明」
	if _, ok := (&memvaultclient.Status{}).SlotLoadedKinds(""); ok {
		t.Error("slots 自体が無いのに ok=true（multi-owner 前の daemon を誤判定）")
	}
}
