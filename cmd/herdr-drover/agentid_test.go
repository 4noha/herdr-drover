package main

// identity 判定（agentid.go）の回帰テスト。
//
// ここが 3 サブシステム（shim / restart・update / organize）の**共有の正**なので、
// 優先順位・矛盾・encode/decode の往復を機械で固定する。片側だけ変えると
// 「対象 0 件」や「他エージェントの pane を横取り」が静かに起きる。

import (
	"strings"
	"testing"

	"github.com/4noha/herdr-drover/internal/herdrapi"
)

func agentSess(agent, kind, val string) herdrapi.AgentSession {
	return herdrapi.AgentSession{Source: "herdr:" + agent, Agent: agent, Kind: kind, Value: val}
}

func injTok() map[string]string {
	return map[string]string{herdrapi.InjTokenPC: "other-herdr", herdrapi.InjTokenSID: "w1:p9"}
}

func TestDecodeAgentNameRoundTrip(t *testing.T) {
	// encode が実際に作る形は decode が必ず受ける（往復）。
	for _, agent := range []string{"claude", "codex", "cursor", "pi", "qodercli"} {
		for _, n := range []int{1, 2, 3, 10, 64} {
			name := encodeAgentName(agent, n)
			got, ok := decodeAgentName(name)
			if !ok || got != agent {
				t.Fatalf("往復失敗: encode(%q,%d)=%q → decode=(%q,%v)", agent, n, name, got, ok)
			}
		}
	}
	// decode は encode の**真部分集合**（encode が作らない形は受けない）。
	for _, tc := range []struct {
		name string
		why  string
	}{
		{"", "空"},
		{"claude-0", "先頭ゼロ／N<2"},
		{"claude-1", "N=1 は encode が '-1' を作らない"},
		{"claude-02", "先頭ゼロ"},
		{"claude-", "値なし"},
		{"-claude", "prefix 空"},
		{"hdprobe", "canonical でない"},
		{"hdprobe-2", "canonical でない"},
		{"claude-code", "lookup_agent の入力 alias であって canonical ではない"},
		{"cursor-agent", "同上（実行バイナリ名）"},
		{"claude-2-3", "多段は encode が作らない"},
		{"Claude", "大文字は canonical でない"},
	} {
		if got, ok := decodeAgentName(tc.name); ok {
			t.Errorf("decode(%q) が通った（%s）→ %q", tc.name, tc.why, got)
		}
	}
}

func TestResolveAgentKindPriority(t *testing.T) {
	for _, tc := range []struct {
		name     string
		id       agentIdentity
		wantKind string
		conflict bool
	}{
		{
			name: "注入 pane は最優先で対象外（検出値が canonical でも）",
			id: agentIdentity{ShimName: "claude", Detected: "claude",
				Session: agentSess("claude", "id", "u-1"), Tokens: injTok()},
			wantKind: "",
		},
		{
			name:     "token 片方だけでも注入 pane 扱い（安全側）",
			id:       agentIdentity{ShimName: "claude", Detected: "claude", Tokens: map[string]string{herdrapi.InjTokenSID: "w1:p3"}},
			wantKind: "",
		},
		{
			name:     "agent_session が最強（命名・検出が無くても決まる）",
			id:       agentIdentity{Session: agentSess("codex", "id", "s-1")},
			wantKind: "codex",
		},
		{
			name:     "シム命名のみ",
			id:       agentIdentity{ShimName: "claude-3"},
			wantKind: "claude",
		},
		{
			name:     "herdr 検出値のみ（herdr UI から直接起動＝従来 restart が取りこぼしていた）",
			id:       agentIdentity{Detected: "claude"},
			wantKind: "claude",
		},
		{
			name:     "検出値が canonical でなければ採用しない（外部申告の素通しを弾く）",
			id:       agentIdentity{Detected: "claude-2"},
			wantKind: "",
		},
		{
			name:     "検出値が未知文字列でも採用しない",
			id:       agentIdentity{Detected: "totally-unknown-agent"},
			wantKind: "",
		},
		{
			name:     "3 権威が一致",
			id:       agentIdentity{ShimName: "claude", Detected: "claude", Session: agentSess("claude", "id", "u")},
			wantKind: "claude",
		},
		{
			name:     "session と命名が矛盾",
			id:       agentIdentity{ShimName: "codex", Session: agentSess("claude", "id", "u")},
			conflict: true,
		},
		{
			name:     "session と検出が矛盾",
			id:       agentIdentity{Detected: "codex", Session: agentSess("claude", "id", "u")},
			conflict: true,
		},
		{
			name:     "命名と検出が矛盾",
			id:       agentIdentity{ShimName: "claude-2", Detected: "codex"},
			conflict: true,
		},
		{
			name:     "命名はあるが検出が非空かつ canonical でない＝機械確定不能",
			id:       agentIdentity{ShimName: "claude", Detected: "claude-9"},
			conflict: true,
		},
		{
			name:     "何も無い",
			id:       agentIdentity{},
			wantKind: "",
		},
		{
			name:     "source が herdr:<agent> でない session は採用しない（偽装対策）",
			id:       agentIdentity{Session: herdrapi.AgentSession{Source: "drover-inj", Agent: "claude", Kind: "id", Value: "u"}},
			wantKind: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kind, conflict := resolveAgentKind(tc.id)
			if tc.conflict {
				if conflict == "" {
					t.Fatalf("矛盾を報告しなかった: kind=%q", kind)
				}
				if kind != "" {
					t.Fatalf("矛盾なのに kind=%q を返した（推測で動かしている）", kind)
				}
				return
			}
			if conflict != "" {
				t.Fatalf("想定外の conflict: %s", conflict)
			}
			if kind != tc.wantKind {
				t.Fatalf("kind = %q, want %q", kind, tc.wantKind)
			}
		})
	}
}

