package main

// install / uninstall — launchd 常駐の登録・解除（macOS）。
//
// herdr プラグイン機構は常駐不可（hook=一発 spawn・64KB cap・supervision
// 無し＝実測）なので、常駐本体は launchd に置く（DESIGN 決定事項）。この
// コマンドは herdr action "install"（scripts/install-action.sh）からも、
// 手動 `herdr-drover install` からも呼ばれる。
//
// cm（claude-master-go）から継承する教訓（根拠は cm CLAUDE.md の実障害記録）:
//   - **ProcessType は指定しない（Background 禁止）**: cm で
//     ProcessType=Background が daemon と exec 子を pri=4 に throttle →
//     scan timeout → 空 STATUS flap の正帰還という実障害を起こした。
//     launchd 既定（未指定）が正解。生成 plist にもコメントで残す。
//   - **バイナリは rm→新 inode 配置**: macOS は既存バイナリへの in-place
//     上書き（同 inode）で署名キャッシュ不整合 → exec SIGKILL
//     (OS_REASON_CODESIGNING) になる実挙動がある。必ず旧ファイルを rm して
//     から新 inode で置く（cm の tmux 差替・daemon 分離で実測済みの罠）。
//   - **書込は tmp→rename 原子化**: truncate 直書きは 0B の瞬間が実観測
//     される（cm WriteStatus 教訓）。plist もバイナリも tmp→rename。
//   - **稼働バイナリと開発ビルドの分離**: launchd の Program を repo の
//     ビルド成果物に向けると `make build` が稼働 daemon を SIGKILL する
//     （cm 実障害）。~/.herdr-drover/bin/ へコピーして分離する。

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// launchdLabel は launchd job のラベル（plist ファイル名も <label>.plist）。
const launchdLabel = "com.4noha.herdr-drover"

// launchdPATH は plist に焼き込む PATH。launchd 起動のプロセスは shell rc を
// 読まないため、bridge が `herdr` CLI を PATH 解決（internal/bridge の
// HerdrBin 既定 "herdr"）できるよう Homebrew を前置する（cm cloud plist と
// 同値の運用実績）。
const launchdPATH = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"

// installPaths は install/uninstall が触るパス一式（home 注入で単体テストは
// 一時 HOME に完全隔離できる＝実 launchctl / 実 ~/Library に触れない）。
type installPaths struct {
	baseDir    string // ~/.herdr-drover
	binDst     string // ~/.herdr-drover/bin/herdr-drover（稼働バイナリ＝開発ビルドと分離）
	plistPath  string // ~/Library/LaunchAgents/com.4noha.herdr-drover.plist
	logPath    string // ~/.herdr-drover/agent.log
	configPath string // ~/.herdr-drover/config（KEY=VALUE。env 未設定時の解決元）
	// cliBinPath は install.sh が置く「ユーザー PATH 上の CLI バイナリ」。
	// 通常 $HD_BINDIR / $HOME/.local/bin/herdr-drover。既定は $HOME/.local/bin。
	// 「エディタで alias claude が使うのはこれ・selfupdate は binDst しか触らず
	// stale になる」実バグ（v0.5.5 で実測）の再発防止用。selfupdate 経路が
	// **binDst と一緒に cliBinPath も同期**するのに使う。
	cliBinPath string
}

func resolveInstallPaths(home string) installPaths {
	base := filepath.Join(home, ".herdr-drover")
	// CLI パスは HD_BINDIR env で override 可能（install.sh と揃える）。既定は
	// ~/.local/bin/herdr-drover（install.sh の既定と一致）。
	cliBinDir := os.Getenv("HD_BINDIR")
	if cliBinDir == "" {
		cliBinDir = filepath.Join(home, ".local", "bin")
	}
	return installPaths{
		baseDir:    base,
		binDst:     filepath.Join(base, "bin", "herdr-drover"),
		plistPath:  filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist"),
		logPath:    filepath.Join(base, "agent.log"),
		configPath: filepath.Join(base, "config"),
		cliBinPath: filepath.Join(cliBinDir, "herdr-drover"),
	}
}

