package main

import "testing"

// injCommandLine はシェルごとの引用規則で attach のコマンド行を組む（BUG-6 の
// Windows 復活経路）。実シェルでの解釈は reconcilelive_windows_test.go が検証する。
func TestInjCommandLine(t *testing.T) {
	argv := []string{`C:\Users\a b\it's\herdr-drover.exe`, "attach", "mac-studio-herdr", "w1:p4J"}
	for _, c := range []struct{ shell, want string }{
		{"powershell.exe", `& 'C:\Users\a b\it''s\herdr-drover.exe' 'attach' 'mac-studio-herdr' 'w1:p4J'`},
		{"pwsh.exe", `& 'C:\Users\a b\it''s\herdr-drover.exe' 'attach' 'mac-studio-herdr' 'w1:p4J'`},
		{"cmd.exe", `"C:\Users\a b\it's\herdr-drover.exe" "attach" "mac-studio-herdr" "w1:p4J"`},
		{"zsh", `'C:\Users\a b\it'\''s\herdr-drover.exe' 'attach' 'mac-studio-herdr' 'w1:p4J'`},
	} {
		got, err := injCommandLine(c.shell, argv)
		if err != nil || got != c.want {
			t.Errorf("%s: got %q err=%v\nwant %q", c.shell, got, err, c.want)
		}
	}
	// cmd は " と % を安全に表記できない＝推測で崩さずエラー（呼び手がログして見送る）。
	for _, bad := range []string{`C:\x"y\d.exe`, `C:\%USERPROFILE%\d.exe`} {
		if _, err := injCommandLine("cmd.exe", []string{bad, "attach"}); err == nil {
			t.Errorf("cmd で %q を受け入れた", bad)
		}
	}
}
