package main

// install/uninstall のテスト。鉄則:
//   - HOME を一時 dir に向けて実 run() 経路（実バイナリと同じ dispatch→
//     flag 解析→ファイル配置）で検証する。合成の別経路を作らない
//   - **実 launchctl / 実 ~/Library/LaunchAgents には絶対に触れない**:
//     全テストが --no-launchctl ＋ t.Setenv("HOME", tempdir)。実 launchctl
//     ロードはカットオーバー作業（ワークフロー外）に委ねる
//   - plist の構文は plutil -lint（実 macOS パーサ）で機械検証（不在 OS では
//     その検査だけ skip）

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// setupInstallHome は HOME を一時 dir へ隔離し、install が解決する env を
// 決定論の値に固定する。
func setupInstallHome(t *testing.T) (home string) {
	t.Helper()
	clearDroverEnv(t)
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GCP_PROJECT", "proj-install-test")
	t.Setenv("CLOUD_RELAY_URL", "wss://relay.example/session?a=1&b=2") // & で XML escape も同時検証
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "/tmp/sa-test.json")
	t.Setenv("PC_ID", "testbox-herdr")
	return home
}

func installedPaths(t *testing.T, home string) installPaths {
	t.Helper()
	return resolveInstallPaths(home)
}

func TestInstallWritesPlistAndBinary(t *testing.T) {
	home := setupInstallHome(t)
	code, out, errb := runCapture(t, "install", "--no-launchctl")
	if code != 0 {
		t.Fatalf("install exit=%d stderr=%q", code, errb)
	}
	p := installedPaths(t, home)

	// --- バイナリ配置（稼働バイナリは repo ビルドと分離＝cm 教訓）---
	st, err := os.Stat(p.binDst)
	if err != nil {
		t.Fatalf("稼働バイナリが配置されていない: %v", err)
	}
	if st.Mode().Perm() != 0o755 {
		t.Fatalf("バイナリ mode=%v want 0755", st.Mode().Perm())
	}
	self, _ := os.Executable()
	if selfSt, err := os.Stat(self); err == nil && st.Size() != selfSt.Size() {
		t.Fatalf("配置バイナリのサイズが自バイナリと不一致: %d != %d", st.Size(), selfSt.Size())
	}

	// --- plist 内容 ---
	b, err := os.ReadFile(p.plistPath)
	if err != nil {
		t.Fatalf("plist が無い: %v", err)
	}
	s := string(b)
	// ProcessType キー不在は仕様（Background 禁止＝cm STATUS flap 教訓）。
	// launchd が読むのは <key> 要素＝それを exact-match で検査する（生成物内の
	// 説明コメントは「意図的に未指定」の根拠として ProcessType に言及してよい）。
	if strings.Contains(s, "<key>ProcessType</key>") {
		t.Fatalf("plist に ProcessType キーがある（Background 禁止どころか指定自体禁止）:\n%s", s)
	}
	if !strings.Contains(s, "ProcessType は意図的に未指定") {
		t.Fatalf("plist に ProcessType 未指定の根拠コメントが無い（cm 教訓の伝承）:\n%s", s)
	}
	for _, want := range []string{
		"<string>" + launchdLabel + "</string>",
		"<key>RunAtLoad</key>",
		"<key>KeepAlive</key>",
		"<string>" + p.binDst + "</string>",
		"<string>agent</string>",
		"<string>proj-install-test</string>",
		"<string>wss://relay.example/session?a=1&amp;b=2</string>", // XML escape
		"<string>/tmp/sa-test.json</string>",
		"<string>testbox-herdr</string>",
		"<string>" + p.logPath + "</string>",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("plist に %q が無い:\n%s", want, s)
		}
	}
	// RunAtLoad/KeepAlive は <true/>（launchd の bool 形式）で入っていること。
	if strings.Count(s, "<true/>") < 2 {
		t.Fatalf("RunAtLoad/KeepAlive の <true/> が 2 つ無い:\n%s", s)
	}
	// 4 必須キーが EnvironmentVariables に全て焼き込まれていること。
	for _, key := range []string{"GCP_PROJECT", "CLOUD_RELAY_URL", "GOOGLE_APPLICATION_CREDENTIALS", "PC_ID"} {
		if !strings.Contains(s, "<key>"+key+"</key>") {
			t.Fatalf("EnvironmentVariables に %s が無い:\n%s", key, s)
		}
	}

	// --- plutil -lint（実 macOS plist パーサでの構文検証）---
	if _, err := exec.LookPath("plutil"); err == nil {
		if lint, err := exec.Command("plutil", "-lint", p.plistPath).CombinedOutput(); err != nil {
			t.Fatalf("plutil -lint 失敗: %v\n%s", err, lint)
		}
	} else {
		t.Logf("plutil 不在（非 macOS）＝lint 検査のみ skip")
	}

	if !strings.Contains(out, p.plistPath) {
		t.Fatalf("stdout に plist パス報告が無い: %q", out)
	}
}