// envKV は plist EnvironmentVariables の 1 項目（map でなく slice＝生成順を
// 決定論にして「再実行で同一 plist」の冪等性を機械検証可能にする）。
type envKV struct{ k, v string }

// loadInstallConfigFile は ~/.herdr-drover/config を読む。形式は 1 行 1 つの
// KEY=VALUE（`#` 行コメント・空行可・先頭の `export ` は無視・値の両端
// クォートは剥がす）。シェル解釈は一切しない（`$VAR` 展開等はしない＝
// exact なテキストのみ。ヒューリスティック禁止の規律）。
// ファイル不在はエラーでなく空 map（env だけで解決する運用を許す）。
func loadInstallConfigFile(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("設定ファイル %s が読めない: %w", path, err)
	}
	m := map[string]string{}
	for i, line := range strings.Split(string(b), "\n") {
		s := strings.TrimSpace(line)
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		s = strings.TrimPrefix(s, "export ") // sidecar .env 慣習（cm ASC env と同形）を許容
		k, v, ok := strings.Cut(s, "=")
		if !ok || strings.TrimSpace(k) == "" {
			return nil, fmt.Errorf("設定ファイル %s の %d 行目が KEY=VALUE でない: %q", path, i+1, line)
		}
		v = strings.TrimSpace(v)
		// 両端が同じクォートなら 1 組だけ剥がす（"a b" / 'a b'）。中間の
		// クォートには触れない＝シェル解釈はしない。
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		m[strings.TrimSpace(k)] = v
	}
	return m, nil
}

// mergeEnrollConfig は enroll が書く ~/.herdr-drover/config.json（JSON・
// resolveConfig と同じ 4 キー）を KEY=VALUE 設定の**最終 fallback** として
// 合流させる（fileCfg に無いキーだけ埋める＝優先順は呼び手コメント参照）。
//
// これが無いと正規オンボーディング enroll→install が成立しない実バグに
// なる: enroll は config.json を書くのに install が KEY=VALUE の config
// しか読まず、env なしの install が「GCP_PROJECT が解決できない」で必ず
// 失敗する（agent の resolveConfig は config.json を拾って動くのに install
// だけ拒否する非対称）。pc_id も同様で、config.json の手動 pc_id を無視して
// hostname 既定を plist に焼くと、実行時 env > file により config.json 側が
// 黙って上書きされ Firestore doc の連続性・revocation 対象 id が乖離する。
// キー対応は fileConfig の 4 フィールドと env 名の exact-match のみ
// （ヒューリスティック禁止の規律）。
func mergeEnrollConfig(fileCfg map[string]string, fc fileConfig) {
	for _, kv := range []struct{ k, v string }{
		{"GCP_PROJECT", fc.GCPProject},
		{"CLOUD_RELAY_URL", fc.CloudRelayURL},
		{"GOOGLE_APPLICATION_CREDENTIALS", fc.GoogleApplicationCredentials},
		{"PC_ID", fc.PCID},
	} {
		if fileCfg[kv.k] == "" && kv.v != "" {
			fileCfg[kv.k] = kv.v
		}
	}
}

