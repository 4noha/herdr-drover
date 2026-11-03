//go:build !windows

// attach_visibility_e2e_test.go — ↗窓の「表示判定」（attachvis.go）と「PC ごとの relay 枠」
// （attachsid.go・BUG-16）の通し試験。
//
// 構図は実運用と同じ 2 台構成: source 側の herdr（実 agent が動く）と、viewer 側の
// **別の** herdr（↗窓 pane で本物の `herdr-drover attach` が動く）。間は実 relay
// （drover-cloud をローカル起動）＋ Firestore エミュレータ。合成ストリーム無し。
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/4noha/drover-cloud/state"
	"github.com/4noha/herdr-drover/internal/herdrapi"
)

type sourceRig struct {
	srv      *testServer
	hc       *herdrapi.Client
	relayURL string
	bin      string
	st       *state.Client
	pcID     string
	paneID   string // source のセッション pane（sid）
}

// startSourceRig は source 側（agent が動く PC）を立てる。
func startSourceRig(t *testing.T, pcID string) *sourceRig {
	t.Helper()
	if os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
		t.Skip("SKIP: gcloud / Java 不在のため Firestore emulator 検証不可")
	}
	requireBridge(t)
	srv, hc := startHerdr(t)
	relayURL := startLocalRelay(t, droverCloudRoot(t))
	bin := buildBinary(t)
	st, err := state.New(context.Background(), projectID, "e2e-vis-observer")
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	agent := exec.Command(bin, "agent")
	agent.Env = []string{
		"HOME=" + t.TempDir(),
		"PATH=" + os.Getenv("PATH"),
		"GCP_PROJECT=" + projectID,
		"PC_ID=" + pcID,
		"DROVER_TICK=1s",
		"FIRESTORE_EMULATOR_HOST=" + os.Getenv("FIRESTORE_EMULATOR_HOST"),
		"HERDR_SOCKET_PATH=" + srv.sock,
		"CLOUD_RELAY_URL=" + relayURL,
	}
	var agentOut syncBuf
	agent.Stderr, agent.Stdout = &agentOut, &agentOut
	if err := agent.Start(); err != nil {
		t.Fatalf("agent start: %v", err)
	}
	t.Cleanup(func() { _ = agent.Process.Kill(); _ = agent.Wait() })
	waitFor(t, 20*time.Second, "pcs registered", func() (bool, error) {
		pcs, err := st.ListPCs(context.Background())
		if err != nil {
			return false, err
		}
		for _, p := range pcs {
			if p == pcID {
				return true, nil
			}
		}
		return false, fmt.Errorf("pcs=%v", pcs)
	})
	ws, err := hc.WorkspaceCreate()
	if err != nil {
		t.Fatalf("workspace.create: %v", err)
	}
	t.Cleanup(func() { _ = hc.WorkspaceClose(ws.Workspace.WorkspaceID) })
	return &sourceRig{srv: srv, hc: hc, relayURL: relayURL, bin: bin, st: st, pcID: pcID, paneID: ws.RootPane.PaneID}
}

// tick は source pane で 3 秒ごとに `<prefix>_<n>` を出し続ける（idle にしない＝
// quiescence の 30s 自切断を起こさず、画面が進むかで「流れているか」を見る）。
func (r *sourceRig) tick(t *testing.T, prefix string) {
	t.Helper()
	cmd := "i=0; while :; do i=$((i+1)); echo " + prefix + "_$i; sleep 3; done\r"
	if err := r.hc.PaneSendText(r.paneID, cmd); err != nil {
		t.Fatalf("source tick: %v", err)
	}
}

var reTick = regexp.MustCompile(`_(\d+)\b`)

// maxTick は画面テキスト中の `<prefix>_<n>` の最大 n（無ければ 0）。
func maxTick(text, prefix string) int {
	best := 0
	for _, l := range strings.Split(text, "\n") {
		i := strings.LastIndex(l, prefix+"_")
		if i < 0 {
			continue
		}
		if m := reTick.FindStringSubmatch(l[i+len(prefix):]); m != nil {
			n, _ := strconv.Atoi(m[1])
			if n > best {
				best = n
			}
		}
	}
	return best
}

