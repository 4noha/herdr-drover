package main

// update — 手動 selfupdate（GitHub Releases から sha256 検証つき原子置換。
// 実体は internal/selfupdate＝遠隔命令 self-update と同じ経路）。

import (
	"bytes"
	"crypto/sha256"
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
		// 「既に最新」でも stale CLI (~/.local/bin) の同期チェックは走らせる。
		// 旧版 (~v0.5.6) からの持ち越しで CLI が binDst と一致していないケースの
		// 追いつきに必要（v0.5.6 実測: 稼働 binDst は v0.5.6 でも CLI は v0.5.5 の
		// まま残り、alias claude で workspaces.json 未知フィールドエラーを踏む）。
		syncCLIBinaryOnLatestCheck(stdout)
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
	// CLI バイナリ（~/.local/bin/herdr-drover 相当・alias claude が指す先）も
	// 同期する（v0.5.6〜）。install.sh は ~/.local/bin へ置くが selfupdate は
	// 従来 binDst のみを更新していたため、CLI 側が v0.5.5 未満のまま stale に
	// なり workspaces.json の inject_placement を「未知フィールド」で拒否する
	// 実バグ（v0.5.5 リリース後に実測）の再発防止。存在しないパス（HD_BINDIR
	// が壊れている / install.sh を通していない）は skip。exe と同一 or binDst
	// と同一なら重複作業を避ける（symlink・同 inode 両方を EvalSymlinks で判定）。
	syncCLIBinary(exeReal, dstReal, p.cliBinPath, stdout)
	// ⚠バイナリはプロセス起動時のみ反映（cm 教訓）＝常駐 agent は再起動で
	// 初めて新版になる。
	fmt.Fprintf(stdout, "常駐 agent への反映は再起動後: launchctl kickstart -k gui/$UID/%s（または Web の restart-agent）\n", launchdLabel)
	return nil
}

// syncCLIBinaryOnLatestCheck は「既に最新」early return 経路でも CLI 側の
// stale チェックを 1 回だけ走らせる。前バージョンから持ち越された CLI と
// 稼働 binDst が SHA 不一致な場合に追いつく（v0.5.6 実測: syncCLIBinary が
// updated==false パスで skip され CLI stale が残った）。
func syncCLIBinaryOnLatestCheck(stdout io.Writer) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	exe, err := updateExecutable()
	if err != nil {
		return
	}
	p := resolveInstallPaths(home)
	if _, err := os.Stat(p.binDst); err != nil {
		return // binDst 無し（未 install）は何もしない
	}
	exeReal, e1 := filepath.EvalSymlinks(exe)
	dstReal, e2 := filepath.EvalSymlinks(p.binDst)
	if e1 != nil || e2 != nil {
		return
	}
	syncCLIBinary(exeReal, dstReal, p.cliBinPath, stdout)
}

// syncCLIBinary は ~/.local/bin/herdr-drover（CLI 側・install.sh が置く）を
// binDst と同一版に揃える。selfupdate が binDst しか触らず CLI が stale になる
// 実バグ（v0.5.5 で実測）の対策。存在しないパス／exe or binDst と同 inode は skip。
// エラーは warn ログのみ（selfupdate 自体は成功しているので終了コードを汚さない）。
// **追加ガード** (v0.5.7〜): 同 inode でなくても **SHA が一致** すれば skip
// （cp -f で手動同期済のケースで無駄書き＋stdout 冗長を避ける）。
func syncCLIBinary(exeReal, dstReal, cliBinPath string, stdout io.Writer) {
	if cliBinPath == "" {
		return
	}
	// 存在しない CLI パスは skip（install.sh を通していないユーザーへの誤配線を避ける）。
	if _, err := os.Stat(cliBinPath); err != nil {
		return
	}
	cliReal, err := filepath.EvalSymlinks(cliBinPath)
	if err != nil {
		fmt.Fprintf(stdout, "⚠ CLI バイナリ %s の実パス解決失敗（skip）: %v\n", cliBinPath, err)
		return
	}
	// 既に同 inode（symlink 経由）or 自バイナリと同一なら再配置不要。
	if cliReal == exeReal || cliReal == dstReal {
		return
	}
	// 内容 (SHA) 一致なら再配置不要（cp -f で手動同期済のケース）。
	// 稼働 binDst と CLI が別 inode でも同一 binary なら書換無用＝stdout も冗長にしない。
	if sameFileContent(cliReal, dstReal) {
		return
	}
	// 新 inode 配置（rm→新規 write→rename＝macOS 署名キャッシュ罠を回避）。
	// 上流の binDst 更新が既に済んでいるので dstReal をソースにする。
	if err := placeBinaryNewInode(dstReal, cliBinPath); err != nil {
		fmt.Fprintf(stdout, "⚠ CLI バイナリ %s の更新失敗（手動で `cp -f %s %s` してください）: %v\n",
			cliBinPath, dstReal, cliBinPath, err)
		return
	}
	fmt.Fprintf(stdout, "✔ CLI バイナリも同期: %s（新 inode）\n", cliBinPath)
}

// sameFileContent は 2 ファイルの内容が完全一致するかを SHA256 で判定する
// （size 事前チェックで早期 false・巨大ファイルでも読取 1 pass）。
// エラー時は false を返す（呼び手は再配置に進む＝安全側）。
func sameFileContent(a, b string) bool {
	fa, err := os.Open(a)
	if err != nil {
		return false
	}
	defer fa.Close()
	fb, err := os.Open(b)
	if err != nil {
		return false
	}
	defer fb.Close()
	sa, _ := fa.Stat()
	sb, _ := fb.Stat()
	if sa != nil && sb != nil && sa.Size() != sb.Size() {
		return false
	}
	ha := sha256.New()
	if _, err := io.Copy(ha, fa); err != nil {
		return false
	}
	hb := sha256.New()
	if _, err := io.Copy(hb, fb); err != nil {
		return false
	}
	return bytes.Equal(ha.Sum(nil), hb.Sum(nil))
}