// 3 サブシステムが**同じ規則**で判定すること（非対称の再発防止）。
// 従来は organize だけが検出値を見ており、restart/update は取りこぼしていた。
func TestIdentityIsConsistentAcrossSubsystems(t *testing.T) {
	cases := []struct {
		name     string
		agent    herdrapi.AgentInfo
		isClaude bool
	}{
		{"シム命名", herdrapi.AgentInfo{PaneID: "w1:p1", Name: "claude"}, true},
		{"検出のみ", herdrapi.AgentInfo{PaneID: "w1:p2", Agent: "claude"}, true},
		{"注入 pane", herdrapi.AgentInfo{PaneID: "w1:p3", Agent: "claude", Tokens: injTok()}, false},
		{"別 agent", herdrapi.AgentInfo{PaneID: "w1:p4", Agent: "codex"}, false},
		{"無関係", herdrapi.AgentInfo{PaneID: "w1:p5", Name: "hdprobe"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// restart/update 経路
			targets, _, err := selectRestartTargets([]herdrapi.AgentInfo{c.agent}, "")
			if err != nil {
				t.Fatalf("selectRestartTargets: %v", err)
			}
			gotRestart := len(targets) == 1

			// organize 経路（同じ pane を PaneInfo として渡す）
			p := herdrapi.PaneInfo{PaneID: c.agent.PaneID, Agent: c.agent.Agent, Tokens: c.agent.Tokens}
			names := map[string]string{c.agent.PaneID: c.agent.Name}
			gotOrganize, conflict := classifyClaudePane(p, names)
			if conflict != "" {
				t.Fatalf("想定外の conflict: %s", conflict)
			}

			if gotRestart != c.isClaude || gotOrganize != c.isClaude {
				t.Fatalf("判定が食い違う: restart=%v organize=%v want=%v",
					gotRestart, gotOrganize, c.isClaude)
			}
		})
	}
}

// 矛盾は黙って落とさず報告される（silent skip 禁止）。
func TestSelectRestartTargetsReportsConflict(t *testing.T) {
	agents := []herdrapi.AgentInfo{
		{PaneID: "w1:p1", Name: "claude", Agent: "codex"}, // 命名と検出が矛盾
		{PaneID: "w1:p2", Name: "claude-2"},               // 正常
	}
	targets, conflicts, err := selectRestartTargets(agents, "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(targets) != 1 || targets[0].PaneID != "w1:p2" {
		t.Fatalf("対象 = %+v（w1:p2 の 1 件のはず）", targets)
	}
	if len(conflicts) != 1 || !strings.Contains(conflicts[0], "w1:p1") {
		t.Fatalf("矛盾が報告されていない: %v", conflicts)
	}
}

// decode は encode の真部分集合。strconv.Atoi が符号を受理するため、
// 数字チェックを外すと "claude-+2" のような**encode が生成しない**名前が通り、
// 無関係な pane を claude と誤認して破壊する。
func TestDecodeAgentNameRejectsSignedAndNonDigit(t *testing.T) {
	for _, name := range []string{
		"claude-+2", "claude--2", "claude-2_", "claude- 2", "claude-2\n",
		"claude-٢", // Arabic-Indic digit（Atoi は弾くが明示的に固定）
	} {
		if got, ok := decodeAgentName(name); ok {
			t.Errorf("decode(%q) が通った（encode は生成しない形）→ %q", name, got)
		}
	}
}

// identity（何のエージェントか）と actionability（argv を組み立て直せるか）は別物。
// wrapper 起動 pane に --resume を足しても届かないので、触ってはいけない。
func TestIsDirectAgentInvocation(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		want bool
		why  string
	}{
		{[]string{"/Users/x/.local/bin/claude"}, true, "絶対パスの直接起動"},
		{[]string{"claude", "--model", "opus"}, true, "相対名でも直接起動ではある"},
		{[]string{"/usr/local/bin/claude"}, true, "別 prefix"},
		{[]string{"zsh", "-lc", "cd ~/p && claude"}, false, "wrapper 経由＝--resume が届かない"},
		{[]string{"/bin/bash", "-c", "claude"}, false, "同上"},
		{[]string{"node", "/x/cli.js"}, false, "ランタイム経由"},
		{[]string{}, false, "argv 無し"},
		{[]string{"claude-wrapper.sh"}, false, "名前が似ているだけ"},
	} {
		if got := isDirectAgentInvocation("claude", tc.argv); got != tc.want {
			t.Errorf("isDirectAgentInvocation(claude, %v) = %v, want %v（%s）", tc.argv, got, tc.want, tc.why)
		}
	}
	// 表に無い kind は常に false（知らないものは触らない）。
	if isDirectAgentInvocation("codex", []string{"/usr/bin/codex"}) {
		t.Error("未対応 kind を直接起動と判定した（推測で触ってはいけない）")
	}
}

// 未命名 pane が正規の対象になったので、要約は pane を特定できる文字列にする
// （遠隔 Ack として Firestore に残る監査記録＝空文字だと復元不能）。
func TestSummarizeRestartIdentifiesUnnamedPanes(t *testing.T) {
	got := summarizeRestart([]restartOutcome{
		{PaneID: "w1:p4", Name: "", Status: "done"},
		{PaneID: "w2:p1", Name: "", Status: "done"},
	})
	if strings.Contains(got, ": ,") || !strings.Contains(got, "w1:p4") || !strings.Contains(got, "w2:p1") {
		t.Fatalf("未命名 pane を特定できない要約: %q", got)
	}
}
