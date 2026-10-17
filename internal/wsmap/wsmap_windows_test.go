//go:build windows

package wsmap

import "testing"

// Windows の実パスでの解決。herdr 0.9.5（Windows）の pane cwd は末尾 \ 付き
// （実採取: "C:\\Users\\nokki\\AppData\\Local\\Temp\\...\\001\\"）。旧 hasPathPrefix は
// 区切りを "/" 固定にしていたため、Windows では prefix ルールが一切一致しなかった
// （exact と default だけが効き、prefix は silent に無効）。
func TestResolveWindowsPaths(t *testing.T) {
	home := `C:\Users\u`
	m := &Map{
		Exact: map[string]string{
			`C:\w\proj`:   "ws-exact",
			"~/works/app": "ws-home",
		},
		Rules: []Rule{
			{Prefix: `C:\w`, Workspace: "ws-w"},
			{Prefix: `C:\w\proj\sub`, Workspace: "ws-sub"},
			{Prefix: "~/works", Workspace: "ws-works"},
			{Prefix: `D:\`, Workspace: "ws-d"},
		},
		Default: "ws-def",
	}
	for _, c := range []struct{ name, cwd, want string }{
		{"exact（herdr の末尾 \\ 付き cwd）", `C:\w\proj\`, "ws-exact"},
		{"~ 展開の exact", `C:\Users\u\works\app`, "ws-home"},
		{"最長 prefix 勝ち", `C:\w\proj\sub\deep`, "ws-sub"},
		{"短い prefix のみ一致", `C:\w\other\`, "ws-w"},
		{"prefix はパス境界一致（C:\\w は C:\\wx に一致しない）", `C:\wx\proj`, "ws-def"},
		{"~ 展開の prefix", `C:\Users\u\works\lib`, "ws-works"},
		{"ドライブルート prefix", `D:\any\where`, "ws-d"},
		{"どれにも一致しなければ default", `E:\elsewhere`, "ws-def"},
	} {
		if got := m.Resolve(c.cwd, home); got != c.want {
			t.Errorf("%s: Resolve(%q)=%q want %q", c.name, c.cwd, got, c.want)
		}
	}
}
