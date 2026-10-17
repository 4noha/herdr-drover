//go:build unix

package main

import "time"

// killHerdrOrphans は Windows 専用の後始末（herdrtree_windows_test.go）。unix の herdr は
// stop で pane のプロセスを畳み、実行中プロセスの cwd も削除を妨げない＝何もしない。
func killHerdrOrphans(herdrPID int, started time.Time) {}
