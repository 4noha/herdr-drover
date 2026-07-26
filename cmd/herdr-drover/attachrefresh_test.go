package main

// OS 非依存の部分だけをここ（untagged）に置く。実 herdr を起こす検証は
// attachrefresh_unix_test.go（`//go:build unix`）＝実 herdr harness が /tmp 前提
// （macOS の sun_path 104B 制約）で Windows へ未移植のため。

import (
	"os"
	"path/filepath"
	"testing"
)

// TestNeedsAttachRefresh は作り直し判定の純関数テーブル。
//
// 肝は 2 つ:
//   - **prev 空（スタンプ未記録）は true**。この仕組みが入る前のバイナリが作った
//     注入 pane が残っている起動＝配信 1 回目にまさに直したいケース。
//   - **cur 空は false**。版数不明のビルドで毎起動作り直すと ↗窓 が起動のたび瞬断する。
func TestNeedsAttachRefresh(t *testing.T) {
	cases := []struct {
		name      string
		prev, cur string
		want      bool
	}{
		{"スタンプ未記録＝旧バイナリ製の pane が残る起動", "", "v0.5.29", true},
		{"版数が上がった", "v0.5.28", "v0.5.29", true},
		{"版数が下がった（ロールバック）も作り直す", "v0.5.29", "v0.5.28", true},
		{"同一版数＝通常の再起動では作り直さない", "v0.5.29", "v0.5.29", false},
		{"現在版が不明なら判定しない", "v0.5.28", "", false},
		{"両方不明も判定しない", "", "", false},
	}
	for _, c := range cases {
		if got := needsAttachRefresh(c.prev, c.cur); got != c.want {
			t.Errorf("%s: needsAttachRefresh(%q,%q)=%v want %v", c.name, c.prev, c.cur, got, c.want)
		}
	}
}

// TestAttachStampRoundTrip はスタンプの読み書き。不在・壊れは "" ＝「記録なし」扱いで、
// needsAttachRefresh 側が true に倒れる（作り直しが永久に止まるより 1 回余計に作り直す）。
func TestAttachStampRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "attach-version") // dir 不在からの作成も見る

	if got := readAttachStamp(path); got != "" {
		t.Fatalf("不在スタンプは空のはず: %q", got)
	}
	if err := writeAttachStamp(path, "v0.5.29"); err != nil {
		t.Fatalf("writeAttachStamp: %v", err)
	}
	if got := readAttachStamp(path); got != "v0.5.29" {
		t.Fatalf("読み戻し不一致: %q", got)
	}
	// 末尾改行を書いているので TrimSpace が効いていること（そのまま比較すると不一致になる）。
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(raw) == "v0.5.29" {
		t.Fatal("改行なしで書かれている（TrimSpace の検証にならない）")
	}
	// 上書きできること（版数が進むたびに更新される）。
	if err := writeAttachStamp(path, "v0.5.30"); err != nil {
		t.Fatalf("writeAttachStamp(上書き): %v", err)
	}
	if got := readAttachStamp(path); got != "v0.5.30" {
		t.Fatalf("上書き後の読み戻し不一致: %q", got)
	}
}

// TestAttachStampPathIsUnderDroverHome は置き場が ~/.herdr-drover（config.json /
// inject-index.json と同 dir）であること。⚠setTestHome で隔離する（実 HOME に
// 書かない・2026-07-25 の実害の教訓。Windows は %USERPROFILE% も要る）。
func TestAttachStampPathIsUnderDroverHome(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	got, err := attachStampPath()
	if err != nil {
		t.Fatalf("attachStampPath: %v", err)
	}
	if want := filepath.Join(home, ".herdr-drover", "attach-version"); got != want {
		t.Fatalf("attachStampPath()=%q want %q", got, want)
	}
}
