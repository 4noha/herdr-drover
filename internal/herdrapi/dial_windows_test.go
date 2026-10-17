//go:build windows

package herdrapi

import (
	"path/filepath"
	"testing"
)

func TestWindowsDefaultSocketPath(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HERDR_SOCKET_PATH", "")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("APPDATA", filepath.Join(root, "roaming"))
	t.Setenv("USERPROFILE", filepath.Join(root, "profile"))
	t.Setenv("HOME", filepath.Join(root, "home"))
	for _, tc := range []struct {
		name, unset, want string
	}{
		{"xdg", "", filepath.Join(root, "config", "herdr", "herdr.sock")},
		{"appdata", "XDG_CONFIG_HOME", filepath.Join(root, "roaming", "herdr", "herdr.sock")},
		{"profile", "APPDATA", filepath.Join(root, "profile", "AppData", "Roaming", "herdr", "herdr.sock")},
		{"home", "USERPROFILE", filepath.Join(root, "home", ".config", "herdr", "herdr.sock")},
	} {
		if tc.unset != "" {
			t.Setenv(tc.unset, "")
		}
		if got := ResolveSocketPath(""); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}
