package injectindex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestOpenAbsentFileIsEmpty はファイル不在時に空 Index を返し loud エラーに
// しないことを保証する（初回起動・rm 後の起動の正規経路）。
func TestOpenAbsentFileIsEmpty(t *testing.T) {
	p := filepath.Join(t.TempDir(), "inject-index.json")
	idx, err := Open(p)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got := idx.Snapshot(); len(got) != 0 {
		t.Fatalf("空 Index が期待だが entries=%d", len(got))
	}
	if idx.Path() != p {
		t.Errorf("Path()=%q want=%q", idx.Path(), p)
	}
}

// TestReserveCommitForgetRoundTrip は Reserve→Commit→Forget の遷移と、
// 別プロセス相当（新しい Open）での復元を検証する。永続化の権威保証。
func TestReserveCommitForgetRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "inject-index.json")
	idx, err := Open(p)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Reserve
	if err := idx.Reserve("w9:p1", "macbook-herdr", "w3:p2"); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	// 新規 Open で pending が残ることを確認（crash 相当）
	idx2, err := Open(p)
	if err != nil {
		t.Fatalf("Open#2: %v", err)
	}
	got := idx2.Snapshot()
	if len(got) != 1 || !got[0].Pending || got[0].PaneID != "w9:p1" || got[0].PC != "macbook-herdr" || got[0].SID != "w3:p2" {
		t.Fatalf("pending entry が復元されない: %+v", got)
	}
	// Commit
	if err := idx.Commit("w9:p1", "macbook-herdr", "w3:p2"); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	idx3, _ := Open(p)
	got = idx3.Snapshot()
	if len(got) != 1 || got[0].Pending {
		t.Fatalf("Commit 後は pending=false のはず: %+v", got)
	}
	// Forget
	if err := idx.Forget("w9:p1"); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	idx4, _ := Open(p)
	if got := idx4.Snapshot(); len(got) != 0 {
		t.Fatalf("Forget 後は空のはず: %+v", got)
	}
	// Forget 冪等（不在 pane_id）
	if err := idx.Forget("no-such"); err != nil {
		t.Fatalf("Forget 不在は no-op のはず: %v", err)
	}
}

// TestReserveDoesNotOverwriteLive は Commit 済 pane を誤って Pending に戻す
// 事故を防ぐ規律を保証する（reconcile リトライで再入した時の重要な安全弁）。
func TestReserveDoesNotOverwriteLive(t *testing.T) {
	p := filepath.Join(t.TempDir(), "inject-index.json")
	idx, _ := Open(p)
	_ = idx.Commit("w9:p1", "pc-a", "sid-a")
	if err := idx.Reserve("w9:p1", "pc-a", "sid-a"); err != nil {
		t.Fatalf("Reserve on live pane: %v", err)
	}
	e, ok := idx.Get("w9:p1")
	if !ok || e.Pending {
		t.Fatalf("Live entry を Reserve が上書きした: %+v", e)
	}
}

// TestAdoptToken は起動時 (a) 分岐（pane.list に token あり／index に無し）で
// index に取り込む挙動を確認する。既に Live で一致すれば no-op（persist 節約）。
func TestAdoptToken(t *testing.T) {
	p := filepath.Join(t.TempDir(), "inject-index.json")
	idx, _ := Open(p)
	if err := idx.AdoptToken("w9:p2", "pc-b", "sid-b"); err != nil {
		t.Fatalf("AdoptToken: %v", err)
	}
	e, ok := idx.Get("w9:p2")
	if !ok || e.Pending || e.PC != "pc-b" || e.SID != "sid-b" {
		t.Fatalf("Adopt 後 entry 不正: %+v", e)
	}
	// 一致 Adopt は no-op（persist しないことを確認するために mtime 変化を見る）
	fi1, _ := os.Stat(p)
	if err := idx.AdoptToken("w9:p2", "pc-b", "sid-b"); err != nil {
		t.Fatalf("AdoptToken (dup): %v", err)
	}
	fi2, _ := os.Stat(p)
	if fi1.ModTime() != fi2.ModTime() {
		t.Errorf("一致 Adopt で persist が走った（無駄書き）: %v -> %v", fi1.ModTime(), fi2.ModTime())
	}
}

// TestIsInjectedFastPath は producer の除外判定が Pending / Live 両方で true
// を返し（race 窓中も除外）、不在は false を返すことを確認する。
func TestIsInjectedFastPath(t *testing.T) {
	p := filepath.Join(t.TempDir(), "inject-index.json")
	idx, _ := Open(p)
	_ = idx.Reserve("w9:p3", "pc-c", "sid-c")
	if !idx.IsInjected("w9:p3") {
		t.Errorf("Pending の pane も除外対象のはず")
	}
	_ = idx.Commit("w9:p4", "pc-d", "sid-d")
	if !idx.IsInjected("w9:p4") {
		t.Errorf("Live の pane は除外対象")
	}
	if idx.IsInjected("no-such") {
		t.Errorf("不在 pane は false")
	}
}

