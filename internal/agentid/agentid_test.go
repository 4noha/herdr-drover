package agentid

// identity 判定の回帰テスト。ここが shim / restart・update / organize / producer の
// **共有の正**なので、優先順位・矛盾・encode/decode の往復を機械で固定する。
// 片側だけ変えると「対象 0 件」や「他エージェントの pane を横取り」が静かに起きる。

import (
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
			name := Encode(agent, n)
			got, ok := Decode(name)
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
		if got, ok := Decode(tc.name); ok {
			t.Errorf("decode(%q) が通った（%s）→ %q", tc.name, tc.why, got)
		}
	}
}

func TestResolveAgentKindPriority(t *testing.T) {
	for _, tc := range []struct {
		name     string
		id       Identity
		wantKind string
		conflict bool
	}{
		{
			name: "注入 pane は最優先で対象外（検出値が canonical でも）",
			id: Identity{ShimName: "claude", Detected: "claude",
				Session: agentSess("claude", "id", "u-1"), Tokens: injTok()},
			wantKind: "",
		},
		{
			name:     "token 片方だけでも注入 pane 扱い（安全側）",
			id:       Identity{ShimName: "claude", Detected: "claude", Tokens: map[string]string{herdrapi.InjTokenSID: "w1:p3"}},
			wantKind: "",
		},
		{
			name:     "agent_session が最強（命名・検出が無くても決まる）",
			id:       Identity{Session: agentSess("codex", "id", "s-1")},
			wantKind: "codex",
		},
		{
			name:     "シム命名のみ",
			id:       Identity{ShimName: "claude-3"},
			wantKind: "claude",
		},
		{
			name:     "herdr 検出値のみ（herdr UI から直接起動＝従来 restart が取りこぼしていた）",
			id:       Identity{Detected: "claude"},
			wantKind: "claude",
		},
		{
			name:     "検出値が canonical でなければ採用しない（外部申告の素通しを弾く）",
			id:       Identity{Detected: "claude-2"},
			wantKind: "",
		},
		{
			name:     "検出値が未知文字列でも採用しない",
			id:       Identity{Detected: "totally-unknown-agent"},
			wantKind: "",
		},
		{
			name:     "3 権威が一致",
			id:       Identity{ShimName: "claude", Detected: "claude", Session: agentSess("claude", "id", "u")},
			wantKind: "claude",
		},
		{
			name:     "session と命名が矛盾",
			id:       Identity{ShimName: "codex", Session: agentSess("claude", "id", "u")},
			conflict: true,
		},
		{
			name:     "session と検出が矛盾",
			id:       Identity{Detected: "codex", Session: agentSess("claude", "id", "u")},
			conflict: true,
		},
		{
			name:     "命名と検出が矛盾",
			id:       Identity{ShimName: "claude-2", Detected: "codex"},
			conflict: true,
		},
		{
			name:     "命名はあるが検出が非空かつ canonical でない＝機械確定不能",
			id:       Identity{ShimName: "claude", Detected: "claude-9"},
			conflict: true,
		},
		{
			name:     "何も無い",
			id:       Identity{},
			wantKind: "",
		},
		{
			name:     "source が herdr:<agent> でない session は採用しない（偽装対策）",
			id:       Identity{Session: herdrapi.AgentSession{Source: "drover-inj", Agent: "claude", Kind: "id", Value: "u"}},
			wantKind: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kind, conflict := Resolve(tc.id)
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

// decode は encode の真部分集合。strconv.Atoi が符号を受理するため、
// 数字チェックを外すと "claude-+2" のような**encode が生成しない**名前が通り、
// 無関係な pane を claude と誤認して破壊する。
func TestDecodeAgentNameRejectsSignedAndNonDigit(t *testing.T) {
	for _, name := range []string{
		"claude-+2", "claude--2", "claude-2_", "claude- 2", "claude-2\n",
		"claude-٢", // Arabic-Indic digit（Atoi は弾くが明示的に固定）
	} {
		if got, ok := Decode(name); ok {
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
		if got := IsDirectInvocation("claude", tc.argv); got != tc.want {
			t.Errorf("IsDirectInvocation(claude, %v) = %v, want %v（%s）", tc.argv, got, tc.want, tc.why)
		}
	}
	// 表に無い kind は常に false（知らないものは触らない）。
	if IsDirectInvocation("codex", []string{"/usr/bin/codex"}) {
		t.Error("未対応 kind を直接起動と判定した（推測で触ってはいけない）")
	}
}