// resolveInstallEnv は plist に焼き込む EnvironmentVariables を解決する。
// 優先順: 現在の環境変数 > ~/.herdr-drover/config > 呼び手が
// mergeEnrollConfig 済みの ~/.herdr-drover/config.json（env の空文字は
// 「未設定」と同義＝resolveConfig と同じ扱い）。
//
// 焼き込む理由: launchd 起動のプロセスは shell rc を読まない＝実行時 env に
// 頼れない。cm と同じく plist へ値を固定する（変更は install 再実行）。
//   - GCP_PROJECT は agent 必須（無いと KeepAlive が crash-loop するだけ
//     なので install 時点で fail-fast する）
//   - PC_ID は未設定なら defaultPCID(hostname) を焼き込む＝以後 hostname が
//     変わっても id が安定する（Firestore doc の連続性）
//   - 空のオプション値は焼かない（省略＝agent 側の既定/警告に委ねる）
func resolveInstallEnv(fileCfg map[string]string) ([]envKV, error) {
	get := func(key string) string {
		if v := os.Getenv(key); v != "" {
			return v
		}
		return fileCfg[key]
	}
	project := get("GCP_PROJECT")
	if project == "" {
		return nil, fmt.Errorf("GCP_PROJECT が解決できない（agent 必須＝KeepAlive の crash-loop を防ぐため install を拒否）。環境変数で渡すか ~/.herdr-drover/config に GCP_PROJECT=<project> を書くか、`herdr-drover enroll` を先に実行すること（config.json も読む）")
	}
	pcid := get("PC_ID")
	if pcid == "" {
		host, err := os.Hostname()
		if err != nil {
			return nil, fmt.Errorf("PC_ID 未設定かつ hostname 取得失敗: %w", err)
		}
		pcid = defaultPCID(host)
	}
	env := []envKV{
		{"PATH", launchdPATH},
		{"GCP_PROJECT", project},
	}
	// 任務指定の 4 キー（GCP_PROJECT/CLOUD_RELAY_URL/GOOGLE_APPLICATION_
	// CREDENTIALS/PC_ID）＋agent が読む追加 knob。順序固定＝plist 生成の
	// 決定論（冪等性テストの前提）。
	for _, key := range []string{"CLOUD_RELAY_URL", "GOOGLE_APPLICATION_CREDENTIALS"} {
		if v := get(key); v != "" {
			env = append(env, envKV{key, v})
		}
	}
	env = append(env, envKV{"PC_ID", pcid})
	for _, key := range []string{"HERDR_SOCKET_PATH", "DROVER_TICK", "DROVER_IDLE"} {
		if v := get(key); v != "" {
			env = append(env, envKV{key, v})
		}
	}
	return env, nil
}

// xmlEscape は plist の <string> 値エスケープ（URL の & や path の特殊文字が
// XML を壊さないように）。
var xmlEscape = strings.NewReplacer(
	"&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;",
).Replace

// buildLaunchdPlist は plist 本文を生成する純関数（入力が同じなら byte 同一
// ＝冪等）。ProcessType を**書かない**ことが仕様（理由はファイル冒頭コメント
// と生成物内コメント。回帰: TestInstallWritesPlistAndBinary の
// `<key>ProcessType</key>` 不在検査）。
func buildLaunchdPlist(home string, p installPaths, env []envKV) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<!-- generated by herdr-drover install（再実行で再生成される＝手編集しない）。
     ProcessType は意図的に未指定: ProcessType=Background は daemon と exec 子を
     throttle し scan timeout→空同期 flap の正帰還を起こす（cm 実障害の教訓）。 -->
<dict>
	<key>Label</key>
	<string>` + xmlEscape(launchdLabel) + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + xmlEscape(p.binDst) + `</string>
		<string>agent</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>EnvironmentVariables</key>
	<dict>
`)
	for _, e := range env {
		b.WriteString("\t\t<key>" + xmlEscape(e.k) + "</key>\n")
		b.WriteString("\t\t<string>" + xmlEscape(e.v) + "</string>\n")
	}
	b.WriteString(`	</dict>
	<key>StandardOutPath</key>
	<string>` + xmlEscape(p.logPath) + `</string>
	<key>StandardErrorPath</key>
	<string>` + xmlEscape(p.logPath) + `</string>
	<key>WorkingDirectory</key>
	<string>` + xmlEscape(home) + `</string>
