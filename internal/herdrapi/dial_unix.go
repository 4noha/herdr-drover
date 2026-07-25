//go:build !windows

package herdrapi

import (
	"net"
	"os"
	"path/filepath"
	"time"
)

// dialHerdr は herdr の ndjson API socket へ接続する（unix: AF_UNIX）。
func dialHerdr(path string, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout("unix", path, timeout)
}

// defaultSocketPath は既定の socket パス（unix: ~/.config/herdr/herdr.sock）。
// ⚠sun_path は 104B 制約（macOS）＝深い階層のパスはサーバ起動自体が失敗する。
func defaultSocketPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		// home 不明でも「herdr.sock」への相対 dial で即エラーになり原因が
		// 分かる方が、空文字で意味不明に失敗するよりまし。
		return "herdr.sock"
	}
	return filepath.Join(home, ".config", "herdr", "herdr.sock")
}