func (r *sourceRig) sourceTick(t *testing.T, prefix string) int {
	t.Helper()
	rd, err := r.hc.PaneRead(r.paneID, "visible")
	if err != nil {
		t.Fatalf("source pane.read: %v", err)
	}
	return maxTick(rd.Text, prefix)
}

// viewer は viewer 側 herdr の pane で動く本物の attach。
type viewer struct {
	hc     *herdrapi.Client
	paneID string
	home   string
}

func (v *viewer) log() string {
	b, _ := os.ReadFile(filepath.Join(v.home, ".herdr-drover", "attach.log"))
	return string(b)
}

func (v *viewer) screen(t *testing.T) string {
	t.Helper()
	rd, err := v.hc.PaneRead(v.paneID, "visible")
	if err != nil {
		return ""
	}
	return rd.Text
}

// startViewer は viewer herdr の pane（シェル）で attach を起動する。HERDR_PANE_ID /
// HERDR_SOCKET_PATH は viewer herdr が pane 環境に入れる値をそのまま使う（表示判定は
// それで自 pane を知る）。
func startViewer(t *testing.T, src *sourceRig, vhc *herdrapi.Client, paneID, viewerPC string) *viewer {
	t.Helper()
	home := t.TempDir()
	cmd := fmt.Sprintf("env HOME=%s GCP_PROJECT=%s PC_ID=%s CLOUD_RELAY_URL=%s FIRESTORE_EMULATOR_HOST=%s %s attach %s %s\r",
		home, projectID, viewerPC, src.relayURL, os.Getenv("FIRESTORE_EMULATOR_HOST"), src.bin, src.pcID, src.paneID)
	if err := vhc.PaneSendText(paneID, cmd); err != nil {
		t.Fatalf("attach 起動: %v", err)
	}
	return &viewer{hc: vhc, paneID: paneID, home: home}
}

// waitTick は viewer の画面に `<prefix>_<n>`（n ≥ want）が出るまで待つ。
func waitTick(t *testing.T, v *viewer, prefix string, want int, timeout time.Duration, what string) {
	t.Helper()
	waitFor(t, timeout, what, func() (bool, error) {
		s := v.screen(t)
		return maxTick(s, prefix) >= want, fmt.Errorf("want %s_%d; screen:\n%s\nattach.log:\n%s", prefix, want, s, v.log())
	})
}

// cycleLines は attach.log に書かれた「接続が終わった」行の数（接続中は書かれない）。
func cycleLines(log string) int {
	n := 0
	for _, l := range strings.Split(log, "\n") {
		if strings.Contains(l, " dial ") && strings.Contains(l, "→ 接続=") {
			n++
		}
	}
	return n
}

