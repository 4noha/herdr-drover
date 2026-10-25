package herdrapi

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ============ RenameWithDetectWait（純関数・BUG-13） ============
//
// ⚠これが落ちると herdr 0.9.x で claude シム・restart-agent-session・update-all が
// pane 生成直後の agent.rename（agent_not_found）で必ず失敗する。

func apiErr(code string) error { return &APIError{Code: code, Message: code} }

func TestRenameWithDetectWaitRetriesUntilDetected(t *testing.T) {
	calls := 0
	call := func(pane, name string) (json.RawMessage, error) {
		calls++
		if calls < 4 { // 0.9.5 実測: 数回 agent_not_found の後に通る
			return nil, apiErr("agent_not_found")
		}
		return json.RawMessage(`{"agent":{"name":"claude"}}`), nil
	}
	raw, waited, err := RenameWithDetectWait(call, "w1:p2", "claude", 2*time.Second, 5*time.Millisecond)
	if err != nil {
		t.Fatalf("検出後に通るはずが失敗: %v", err)
	}
	if calls != 4 {
		t.Fatalf("calls=%d want 4（検出されるまでリトライ）", calls)
	}
	if waited <= 0 {
		t.Fatalf("待ち時間が記録されていない: %v（silent にしない）", waited)
	}
	if !strings.Contains(string(raw), "claude") {
		t.Fatalf("応答が返っていない: %s", raw)
	}
}

func TestRenameWithDetectWaitFirstTrySucceeds(t *testing.T) {
	calls := 0
	call := func(string, string) (json.RawMessage, error) {
		calls++
		return json.RawMessage(`{}`), nil
	}
	if _, _, err := RenameWithDetectWait(call, "p", "n", time.Second, time.Millisecond); err != nil || calls != 1 {
		t.Fatalf("0.7.4 相当（即検出）は 1 発で通るべき: calls=%d err=%v", calls, err)
	}
}

// 他のエラーは待たずに即返す（agent_name_taken は呼び手が次の採番へ進む契約）。
func TestRenameWithDetectWaitDoesNotRetryOtherErrors(t *testing.T) {
	for _, code := range []string{"agent_name_taken", "invalid_request", "pane_not_found"} {
		calls := 0
		call := func(string, string) (json.RawMessage, error) {
			calls++
			return nil, apiErr(code)
		}
		start := time.Now()
		_, _, err := RenameWithDetectWait(call, "p", "n", 5*time.Second, 50*time.Millisecond)
		var ae *APIError
		if !errors.As(err, &ae) || ae.Code != code {
			t.Fatalf("%s: コードが保たれていない（呼び手が exact-match で分岐できない）: %v", code, err)
		}
		if calls != 1 || time.Since(start) > time.Second {
			t.Fatalf("%s: リトライしてはいけない: calls=%d elapsed=%v", code, calls, time.Since(start))
		}
	}
}

// 永遠に検出されない pane（そもそも agent ではない）を無限に待たない。
func TestRenameWithDetectWaitGivesUpAtTimeout(t *testing.T) {
	calls := 0
	call := func(string, string) (json.RawMessage, error) {
		calls++
		return nil, apiErr("agent_not_found")
	}
	start := time.Now()
	_, _, err := RenameWithDetectWait(call, "w1:p9", "claude", 120*time.Millisecond, 10*time.Millisecond)
	if err == nil {
		t.Fatal("検出されないのに成功扱い")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("上限を超えて待っている: %v", elapsed)
	}
	var ae *APIError
	if !errors.As(err, &ae) || ae.Code != "agent_not_found" {
		t.Fatalf("最後の agent_not_found が包まれて残っていない: %v", err)
	}
	if !strings.Contains(err.Error(), "w1:p9") {
		t.Fatalf("エラーに pane が無い（調査できない）: %v", err)
	}
	if calls < 3 {
		t.Fatalf("リトライしていない: calls=%d", calls)
	}
}

// ============ LaunchAgent（実 herdr） ============

// writeDetectableStub は herdr が agent と検出できる偽 claude を短い /tmp パスに作る。
// ⚠exec は使わない: `exec sleep` は前景を "sleep" に置換するので検出されない
// （実測。sleep を子に持つ sh のままなら検出される）。
func writeDetectableStub(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "hdstub")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	stub := filepath.Join(dir, "claude")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nsleep 300\n"), 0o755); err != nil {
		t.Fatalf("stub 作成: %v", err)
	}
	return stub
}

// TestLaunchAgent は LaunchAgent → agent.list 反映（旧 TestAgentStart の後継）。
// 0.9.x で廃止された agent.start{name,argv} と同じ観測面（名前付き agent が
// 指定 cwd の新 pane に居る）を、layout.apply＋検出待ち rename で満たすこと。
func TestLaunchAgent(t *testing.T) {
	_, c := startHerdr(t)
	stub := writeDetectableStub(t)

	ag, err := c.LaunchAgent("hdapitest", []string{stub}, &LaunchAgentOptions{Cwd: "/private/tmp"})
	if err != nil {
		t.Fatalf("LaunchAgent: %v", err)
	}
	if ag.Name != "hdapitest" || ag.PaneID == "" || ag.TerminalID == "" {
		t.Fatalf("unexpected agent: %+v", ag)
	}
	if ag.Cwd != "/private/tmp" {
		t.Fatalf("agent cwd not applied: %+v", ag)
	}
	// workspace 未指定＝新 workspace に agent pane だけが居る（素シェルを余らせない）。
	panes, err := c.PaneList()
	if err != nil {
		t.Fatalf("pane.list: %v", err)
	}
	var inWS []string
	for _, p := range panes {
		if p.WorkspaceID == ag.WorkspaceID {
			inWS = append(inWS, p.PaneID)
		}
	}
	if len(inWS) != 1 || inWS[0] != ag.PaneID {
		t.Fatalf("新 workspace %s の pane が agent だけでない: %v（agent=%s）", ag.WorkspaceID, inWS, ag.PaneID)
	}

	waitFor(t, 5*time.Second, "agent in agent.list", func() (bool, error) {
		agents, err := c.AgentList()
		if err != nil {
			return false, err
		}
		for _, a := range agents {
			if a.Name == "hdapitest" && a.PaneID == ag.PaneID {
				return true, nil
			}
		}
		return false, nil
	})
}

// 検出されない argv（agent ではない）を渡したら、待ってから loud に失敗する。
// 黙って無名の pane を返さない（呼び手が名前で同定できない状態を成功にしない）。
func TestLaunchAgentFailsLoudlyForUndetectableArgv(t *testing.T) {
	_, c := startHerdr(t)
	_, err := c.LaunchAgent("hdapitest", []string{"/bin/sleep", "300"}, &LaunchAgentOptions{Cwd: "/private/tmp"})
	if err == nil {
		t.Fatal("agent と検出されない argv で成功扱いになった")
	}
	var ae *APIError
	if !errors.As(err, &ae) || ae.Code != "agent_not_found" {
		t.Fatalf("agent_not_found が保たれていない: %v", err)
	}
}