</dict>
</plist>
`)
	return []byte(b.String())
}

// writeFileAtomic は tmp→rename で書く（cm WriteStatus 教訓: truncate 直書き
// は 0B の瞬間が実観測される）。tmp 名は CreateTemp で一意（pidfile.go の
// 並行 writer 実測バグと同じ理由で固定 tmp 名は使わない）。
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// placeBinaryNewInode は src（通常 os.Executable）を dst へ「rm→新 inode
// 配置」で置く。macOS の署名キャッシュ罠（同 inode への in-place 上書きは
// exec SIGKILL）対策＝cm の稼働 daemon 反映手順そのもの。
//
// 実装は read-all → rm → tmp write → rename:
//   - 先に src を読み切るのは、dst==src（インストール済みバイナリ自身から
//     の再 install）でも rm で自分を消してから読めなくなる事故を防ぐため
//   - rename は rm 後の新 inode を保証しつつ原子的（部分書きの dst を
//     launchd が exec し得る窓を作らない）
func placeBinaryNewInode(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("配置元バイナリ %s が読めない: %w", src, err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.Remove(dst); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("旧バイナリ %s の除去に失敗（新 inode 配置の前提）: %w", dst, err)
	}
	return writeFileAtomic(dst, data, 0o755)
}

// runLaunchctl は launchctl を実行する（stderr/stdout は呼び手の writer へ
// 素通し＝失敗理由を隠さない。cm tmux.go の「stderr を捨てて silent fail」
// 教訓）。
func runLaunchctl(stdout, stderr io.Writer, args ...string) error {
	cmd := exec.Command("launchctl", args...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	return cmd.Run()
}

// cmdInstall — `herdr-drover install [--dry-run] [--no-launchctl]`。
// 冪等: 再実行はバイナリ再配置（新 inode）＋plist 再生成＋launchd 再ロード。
func cmdInstall(args []string, stdout, stderr io.Writer) error {
	fl := flag.NewFlagSet("install", flag.ContinueOnError)
	fl.SetOutput(stderr)
	dryRun := fl.Bool("dry-run", false, "何も変更せず、行う予定の操作と生成 plist を表示する")
	noLaunchctl := fl.Bool("no-launchctl", false, "launchctl の実行を抑止する（テスト用。ファイル配置のみ行う）")
	if err := fl.Parse(args); err != nil {
		return err
	}
	if fl.NArg() != 0 {
		return fmt.Errorf("余分な引数 %v（install はフラグのみ取る）", fl.Args())
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("home ディレクトリ不明: %w", err)
	}
	p := resolveInstallPaths(home)
	fileCfg, err := loadInstallConfigFile(p.configPath)
	if err != nil {
		return err
	}
	// enroll の永続設定（config.json）を最終 fallback に合流＝正規動線
	// enroll→install を成立させる（詳細は mergeEnrollConfig コメント）。
	// 壊れた JSON は install 拒否: env に全値があっても黙って進めると
	// 「enroll 済のはずの値と違う常駐」が plist に恒久化する（resolveConfig
	// の「沈黙で無視しない」規律と同じ理由で loud に失敗する）。
	jsonPath, err := configFilePath()
	if err != nil {
		return err
	}
	fc, err := readFileConfig(jsonPath)
	if err != nil {
		return err
	}
	mergeEnrollConfig(fileCfg, fc)
	env, err := resolveInstallEnv(fileCfg)
	if err != nil {
		return err
	}
	src, err := os.Executable()
	if err != nil {
		return fmt.Errorf("自バイナリのパスが取れない: %w", err)
	}
	plist := buildLaunchdPlist(home, p, env)

	if *dryRun {
		fmt.Fprintf(stdout, "[dry-run] 変更なし。実行した場合の操作:\n")
		fmt.Fprintf(stdout, "  1. %s → %s へ配置（rm→新 inode）\n", src, p.binDst)
		fmt.Fprintf(stdout, "  2. %s を生成（tmp→rename）:\n", p.plistPath)
		stdout.Write(plist)
		if *noLaunchctl {
			fmt.Fprintf(stdout, "  3. launchctl はスキップ（--no-launchctl）\n")
		} else {
			fmt.Fprintf(stdout, "  3. launchctl bootout（既存があれば）→ bootstrap gui/%d %s\n", os.Getuid(), p.plistPath)
		}
		return nil
	}

	if err := placeBinaryNewInode(src, p.binDst); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "✔ バイナリ配置: %s（新 inode）\n", p.binDst)

	if err := writeFileAtomic(p.plistPath, plist, 0o644); err != nil {
		return fmt.Errorf("plist 書込失敗 %s: %w", p.plistPath, err)
	}
	fmt.Fprintf(stdout, "✔ plist 生成: %s\n", p.plistPath)

	if *noLaunchctl {
		fmt.Fprintf(stdout, "launchctl はスキップ（--no-launchctl）。手動ロード:\n")
		fmt.Fprintf(stdout, "  launchctl bootstrap gui/%d %s\n", os.Getuid(), p.plistPath)
		return nil
	}
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("launchd は macOS のみ（この OS では --no-launchctl でファイル配置だけ行い、常駐は手動で設定すること）")
	}
	// 冪等な再ロード: 既ロードなら一度 bootout（未ロードのエラーは正常系
	// なので無視）→ bootstrap（RunAtLoad=true が即起動する）。
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	_ = runLaunchctl(io.Discard, io.Discard, "bootout", domain+"/"+launchdLabel)
	if err := runLaunchctl(stdout, stderr, "bootstrap", domain, p.plistPath); err != nil {
		return fmt.Errorf("launchctl bootstrap 失敗: %w", err)
	}
	fmt.Fprintf(stdout, "✔ launchd 登録・起動: %s（ログ: %s）\n", launchdLabel, p.logPath)
	return nil
}

// cmdUninstall — `herdr-drover uninstall [--dry-run] [--no-launchctl]`。
// launchd job を落とし plist と稼働バイナリを除去する。設定
// （~/.herdr-drover/config）とログは残す（再 install で再利用・障害調査の
// 一次情報を消さない）。冪等: 対象が無ければその旨を表示して成功。
func cmdUninstall(args []string, stdout, stderr io.Writer) error {
	fl := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	fl.SetOutput(stderr)
	dryRun := fl.Bool("dry-run", false, "何も変更せず、行う予定の操作を表示する")
	noLaunchctl := fl.Bool("no-launchctl", false, "launchctl の実行を抑止する（テスト用。ファイル除去のみ行う）")
	if err := fl.Parse(args); err != nil {
		return err
	}
	if fl.NArg() != 0 {
		return fmt.Errorf("余分な引数 %v（uninstall はフラグのみ取る）", fl.Args())
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("home ディレクトリ不明: %w", err)
	}
	p := resolveInstallPaths(home)

	if *dryRun {
		fmt.Fprintf(stdout, "[dry-run] 変更なし。実行した場合の操作:\n")
		if !*noLaunchctl {
			fmt.Fprintf(stdout, "  1. launchctl bootout gui/%d/%s\n", os.Getuid(), launchdLabel)
		}
		fmt.Fprintf(stdout, "  2. rm %s\n  3. rm %s\n", p.plistPath, p.binDst)
		fmt.Fprintf(stdout, "  （%s と %s は残す）\n", p.configPath, p.logPath)
		return nil
	}

	if !*noLaunchctl && runtime.GOOS == "darwin" {
		// 未ロードの bootout はエラーになるが冪等性のため無視（正常系）。
		_ = runLaunchctl(io.Discard, io.Discard, "bootout", fmt.Sprintf("gui/%d/%s", os.Getuid(), launchdLabel))
		fmt.Fprintf(stdout, "✔ launchd 解除（未ロードなら no-op）: %s\n", launchdLabel)
	}
	for _, path := range []string{p.plistPath, p.binDst} {
		if err := os.Remove(path); errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(stdout, "- %s は元々無い（冪等）\n", path)
		} else if err != nil {
			return fmt.Errorf("除去失敗 %s: %w", path, err)
		} else {
			fmt.Fprintf(stdout, "✔ 除去: %s\n", path)
		}
	}
	fmt.Fprintf(stdout, "設定とログは残置: %s / %s\n", p.configPath, p.logPath)
	return nil
}
