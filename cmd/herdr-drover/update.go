package main

// update — 手動 selfupdate（GitHub Releases から sha256 検証つき原子置換。
// 実体は internal/selfupdate＝遠隔命令 self-update と同じ経路）。

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/4noha/drover-cloud/selfupdate"
)

// テスト seam（internal/commands の DoUpdate seam と同じ流儀）。
// selfupdate.Update は実 GitHub へ出るため、cmdUpdate の「置換後の稼働
// バイナリ同期」を実ファイルで検証するテストがここを差し替える
// （selfupdate 本体の HTTP/sha256/原子置換は internal/selfupdate の
// ローカル fixture テストが実経路で担保済み）。
var (
	updateSelf       = selfupdate.Update
	updateExecutable = os.Executable
)

func cmdUpdate(stdout io.Writer) error {
	fmt.Fprintf(stdout, "現在 %s。最新を確認中...\n", version)
	tag, updated, err := updateSelf(version)
	if err != nil {
		return fmt.Errorf("更新失敗: %w", err)
	}
	if !updated {
		fmt.Fprintf(stdout, "既に最新です (%s)\n", tag)
		return nil
	}
	fmt.Fprintf(stdout, "更新しました: %s → %s\n", version, tag)

	// 稼働バイナリ（launchd Program = ~/.herdr-drover/bin/herdr-drover）も
	// 同期する。selfupdate は os.Executable（例: ~/.local/bin）しか置換
	// しないため、これ無しで kickstart を案内すると launchd は**旧バイナリを
	// 再 exec** し、ユーザーは更新済みと信じたまま daemon が旧版で走り続ける
	// （cm の「旧 inode proxy は旧版🔴」と同型の stale 版事故。binDst が
	// 別コピーなのは開発ビルドが稼働 daemon を SIGKILL しない分離のため＝
	// install.go 冒頭コメント）。
	exe, err := updateExecutable()
	if err != nil {
		return fmt.Errorf("自バイナリは更新済みだが自パスが取れない: %w（稼働バイナリ反映は `herdr-drover install` を再実行すること）", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("自バイナリは更新済みだが home が取れない: %w（稼働バイナリ反映は `herdr-drover install` を再実行すること）", err)
	}
	p := resolveInstallPaths(home)
	if _, err := os.Stat(p.binDst); err != nil {
		// launchd 未 install＝稼働バイナリ無し。存在しない常駐への kickstart
		// 案内は誤誘導なので出さない（常駐したい場合は install が正規手順）。
		return nil
	}
	// 自バイナリ＝稼働バイナリ（binDst 直接実行・agent 経由の self-update と
	// 同型）なら selfupdate が既に新 inode で置換済み＝再配置不要。symlink
	// 経由（~/.local/bin が binDst への link 等）も実体一致で判定する。
	exeReal, e1 := filepath.EvalSymlinks(exe)
	dstReal, e2 := filepath.EvalSymlinks(p.binDst)
	if e1 != nil || e2 != nil || exeReal != dstReal {
		// rm→新 inode 配置（placeBinaryNewInode）＝macOS 署名キャッシュ罠
		// （同 inode 上書きは exec SIGKILL）を踏まない。
		if err := placeBinaryNewInode(exe, p.binDst); err != nil {
			return fmt.Errorf("自バイナリは更新済みだが稼働バイナリ %s の更新に失敗: %w（`herdr-drover install` を再実行してから kickstart すること）", p.binDst, err)
		}
		fmt.Fprintf(stdout, "✔ 稼働バイナリも更新: %s（新 inode）\n", p.binDst)
	}
	// ⚠バイナリはプロセス起動時のみ反映（cm 教訓）＝常駐 agent は再起動で
	// 初めて新版になる。
	fmt.Fprintf(stdout, "常駐 agent への反映は再起動後: launchctl kickstart -k gui/$UID/%s（または Web の restart-agent）\n", launchdLabel)
	return nil
}