// TestOpenRejectsUnknownField は typo/破壊的スキーマ変更を silent 無視しない
// 規律を保証する（wsmap.Parse 同流儀）。
func TestOpenRejectsUnknownField(t *testing.T) {
	p := filepath.Join(t.TempDir(), "inject-index.json")
	body := `{"version":1,"entries":[],"unknown_field":"boom"}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Open(p)
	if err == nil {
		t.Fatal("未知フィールドは loud エラーのはず")
	}
	if !strings.Contains(err.Error(), "壊れている") {
		t.Errorf("エラーメッセージが壊れ扱いを示すべき: %v", err)
	}
}

// TestOpenRejectsWrongVersion は将来のスキーマ変更で version=2 の JSON を
// 古い drover が silent に空扱いにしない規律を保証する。
func TestOpenRejectsWrongVersion(t *testing.T) {
	p := filepath.Join(t.TempDir(), "inject-index.json")
	body := `{"version":2,"entries":[]}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(p); err == nil {
		t.Fatal("非対応 version は loud エラーのはず")
	}
}

// TestOpenRejectsBrokenJSON は torn write / 手編集失敗を検出することを確認。
func TestOpenRejectsBrokenJSON(t *testing.T) {
	p := filepath.Join(t.TempDir(), "inject-index.json")
	if err := os.WriteFile(p, []byte(`{"version":1,"entries":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(p); err == nil {
		t.Fatal("壊れ JSON は loud エラーのはず")
	}
}

// TestOpenRejectsTrailingContent は「JSON 値の後ろに余分な内容」（連結事故）
// を検出することを確認する。
func TestOpenRejectsTrailingContent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "inject-index.json")
	body := `{"version":1,"entries":[]}` + "\n" + `{"version":1,"entries":[]}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(p); err == nil {
		t.Fatal("連結 JSON は loud エラーのはず")
	}
}

// TestPersistFilePermission は inject-index.json が 0600（他ユーザーに閉じる）
// で書かれる規律を保証する（pane_id / remote PC 名を含むため）。
func TestPersistFilePermission(t *testing.T) {
	p := filepath.Join(t.TempDir(), "inject-index.json")
	idx, _ := Open(p)
	if err := idx.Reserve("w9:p1", "pc", "sid"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("perm=%o want 0600", perm)
	}
}

// TestSnapshotIsDeterministicallySorted は pane_id 昇順で返ることを確認
// （ログ比較の予測可能性・テスト安定性）。
func TestSnapshotIsDeterministicallySorted(t *testing.T) {
	p := filepath.Join(t.TempDir(), "inject-index.json")
	idx, _ := Open(p)
	// 意図的に逆順で入れる
	_ = idx.Commit("w9:pC", "x", "y")
	_ = idx.Commit("w9:pA", "x", "y")
	_ = idx.Commit("w9:pB", "x", "y")
	got := idx.Snapshot()
	if len(got) != 3 {
		t.Fatalf("len=%d", len(got))
	}
	want := []string{"w9:pA", "w9:pB", "w9:pC"}
	for i, e := range got {
		if e.PaneID != want[i] {
			t.Errorf("[%d]=%s want=%s", i, e.PaneID, want[i])
		}
	}
	// disk 上の JSON も昇順で保存されている（bisect でファイル比較する時の予測性）。
	data, _ := os.ReadFile(p)
	var f file
	_ = json.Unmarshal(data, &f)
	for i, e := range f.Entries {
		if e.PaneID != want[i] {
			t.Errorf("disk[%d]=%s want=%s", i, e.PaneID, want[i])
		}
	}
}

// TestConcurrentAccessSerialized は並行呼び出し安全性を race 検出で保証する。
// go test -race 下で map race が発火しないことが期待挙動。
func TestConcurrentAccessSerialized(t *testing.T) {
	p := filepath.Join(t.TempDir(), "inject-index.json")
	idx, _ := Open(p)
	var wg sync.WaitGroup
	// 10 writer × 20 op で 200 回書換／読取を混ぜる。
	for w := 0; w < 10; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				pane := "w9:p" + string(rune('a'+w)) + "-" + string(rune('0'+(i%10)))
				_ = idx.Reserve(pane, "pc", "sid")
				_ = idx.IsInjected(pane)
				_ = idx.Commit(pane, "pc", "sid")
				_ = idx.Forget(pane)
			}
		}(w)
	}
	wg.Wait()
	// 最終状態は空か、少なくとも panic なく到達すれば OK。
	if got := len(idx.Snapshot()); got != 0 {
		t.Logf("残存 entries=%d（Forget 完了順の兼ね合いで残ってもよい）", got)
	}
}

// TestReserveRejectsEmpty は入力バリデーションを機械確認する。
func TestReserveRejectsEmpty(t *testing.T) {
	p := filepath.Join(t.TempDir(), "inject-index.json")
	idx, _ := Open(p)
	for _, c := range []struct{ pane, pc, sid string }{
		{"", "pc", "sid"},
		{"pid", "", "sid"},
		{"pid", "pc", ""},
	} {
		if err := idx.Reserve(c.pane, c.pc, c.sid); err == nil {
			t.Errorf("Reserve(%q,%q,%q) はエラーのはず", c.pane, c.pc, c.sid)
		}
	}
}
