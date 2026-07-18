package wsmap

// 着地ルールの機械検証。Resolve/Parse は純関数＝テーブル網羅。
// Load/Save は実 temp-HOME のファイル往復（writeFileAtomic の実書込）。
// ResolveWorkspaceID の実 herdr 検証は cmd/herdr-drover/claudeshim_newtab_test.go
// （隔離サーバ harness がそちらに在る）。

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// ============ 純関数: Resolve（exact > 最長 prefix > default > ""） ============

func TestResolveTable(t *testing.T) {
	home := "/Users/u"
	m := &Map{
		Exact: map[string]string{
			"/w/proj":     "ws-exact",
			"~/works/app": "ws-home",
		},
		Rules: []Rule{
			{Prefix: "/w", Workspace: "ws-w"},
			{Prefix: "/w/proj/sub", Workspace: "ws-sub"},
			{Prefix: "~/works", Workspace: "ws-works"},
		},
		Default: "ws-def",
	}
	cases := []struct {
		name string
		cwd  string
		want string
	}{
		{"exact が prefix より優先", "/w/proj", "ws-exact"},
		{"~ 展開の exact", "/Users/u/works/app", "ws-home"},
		{"最長 prefix 勝ち", "/w/proj/sub/deep", "ws-sub"},
		{"短い prefix のみ一致", "/w/other", "ws-w"},
		{"prefix はパス境界一致（/w は /wx に一致しない）", "/wx/proj", "ws-def"},
		{"~ 展開の prefix", "/Users/u/works/lib", "ws-works"},
		{"どれにも一致しなければ default", "/elsewhere", "ws-def"},
		{"exact 自身の子孫は exact でなく prefix 系（/w）", "/w/proj/child", "ws-w"},
	}
	for _, c := range cases {
		if got := m.Resolve(c.cwd, home); got != c.want {
			t.Fatalf("%s: Resolve(%q)=%q want %q", c.name, c.cwd, got, c.want)
		}
	}
}

func TestResolveNoDefaultReturnsEmpty(t *testing.T) {
	m := &Map{Rules: []Rule{{Prefix: "/w", Workspace: "ws-w"}}}
	if got := m.Resolve("/elsewhere", "/Users/u"); got != "" {
		t.Fatalf("default 無しの不一致で %q（want \"\"=ルール無し）", got)
	}
	empty := &Map{}
	if got := empty.Resolve("/anything", "/Users/u"); got != "" {
		t.Fatalf("空 Map で %q（want \"\"）", got)
	}
}

func TestResolveSameLengthPrefixFirstRuleWins(t *testing.T) {
	// 同長 prefix（同一 prefix の重複定義）は rules 配列の先勝ち＝決定的。
	m := &Map{Rules: []Rule{
		{Prefix: "/w/a", Workspace: "first"},
		{Prefix: "/w/a", Workspace: "second"},
	}}
	if got := m.Resolve("/w/a/x", "/h"); got != "first" {
		t.Fatalf("同長 prefix の先勝ちが崩れた: %q", got)
	}
}

func TestResolveExactDuplicateExpansionDeterministic(t *testing.T) {
	// "~/x" と "/Users/u/x" が同一パスへ展開される場合はキー辞書順の先勝ち
	// （"/Users/u/x" < "~/x"）＝map 走査順に依存しない決定性。
	m := &Map{Exact: map[string]string{
		"~/x":         "tilde",
		"/Users/u/x":  "abs",
		"/Users/u/x/": "abs-slash", // Clean で同一化（辞書順では末尾 / 付きが後）
	}}
	for i := 0; i < 20; i++ { // map 順の揺れを炙り出すため反復
		if got := m.Resolve("/Users/u/x", "/Users/u"); got != "abs" {
			t.Fatalf("展開衝突キーの決定的解決が崩れた（iter %d）: %q", i, got)
		}
	}
}

func TestResolveTrailingSlashAndRootPrefix(t *testing.T) {
	m := &Map{Rules: []Rule{{Prefix: "/w/a/", Workspace: "ws-a"}}}
	if got := m.Resolve("/w/a/x", "/h"); got != "ws-a" {
		t.Fatalf("末尾スラッシュ prefix が効かない: %q", got)
	}
	if got := m.Resolve("/w/a", "/h"); got != "ws-a" {
		t.Fatalf("prefix そのものの cwd が一致しない: %q", got)
	}
	root := &Map{Rules: []Rule{{Prefix: "/", Workspace: "ws-root"}}}
	if got := root.Resolve("/any/where", "/h"); got != "ws-root" {
		t.Fatalf("ルート prefix / が全 abs パスに一致しない: %q", got)
	}
}

func TestResolveTildeOnlyPrefix(t *testing.T) {
	m := &Map{Rules: []Rule{{Prefix: "~", Workspace: "ws-home"}}}
	if got := m.Resolve("/Users/u/anything", "/Users/u"); got != "ws-home" {
		t.Fatalf("prefix ~ が home 配下に一致しない: %q", got)
	}
	if got := m.Resolve("/Users/uu/x", "/Users/u"); got != "" {
		t.Fatalf("prefix ~ が /Users/uu（境界外）に一致した: %q", got)
	}
}

