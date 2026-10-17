// Package testtmp は実 herdr 隔離サーバ用の一時 dir の置き場所を OS ごとに返す
// （テスト専用。製品コードからは import しない）。
package testtmp

import (
	"os"
	"runtime"
)

// Root は os.MkdirTemp の親に使う短いパス。
//   - unix: "/tmp"。macOS の sun_path 104B 制約＝t.TempDir() の深い階層では
//     herdr の socket bind 自体が失敗する（実測）。
//   - Windows: herdr は named pipe（pipe 名は socket パス文字列そのもの）で
//     sun_path 制約が無く、"/tmp" は存在しない＝OS の temp dir を使う。
func Root() string {
	if runtime.GOOS == "windows" {
		return os.TempDir()
	}
	return "/tmp"
}