// 冪等な再実行＝2 回目も成功し、バイナリは**新 inode** で置き直される
// （macOS 署名キャッシュ罠: 同 inode への in-place 上書きは exec SIGKILL）。
func TestInstallIdempotentAndNewInode(t *testing.T) {
	home := setupInstallHome(t)
	p := installedPaths(t, home)

	if code, _, errb := runCapture(t, "install", "--no-launchctl"); code != 0 {
		t.Fatalf("install#1 exit=%d stderr=%q", code, errb)
	}
	st1, err := os.Stat(p.binDst)
	if err != nil {
		t.Fatal(err)
	}
	ino1 := st1.Sys().(*syscall.Stat_t).Ino
	plist1, _ := os.ReadFile(p.plistPath)

	if code, _, errb := runCapture(t, "install", "--no-launchctl"); code != 0 {
		t.Fatalf("install#2 exit=%d stderr=%q", code, errb)
	}
	st2, err := os.Stat(p.binDst)
	if err != nil {
		t.Fatal(err)
	}
	ino2 := st2.Sys().(*syscall.Stat_t).Ino
	if ino1 == ino2 {
		t.Fatalf("再 install でバイナリ inode が変わっていない（in-place 上書き＝署名キャッシュ罠）: ino=%d", ino1)
	}
	plist2, _ := os.ReadFile(p.plistPath)
	if string(plist1) != string(plist2) {
		t.Fatalf("同一入力で plist が byte 不一致（生成が非決定論）:\n#1:\n%s\n#2:\n%s", plist1, plist2)
	}
}

func TestInstallDryRunChangesNothing(t *testing.T) {
	home := setupInstallHome(t)
	p := installedPaths(t, home)
	code, out, errb := runCapture(t, "install", "--dry-run", "--no-launchctl")
	if code != 0 {
		t.Fatalf("dry-run exit=%d stderr=%q", code, errb)
	}
	if _, err := os.Stat(p.plistPath); !os.IsNotExist(err) {
		t.Fatalf("dry-run で plist が作られた")
	}
	if _, err := os.Stat(p.binDst); !os.IsNotExist(err) {
		t.Fatalf("dry-run でバイナリが配置された")
	}
	// dry-run は生成予定の plist 本文を見せる（目視確認用）。
	if !strings.Contains(out, "<key>GCP_PROJECT</key>") || !strings.Contains(out, "dry-run") {
		t.Fatalf("dry-run 出力に plist プレビューが無い: %q", out)
	}
}

// GCP_PROJECT 不能は install 拒否（入れてしまうと KeepAlive の crash-loop）。
func TestInstallRequiresProject(t *testing.T) {
	clearDroverEnv(t)
	t.Setenv("HOME", t.TempDir())
	code, _, errb := runCapture(t, "install", "--no-launchctl")
	if code != 1 {
		t.Fatalf("exit=%d want 1", code)
	}
	if !strings.Contains(errb, "GCP_PROJECT") || !strings.Contains(errb, "config") {
		t.Fatalf("GCP_PROJECT 未解決の案内（env or ~/.herdr-drover/config）が無い: %q", errb)
	}
}