// ============ 純関数: Parse（壊れ・未知フィールド・不正値は loud） ============

func TestParseLoudErrors(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"壊れた JSON", `{"exact": {broken`},
		{"未知フィールド（typo の silent 無視禁止）", `{"defalut": "x"}`},
		{"JSON 値の後の余分な内容", `{}{"exact":{}}`},
		{"相対パスの exact キー", `{"exact": {"rel/path": "x"}}`},
		{"相対パスの prefix", `{"rules": [{"prefix": "rel", "workspace": "x"}]}`},
		{"空 label の exact", `{"exact": {"/a": ""}}`},
		{"空 label の rule", `{"rules": [{"prefix": "/a", "workspace": ""}]}`},
		{"~user 形式は非サポート", `{"exact": {"~other/x": "a"}}`},
	}
	for _, c := range cases {
		if _, err := Parse([]byte(c.in)); err == nil {
			t.Fatalf("%s: エラーにならない（loud 規律違反）: %s", c.name, c.in)
		}
	}
}

func TestParseValid(t *testing.T) {
	m, err := Parse([]byte(`{
	  "exact": {"/w/proj": "a", "~/x": "b"},
	  "rules": [{"prefix": "/w", "workspace": "c"}],
	  "default": "d"
	}`))
	if err != nil {
		t.Fatalf("正常スキーマで Parse 失敗: %v", err)
	}
	if m.Exact["/w/proj"] != "a" || m.Exact["~/x"] != "b" ||
		len(m.Rules) != 1 || m.Rules[0].Workspace != "c" || m.Default != "d" {
		t.Fatalf("Parse 結果が想定外: %+v", m)
	}
}

// ============ 実ファイル: Load/Save（temp-HOME 隔離） ============

func TestLoadMissingFileIsEmptyMap(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m, err := Load()
	if err != nil {
		t.Fatalf("ファイル不在は「ルール無し」で正常のはず: %v", err)
	}
	if got := m.Resolve("/anything", "/h"); got != "" {
		t.Fatalf("空 Map の Resolve が %q（want \"\"）", got)
	}
}

func TestLoadBrokenFileLoudWithPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".herdr-drover")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	p := filepath.Join(dir, "workspaces.json")
	if err := os.WriteFile(p, []byte(`{"exact": {broken`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := Load()
	if err == nil {
		t.Fatalf("壊れたファイルで Load が成功（silent fallback 禁止）")
	}
	if !strings.Contains(err.Error(), p) {
		t.Fatalf("エラーにファイルパスが無い＝ユーザーが直せない: %v", err)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	orig := &Map{
		Exact:   map[string]string{"/w/proj": "a", "~/works/x": "b"},
		Rules:   []Rule{{Prefix: "~/works", Workspace: "c"}, {Prefix: "/opt", Workspace: "d"}},
		Default: "e",
	}
	if err := orig.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Load は生値を保持する（~ を silent に abs へ書き換えない＝鉄則④）
	if !reflect.DeepEqual(orig, got) {
		t.Fatalf("往復不一致:\norig=%+v\ngot =%+v", orig, got)
	}
	// 上書き Save も原子的に成立する（writeFileAtomic 経路の実書込）
	orig.Default = "f"
	if err := orig.Save(); err != nil {
		t.Fatalf("Save(2): %v", err)
	}
	got2, err := Load()
	if err != nil {
		t.Fatalf("Load(2): %v", err)
	}
	if got2.Default != "f" {
		t.Fatalf("上書き Save が反映されない: %+v", got2)
	}
}

// ============ Update（flock 下の read-modify-write） ============

// 並行 writer が各自のキーだけを Update で書いても全キーが残ること
// （lost update 防止の要。flock は open file description 単位＝同一プロセス
// 内の別 fd でも相互排除される＝実プロセス間と同じ経路を踏む）。
func TestUpdateConcurrentWritersKeepAllKeys(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- Update(func(m *Map) (bool, error) {
				if m.Exact == nil {
					m.Exact = map[string]string{}
				}
				m.Exact[fmt.Sprintf("/k%d", i)] = "v"
				return true, nil
			})
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	m, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Exact) != n {
		t.Fatalf("lost update: %d/%d キーしか残っていない: %+v", len(m.Exact), n, m.Exact)
	}
}

// changed=false / mutate エラー時は Save しない（既存内容が不変のまま）。
func TestUpdateNoSaveOnUnchangedOrError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	seed := `{"exact":{"/a":"x"}}`
	dir := filepath.Join(home, ".herdr-drover")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "workspaces.json")
	if err := os.WriteFile(p, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Update(func(m *Map) (bool, error) {
		m.Exact["/a"] = "mutated-but-unchanged" // changed=false なら破棄される
		return false, nil
	}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != seed {
		t.Fatalf("changed=false なのに書き換わった: %s", b)
	}
	wantErr := fmt.Errorf("boom")
	if err := Update(func(m *Map) (bool, error) {
		m.Exact["/a"] = "mutated-then-error"
		return true, wantErr
	}); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("mutate エラーが返らない: %v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != seed {
		t.Fatalf("mutate エラーなのに書き換わった: %s", b)
	}
}
