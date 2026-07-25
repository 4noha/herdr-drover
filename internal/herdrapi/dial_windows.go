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

// defaultSocketPath は既定の socket パス（Windows: %APPDATA%\herdr\herdr.sock）。
func defaultSocketPath() string {
	if ad := os.Getenv("APPDATA"); ad != "" {
		return filepath.Join(ad, "herdr", "herdr.sock")
	}
	return "herdr.sock"
}
