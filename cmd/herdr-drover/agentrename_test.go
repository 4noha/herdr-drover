package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/4noha/herdr-drover/internal/herdrapi"
)

// TestRenameWithDetectWait は herdr 0.9.x の「agent 検出が非同期」に追随する
// リトライの性質を固定する（BUG-13）。
//
// ⚠これが落ちると herdr 0.9.x で claude シム・restart-agent-session・update-all が
// pane 生成直後の agent.rename（agent_not_found）で必ず失敗する。

func apiErr(code string) error {
	return &herdrapi.APIError{Code: code, Message: code}
}

func TestRenameWithDetectWaitRetriesUntilDetected(t *testing.T) {
	calls := 0
	call := func(pane, name string) (json.RawMessage, error) {
		calls++
		if calls < 4 { // 0.9.5 実測: 数回 agent_not_found の後に通る
			return nil, apiErr("agent_not_found")
		}
		return json.RawMessage(`{"agent":{"name":"claude"}}`), nil
	}
	raw, waited, err := renameWithDetectWait(call, "w1:p2", "claude", 2*time.Second, 5*time.Millisecond)
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
	_, _, err := renameWithDetectWait(call, "p", "n", time.Second, time.Millisecond)
	if err != nil || calls != 1 {
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
		_, _, err := renameWithDetectWait(call, "p", "n", 5*time.Second, 50*time.Millisecond)
		var ae *herdrapi.APIError
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
	_, _, err := renameWithDetectWait(call, "w1:p9", "claude", 120*time.Millisecond, 10*time.Millisecond)
	if err == nil {
		t.Fatal("検出されないのに成功扱い")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("上限を超えて待っている: %v", elapsed)
	}
	var ae *herdrapi.APIError
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
