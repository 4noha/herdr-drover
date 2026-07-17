package selfupdate

// cm selfupdate_test.go のパターン移植（ヘッダ契約・httpErr）＋
// ローカル HTTP fixture での Update 全経路検証（実 HTTP・実ファイル置換・
// sha256 検証。実 GitHub には一切出ない＝seam apiBase/dlBase/osExecutable）。

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setGHHeaders 必須挙動: UA 明示・Accept・GITHUB_TOKEN 有無で
// Authorization を自動制御。UA 未設定 = GitHub が即 403 で拒否する規約
// への境界（cm 実環境で `更新` が 403 を 3 連発した真因の固定回帰）。
func TestSetGHHeadersDefault(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	req, _ := http.NewRequest("GET", "http://example.com", nil)
	setGHHeaders(req)
	if got := req.Header.Get("User-Agent"); got != UserAgent {
		t.Fatalf("UA: got=%q want=%q", got, UserAgent)
	}
	if got := req.Header.Get("Accept"); got != "application/vnd.github+json" {
		t.Fatalf("Accept: got=%q want=%q", got, "application/vnd.github+json")
	}
	if got := req.Header.Get("Authorization"); got != "" {
		t.Fatalf("GITHUB_TOKEN 未設定で Authorization 付与: %q", got)
	}
}

func TestSetGHHeadersWithToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "abc123")
	req, _ := http.NewRequest("GET", "http://example.com", nil)
	setGHHeaders(req)
	if got := req.Header.Get("Authorization"); got != "Bearer abc123" {
		t.Fatalf("Authorization: got=%q want=%q", got, "Bearer abc123")
	}
}

// httpErr は body 先頭 256B を error に含める＝`403 Forbidden` だけでは
// rate limit / UA 拒否 / 権限を判別不能だった cm 教訓の境界。
func TestHttpErrIncludesBody(t *testing.T) {
	resp := &http.Response{
		Status: "403 Forbidden",
		Body:   http.NoBody, // 空 body でも prefix/status は出る
	}
	err := httpErr("github api", resp)
	if err == nil {
		t.Fatal("error が nil")
	}
	s := err.Error()
	if !strings.Contains(s, "github api") || !strings.Contains(s, "403 Forbidden") || !strings.Contains(s, "body=") {
		t.Fatalf("error 形式が想定外: %q", s)
	}
}

// fixtureServer はローカル HTTP fixture（GitHub API/Releases の HTTP 契約
// 準拠のパス構成）を立て、seam を差し替える。checksums は引数で改竄可能に
// して sha256 検証の負経路も同じ器で通す。
func fixtureServer(t *testing.T, tag string, bin []byte, checksums string) (gotUA, gotAccept *string) {
	t.Helper()
	t.Setenv("DROVER_REPO", "") // 既定 Repo（4noha/herdr-drover）を強制
	name := assetName()
	var ua, accept string
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/4noha/herdr-drover/releases/latest",
		func(w http.ResponseWriter, r *http.Request) {
			ua = r.Header.Get("User-Agent")
			accept = r.Header.Get("Accept")
			fmt.Fprintf(w, `{"tag_name":%q}`, tag)
		})
	mux.HandleFunc("/4noha/herdr-drover/releases/latest/download/"+name,
		func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(bin) })
	mux.HandleFunc("/4noha/herdr-drover/releases/latest/download/checksums.txt",
		func(w http.ResponseWriter, r *http.Request) { _, _ = fmt.Fprint(w, checksums) })
	ts := httptest.NewServer(mux)
	oldAPI, oldDL := apiBase, dlBase
	apiBase, dlBase = ts.URL, ts.URL
	t.Cleanup(func() {
		apiBase, dlBase = oldAPI, oldDL
		ts.Close()
	})
	return &ua, &accept
}

// seamExecutable は置換先を一時ファイルに向ける（実行中テストバイナリを
// 書き換えない）。旧内容 0755 で作る＝実バイナリ配置と同型。
func seamExecutable(t *testing.T, oldContent []byte) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "herdr-drover")
	if err := os.WriteFile(exe, oldContent, 0o755); err != nil {
		t.Fatalf("exe fixture: %v", err)
	}
	oldExec := osExecutable
	osExecutable = func() (string, error) { return exe, nil }
	t.Cleanup(func() { osExecutable = oldExec })
	return exe
}

func TestUpdateViaLocalFixture(t *testing.T) {
	newBin := []byte("NEW-BINARY-v9.9.9\n")
	sum := sha256.Sum256(newBin)
	sums := fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), assetName())
	ua, accept := fixtureServer(t, "v9.9.9", newBin, sums)
	exe := seamExecutable(t, []byte("OLD-BINARY-v0.1.0\n"))

	tag, updated, err := Update("v0.1.0")
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if tag != "v9.9.9" || !updated {
		t.Fatalf("tag=%q updated=%v（v9.9.9/true のはず）", tag, updated)
	}
	got, err := os.ReadFile(exe)
	if err != nil {
		t.Fatalf("read exe: %v", err)
	}
	if string(got) != string(newBin) {
		t.Fatalf("バイナリが置換されていない: %q", got)
	}
	fi, _ := os.Stat(exe)
	if fi.Mode().Perm() != 0o755 {
		t.Fatalf("実行権限が %v（0755 のはず）", fi.Mode().Perm())
	}
	// HTTP 契約: 固有 UA と Accept が fixture に実到達している。
	if *ua != UserAgent || *accept != "application/vnd.github+json" {
		t.Fatalf("GitHub ヘッダ契約が守られていない: UA=%q Accept=%q", *ua, *accept)
	}

	// 既に最新（tag 一致・v prefix 正規化）→ 置換しない。
	tag2, updated2, err := Update("9.9.9")
	if err != nil || tag2 != "v9.9.9" || updated2 {
		t.Fatalf("既に最新の判定が壊れている: tag=%q updated=%v err=%v", tag2, updated2, err)
	}
}

func TestUpdateChecksumMismatchRejects(t *testing.T) {
	newBin := []byte("NEW-BINARY-evil\n")
	// 改竄 fixture: checksums は別内容の sha256 を返す。
	bogus := sha256.Sum256([]byte("something else"))
	sums := fmt.Sprintf("%s  %s\n", hex.EncodeToString(bogus[:]), assetName())
	fixtureServer(t, "v9.9.9", newBin, sums)
	oldContent := []byte("OLD-BINARY-v0.1.0\n")
	exe := seamExecutable(t, oldContent)

	_, _, err := Update("v0.1.0")
	if err == nil || !strings.Contains(err.Error(), "sha256 不一致") {
		t.Fatalf("sha256 検証が拒否しない: err=%v", err)
	}
	got, _ := os.ReadFile(exe)
	if string(got) != string(oldContent) {
		t.Fatalf("検証失敗なのにバイナリが書き換わっている: %q", got)
	}
}

func TestExpectedSHAExactMatch(t *testing.T) {
	sums := []byte("aaaa  other_asset\nBBBB  " + assetName() + "\n")
	sha, ok := expectedSHA(sums, assetName())
	if !ok || sha != "bbbb" {
		t.Fatalf("expectedSHA: sha=%q ok=%v（小文字化・名前 exact-match のはず）", sha, ok)
	}
	if _, ok := expectedSHA(sums, "no_such"); ok {
		t.Fatalf("不在 asset で ok=true")
	}
}
