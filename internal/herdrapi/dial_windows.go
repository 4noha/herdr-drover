//go:build windows

package herdrapi

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/Microsoft/go-winio"
)

// dialHerdr は herdr の API socket へ接続する（Windows: named pipe）。
//
// Windows 版 herdr は AF_UNIX ではなく named pipe を使う。socket パスの
// ファイル（例 %APPDATA%\herdr\herdr.sock）は実体はヒントファイル（中身は
// <pid>:<nonce>）で、実際の pipe 名は **その socket パス文字列そのもの**
// （コロン込み）。コロンのため Win32 の `\\.\pipe\...` では開けず、NT 名前
// 空間接頭辞 `\??\pipe\` を付けると開ける（実測。go-winio も同接頭辞なら可）。
func dialHerdr(path string, timeout time.Duration) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return winio.DialPipeContext(ctx, `\??\pipe\`+path)
}

// defaultSocketPath は herdr（Windows）の設定 dir 解決順に合わせた既定 socket パス:
// XDG_CONFIG_HOME > %APPDATA% > %USERPROFILE%\AppData\Roaming > HOME/.config
// （Windows でも XDG_CONFIG_HOME が最優先＝隔離サーバのテストもこれで分離する）。
func defaultSocketPath() string {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "herdr", "herdr.sock")
	}
	if ad := os.Getenv("APPDATA"); ad != "" {
		return filepath.Join(ad, "herdr", "herdr.sock")
	}
	if profile := os.Getenv("USERPROFILE"); profile != "" {
		return filepath.Join(profile, "AppData", "Roaming", "herdr", "herdr.sock")
	}
	if home := os.Getenv("HOME"); home != "" {
		return filepath.Join(home, ".config", "herdr", "herdr.sock")
	}
	return filepath.Join(os.TempDir(), "herdr", "herdr.sock")
}