// tabCreate は同じ Workspace に非フォーカスの Tab を足し、その tab_id を返す。
func tabCreate(t *testing.T, hc *herdrapi.Client, workspaceID string) string {
	t.Helper()
	raw, err := hc.Call("tab.create", map[string]any{"workspace_id": workspaceID, "focus": false})
	if err != nil {
		t.Fatalf("tab.create: %v", err)
	}
	var r struct {
		TabID string `json:"tab_id"`
		Tab   struct {
			TabID string `json:"tab_id"`
		} `json:"tab"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("tab.create decode: %v (%s)", err, raw)
	}
	if r.TabID != "" {
		return r.TabID
	}
	if r.Tab.TabID == "" {
		t.Fatalf("tab.create の応答に tab_id が無い: %s", raw)
	}
	return r.Tab.TabID
}

func tabFocus(t *testing.T, hc *herdrapi.Client, tabID string) {
	t.Helper()
	if _, err := hc.Call("tab.focus", map[string]any{"tab_id": tabID}); err != nil {
		t.Fatalf("tab.focus %s: %v", tabID, err)
	}
}

// 隠れた Tab の ↗窓は relay に繋がず、表示に戻ると再接続して最新画面を描く。
func TestE2EAttachHiddenTabDisconnects(t *testing.T) {
	src := startSourceRig(t, "e2e-vis-src-herdr")
	src.tick(t, "VIS")

	_, vhc := startHerdr(t) // viewer 側の別 herdr
	ws, err := vhc.WorkspaceCreate()
	if err != nil {
		t.Fatalf("viewer workspace.create: %v", err)
	}
	t.Cleanup(func() { _ = vhc.WorkspaceClose(ws.Workspace.WorkspaceID) })
	attachTab := ws.Tab.TabID
	otherTab := tabCreate(t, vhc, ws.Workspace.WorkspaceID)

	v := startViewer(t, src, vhc, ws.RootPane.PaneID, "e2e-vis-viewer-a")
	waitTick(t, v, "VIS", 1, 40*time.Second, "↗窓に source の画面が出る")

	// 別の Tab をアクティブに＝↗窓は非表示。attach は自分から切る。
	tabFocus(t, vhc, otherTab)
	waitFor(t, 15*time.Second, "非表示で切断", func() (bool, error) {
		return strings.Contains(v.log(), "非表示で切断"), fmt.Errorf("attach.log:\n%s", v.log())
	})
	// 非表示の間、source は進むが ↗窓の画面は進まない（流れてこない）。
	frozen := maxTick(v.screen(t), "VIS")
	srcBefore := src.sourceTick(t, "VIS")
	time.Sleep(10 * time.Second)
	if now := maxTick(v.screen(t), "VIS"); now > frozen {
		t.Fatalf("非表示の ↗窓にフレームが流れている（%d → %d）:\nattach.log:\n%s", frozen, now, v.log())
	}
	if after := src.sourceTick(t, "VIS"); after <= srcBefore {
		t.Fatalf("前提が崩れている: source が進んでいない（%d → %d）", srcBefore, after)
	}
	if n := cycleLines(v.log()); n != 1 {
		t.Fatalf("非表示中に再接続している（接続終了 %d 回・1 のはず）:\n%s", n, v.log())
	}

	// 表示に戻す＝再接続して最新画面に追いつく。
	tabFocus(t, vhc, attachTab)
	waitTick(t, v, "VIS", src.sourceTick(t, "VIS"), 30*time.Second, "表示に戻った ↗窓が最新画面に追いつく")
}

// 同じリモートセッションを 2 台の PC が同時に見ても、relay の枠を奪い合わない（BUG-16）。
// 旧コードは `<sid>#inj` を共有し、約 32 秒周期で互いに切断し続けた（relay ログで確認）。
func TestE2EAttachTwoViewersNoContention(t *testing.T) {
	src := startSourceRig(t, "e2e-vis-src2-herdr")
	src.tick(t, "VIS2")

	_, vhc := startHerdr(t)
	var vs []*viewer
	for _, pc := range []string{"e2e-vis-viewer-a", "e2e-vis-viewer-b"} {
		ws, err := vhc.WorkspaceCreate() // Workspace ごとにアクティブ Tab＝両方「表示中」
		if err != nil {
			t.Fatalf("viewer workspace.create: %v", err)
		}
		t.Cleanup(func() { _ = vhc.WorkspaceClose(ws.Workspace.WorkspaceID) })
		vs = append(vs, startViewer(t, src, vhc, ws.RootPane.PaneID, pc))
	}
	for i, v := range vs {
		waitTick(t, v, "VIS2", 1, 40*time.Second, fmt.Sprintf("viewer %d に source の画面", i))
	}
	for i, v := range vs {
		if !strings.Contains(v.log(), "#inj@e2e-vis-viewer-") {
			t.Fatalf("viewer %d が PC ごとの relay sid を使っていない:\n%s", i, v.log())
		}
	}
	// 奪い合いは約 32 秒周期（奪われた側の backoff 上限 30s）で起きる。source は動き続けて
	// いるので quiescence（無通信 30s）の自切断も起きない＝接続が終わる理由は無いはず。
	time.Sleep(45 * time.Second)
	for i, v := range vs {
		if n := cycleLines(v.log()); n != 0 {
			t.Fatalf("viewer %d の接続が切れた（接続終了 %d 回・0 のはず）:\n%s", i, n, v.log())
		}
	}
	want := src.sourceTick(t, "VIS2")
	for i, v := range vs {
		waitTick(t, v, "VIS2", want, 20*time.Second, fmt.Sprintf("viewer %d が最新画面に追いつく", i))
	}
}