// ~/.herdr-drover/config からの解決と、env > file の優先順。
func TestInstallConfigFileFallbackAndPrecedence(t *testing.T) {
	clearDroverEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	p := resolveInstallPaths(home)
	if err := os.MkdirAll(p.baseDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := `# herdr-drover install の解決元（KEY=VALUE・シェル解釈なし）
GCP_PROJECT=proj-from-file
export CLOUD_RELAY_URL="wss://file.example/session"
PC_ID=filebox-herdr
`
	if err := os.WriteFile(p.configPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	// PC_ID だけ env で上書き → env が勝つこと。
	t.Setenv("PC_ID", "envbox-herdr")

	code, _, errb := runCapture(t, "install", "--no-launchctl")
	if code != 0 {
		t.Fatalf("install exit=%d stderr=%q", code, errb)
	}
	b, err := os.ReadFile(p.plistPath)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, "<string>proj-from-file</string>") {
		t.Fatalf("config ファイルの GCP_PROJECT が焼かれていない:\n%s", s)
	}
	if !strings.Contains(s, "<string>wss://file.example/session</string>") {
		t.Fatalf("export 付き・クォート付き値が解決されていない:\n%s", s)
	}
	if !strings.Contains(s, "<string>envbox-herdr</string>") || strings.Contains(s, "filebox-herdr") {
		t.Fatalf("env > config の優先順が破れている:\n%s", s)
	}
}

// PC_ID が env にも config にも無ければ defaultPCID(hostname) を焼き込む
// （以後 hostname が変わっても id 安定）。
func TestInstallBakesDefaultPCID(t *testing.T) {
	clearDroverEnv(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GCP_PROJECT", "proj-x")
	home := os.Getenv("HOME")
	code, _, errb := runCapture(t, "install", "--no-launchctl")
	if code != 0 {
		t.Fatalf("install exit=%d stderr=%q", code, errb)
	}
	b, err := os.ReadFile(resolveInstallPaths(home).plistPath)
	if err != nil {
		t.Fatal(err)
	}
	host, _ := os.Hostname()
	want := "<string>" + defaultPCID(host) + "</string>"
	if !strings.Contains(string(b), want) {
		t.Fatalf("既定 PC_ID %q が焼かれていない:\n%s", want, b)
	}
}

func TestUninstallRemovesArtifactsKeepsConfigAndLog(t *testing.T) {
	home := setupInstallHome(t)
	p := installedPaths(t, home)
	// config とログを事前に置く（uninstall が残すべきもの）。
	if err := os.MkdirAll(p.baseDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.configPath, []byte("GCP_PROJECT=keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.logPath, []byte("log\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if code, _, errb := runCapture(t, "install", "--no-launchctl"); code != 0 {
		t.Fatalf("install exit=%d stderr=%q", code, errb)
	}
	if code, _, errb := runCapture(t, "uninstall", "--no-launchctl"); code != 0 {
		t.Fatalf("uninstall exit=%d stderr=%q", code, errb)
	}
	if _, err := os.Stat(p.plistPath); !os.IsNotExist(err) {
		t.Fatalf("plist が残っている")
	}
	if _, err := os.Stat(p.binDst); !os.IsNotExist(err) {
		t.Fatalf("稼働バイナリが残っている")
	}
	if _, err := os.Stat(p.configPath); err != nil {
		t.Fatalf("config が消された（残す仕様）: %v", err)
	}
	if _, err := os.Stat(p.logPath); err != nil {
		t.Fatalf("ログが消された（残す仕様）: %v", err)
	}
	// 冪等: 2 回目の uninstall も成功（対象なしは正常系）。
	if code, out, errb := runCapture(t, "uninstall", "--no-launchctl"); code != 0 {
		t.Fatalf("uninstall#2 exit=%d stderr=%q", code, errb)
	} else if !strings.Contains(out, "元々無い") {
		t.Fatalf("冪等 no-op の報告が無い: %q", out)
	}
}

// フラグ解析の異常系: 未知フラグと余分な位置引数は明示エラー。
func TestInstallRejectsBadArgs(t *testing.T) {
	setupInstallHome(t)
	if code, _, _ := runCapture(t, "install", "--bogus"); code != 1 {
		t.Fatalf("未知フラグ exit=%d want 1", code)
	}
	if code, _, errb := runCapture(t, "install", "now", "--no-launchctl"); code != 1 || !strings.Contains(errb, "余分な引数") {
		t.Fatalf("余分引数 exit=%d stderr=%q", code, errb)
	}
}

// 【指摘再現】正規オンボーディング enroll→install。enroll は
// ~/.herdr-drover/config.json（JSON）を書くのに、install が
// ~/.herdr-drover/config（KEY=VALUE）しか読まないと、env なしの install が
// 「GCP_PROJECT が解決できない」で必ず失敗する（enroll の launchd 常駐案内
// どおりに進むと詰む）。実 fake enroll server→実 install 経路で検証。
func TestInstallResolvesFromEnrollConfigJSON(t *testing.T) {
	clearDroverEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	// sa_json は資格情報として無効な JSON＝enroll 末尾の ClearRevoked
	// best-effort が即エラー skip し、テストが GCP へ出ない（enroll_test 同様）。
	wsURL := fakeEnrollServer(t, "INST0001", "proj-enrolled", "wss://relay.enrolled.example", `{"fake":"sa"}`)
	if code, out, errb := runCapture(t, "enroll", "INST0001", "--relay", wsURL); code != 0 {
		t.Fatalf("enroll exit=%d\nstdout:%s\nstderr:%s", code, out, errb)
	}

	code, _, errb := runCapture(t, "install", "--no-launchctl")
	if code != 0 {
		t.Fatalf("enroll 済み HOME で install が失敗（config.json fallback 不在）: exit=%d stderr=%q", code, errb)
	}
	p := resolveInstallPaths(home)
	b, err := os.ReadFile(p.plistPath)
	if err != nil {
		t.Fatalf("plist が無い: %v", err)
	}
	s := string(b)
	for _, want := range []string{
		"<string>proj-enrolled</string>",
		"<string>wss://relay.enrolled.example</string>",
		"<string>" + filepath.Join(home, ".herdr-drover", "sa.json") + "</string>",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("enroll の設定が plist に焼かれていない（%q 不在）:\n%s", want, s)
		}
	}
}

// 【指摘再現】config.json の pc_id 手動設定を install も尊重する。旧実装は
// env/hostname 由来 PC_ID を plist に焼くため、実行時 env > file で
// config.json の pc_id が黙って上書きされ Firestore doc の連続性が乖離する。
func TestInstallRespectsConfigJSONPCID(t *testing.T) {
	clearDroverEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".herdr-drover")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"),
		[]byte(`{"gcp_project":"proj-json","pc_id":"custom-json-herdr"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, errb := runCapture(t, "install", "--no-launchctl")
	if code != 0 {
		t.Fatalf("install exit=%d stderr=%q", code, errb)
	}
	b, err := os.ReadFile(resolveInstallPaths(home).plistPath)
	if err != nil {
		t.Fatal(err)
	}
	host, _ := os.Hostname()
	if !strings.Contains(string(b), "<string>custom-json-herdr</string>") ||
		strings.Contains(string(b), "<string>"+defaultPCID(host)+"</string>") {
		t.Fatalf("config.json の pc_id が尊重されず hostname 既定が焼かれた:\n%s", b)
	}
}

// 優先順は env > config（KEY=VALUE）> config.json。KEY=VALUE にあるキーは
// それが勝ち、無いキーだけ config.json から補完される。
func TestInstallPrecedenceKVOverJSON(t *testing.T) {
	clearDroverEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	p := resolveInstallPaths(home)
	if err := os.MkdirAll(p.baseDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.configPath, []byte("GCP_PROJECT=proj-kv\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.baseDir, "config.json"),
		[]byte(`{"gcp_project":"proj-json","cloud_relay_url":"wss://json.example"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, errb := runCapture(t, "install", "--no-launchctl")
	if code != 0 {
		t.Fatalf("install exit=%d stderr=%q", code, errb)
	}
	b, err := os.ReadFile(p.plistPath)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, "<string>proj-kv</string>") || strings.Contains(s, "proj-json") {
		t.Fatalf("config（KEY=VALUE）> config.json の優先順が破れている:\n%s", s)
	}
	if !strings.Contains(s, "<string>wss://json.example</string>") {
		t.Fatalf("KEY=VALUE に無いキーが config.json から補完されていない:\n%s", s)
	}
}

// 壊れた config.json は install 拒否（黙って env/hostname だけで焼くと
// 「enroll 済のはずの値と違う常駐」が恒久化する＝resolveConfig の「沈黙で
// 無視しない」規律と同じ。env に全値があっても enroll 状態の破損は loud に）。
func TestInstallRejectsBrokenConfigJSON(t *testing.T) {
	clearDroverEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GCP_PROJECT", "proj-env")
	dir := filepath.Join(home, ".herdr-drover")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, errb := runCapture(t, "install", "--no-launchctl")
	if code != 1 || !strings.Contains(errb, "config.json") {
		t.Fatalf("壊れた config.json が拒否されない: exit=%d stderr=%q", code, errb)
	}
}

// loadInstallConfigFile の単体（コメント/空行/export/クォート/壊れ行）。
func TestLoadInstallConfigFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")

	// 不在は空 map（env のみ運用を許す）
	m, err := loadInstallConfigFile(path)
	if err != nil || len(m) != 0 {
		t.Fatalf("不在: m=%v err=%v", m, err)
	}

	body := "# comment\n\nA=1\nexport B=\"two words\"\nC='x=y'\n D = spaced \n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err = loadInstallConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"A": "1", "B": "two words", "C": "x=y", "D": "spaced"}
	for k, v := range want {
		if m[k] != v {
			t.Fatalf("m[%q]=%q want %q（全体: %v）", k, m[k], v, m)
		}
	}

	// KEY=VALUE でない行は行番号付きで明示エラー（黙って捨てない）
	if err := os.WriteFile(path, []byte("A=1\nbroken line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadInstallConfigFile(path); err == nil || !strings.Contains(err.Error(), "2 行目") {
		t.Fatalf("壊れ行の明示エラーが無い: %v", err)
	}
}
