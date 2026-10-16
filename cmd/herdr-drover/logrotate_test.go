package main

// logrotate のテスト。**実ファイル＋実 fd**で書く（O_APPEND の fd を開いたまま
// 退避する、というのがこの機構の本体なので、合成では何も担保できない）。
//
// 回帰対象（2026-09-18 実測）:
//   - agent.log にローテートが無く 126MB（mac-studio・2 か月）/ 99.8MB（共用
//     slave・11 日）まで伸びていた＝調査の一次情報が読めない
//   - attach.log は open 時しか判定せず `.1` が上限の 4 倍（31MB）になっていた

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// openAppendLog は「launchd が開いて daemon に渡す fd」と同じ形（O_APPEND）で
// ログを作り、size バイトぶん書いて fd を返す。
func openAppendLog(t *testing.T, path string, lines int) *os.File {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(func() { _ = f.Close() })
	for i := 0; i < lines; i++ {
		if _, err := fmt.Fprintf(f, "line %04d %s\n", i, strings.Repeat("x", 64)); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	return f
}

func mustStat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi
}

// TestRotateAppendLogPreservesInodeAndFollowsAppendFd は本体の担保:
// 退避しても **inode が変わらない**（＝ログを開いたままの launchd / 他 viewer が
// 取り残されない）こと、切り詰め後の追記が **先頭から**続くこと（O_APPEND の fd
// が穴あきファイルを作らない）こと。
//
// rename 実装ではここが落ちる（inode が変わり、開いたままの fd の書き込みは
// `.1` 側へ行くので新しい agent.log は空のまま）。
func TestRotateAppendLogPreservesInodeAndFollowsAppendFd(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.log")
	f := openAppendLog(t, path, 200) // 約 15KB

	before := mustStat(t, path)
	orig, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	note, err := rotateAppendLog(path, 4096)
	if err != nil {
		t.Fatalf("rotateAppendLog: %v", err)
	}
	if !strings.Contains(note, "ローテート") || !strings.Contains(note, "agent.log.1") {
		t.Fatalf("退避したのに何をしたか報告していない（silent な破棄の禁止）: note=%q", note)
	}

	// 1 世代目に現物がそのまま入っている。
	gen, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("ReadFile .1: %v", err)
	}
	if !bytes.Equal(gen, orig) {
		t.Fatalf("退避内容が現物と違う: %d バイト（want %d）", len(gen), len(orig))
	}

	// inode が保たれている＝開いたままの fd が取り残されない。
	after := mustStat(t, path)
	if !os.SameFile(before, after) {
		t.Fatalf("inode が変わった（rename 実装）＝launchd/他 viewer の fd が %s.1 側に取り残される", path)
	}

	// **切り詰め済みの本体へ、退避前から開いている fd で追記できる**。
	if _, err := f.WriteString("after rotate\n"); err != nil {
		t.Fatalf("退避後の追記: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile after: %v", err)
	}
	if string(got) != "after rotate\n" {
		t.Fatalf("切り詰め後の追記が先頭から続いていない（穴あき/offset 保持）: %d バイト %q", len(got), truncStr(string(got), 80))
	}
}

func truncStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// TestRotateAppendLogSkipsUnderCap は過剰発火しないこと（上限以下では触らない）。
func TestRotateAppendLogSkipsUnderCap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.log")
	openAppendLog(t, path, 10)

	note, err := rotateAppendLog(path, 1<<20)
	if err != nil || note != "" {
		t.Fatalf("上限以下で退避した: note=%q err=%v", note, err)
	}
	if _, serr := os.Stat(path + ".1"); serr == nil {
		t.Fatalf("上限以下なのに %s.1 を作った", path)
	}

	// 不在のログはエラーにしない（まだ誰も書いていない＝正常）。
	if note, err := rotateAppendLog(filepath.Join(dir, "none.log"), 10); err != nil || note != "" {
		t.Fatalf("不在ログでエラー/退避した: note=%q err=%v", note, err)
	}
}

// TestRotateAppendLogRespectsClaim は **同時退避で先の世代を潰さない**こと。
// attach.log は最大十数プロセスが共有する＝全員が同時に上限超過を見る。
func TestRotateAppendLogRespectsClaim(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "attach.log")
	openAppendLog(t, path, 200)

	// 先に「退避済みの世代」を置き、他プロセスが退避中であるマーカーを立てる。
	if err := os.WriteFile(path+".1", []byte("前の世代\n"), 0o600); err != nil {
		t.Fatalf("write .1: %v", err)
	}
	claim, err := os.OpenFile(path+".rotating", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	_ = claim.Close()

	note, err := rotateAppendLog(path, 4096)
	if err != nil {
		t.Fatalf("rotateAppendLog: %v", err)
	}
	if !strings.Contains(note, "他プロセスが実行中") {
		t.Fatalf("退避中の競合を報告していない: note=%q", note)
	}
	if b, rerr := os.ReadFile(path + ".1"); rerr != nil || string(b) != "前の世代\n" {
		t.Fatalf("他プロセスの退避中に %s.1 を上書きした: %q err=%v", path, truncStr(string(b), 40), rerr)
	}
	if fi := mustStat(t, path); fi.Size() == 0 {
		t.Fatalf("退避せずに本体を切り詰めた（記録の消失）")
	}

	// 残骸（古いマーカー）は除去して次周に回す。
	old := time.Now().Add(-2 * rotateClaimStale)
	if err := os.Chtimes(path+".rotating", old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	note, err = rotateAppendLog(path, 4096)
	if err != nil {
		t.Fatalf("rotateAppendLog(stale): %v", err)
	}
	if !strings.Contains(note, "残骸") {
		t.Fatalf("古いマーカーを残骸として報告していない: note=%q", note)
	}
	if _, serr := os.Stat(path + ".rotating"); serr == nil {
		t.Fatalf("古いマーカーを除去していない")
	}
	// 次の呼びで実際に退避される。
	if note, err = rotateAppendLog(path, 4096); err != nil || !strings.Contains(note, "ローテート") {
		t.Fatalf("残骸除去後に退避しない: note=%q err=%v", note, err)
	}
}

// TestAgentLogRotatorRotatesLiveLog は daemon 常駐中の周期判定（＝ローテートが
// 実際に走る経路）を担保する。**この goroutine が無い／周期でなく open 時だけの
// 実装では 126MB まで伸びる**のが実障害だった。
func TestAgentLogRotatorRotatesLiveLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.log")
	f := openAppendLog(t, path, 300) // 約 23KB（上限 4KB を超える）
	lg := log.New(f, "", log.LstdFlags)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		runAgentLogRotator(ctx, f, lg, path, 4096, 20*time.Millisecond)
		close(done)
	}()

	deadline := time.Now().Add(10 * time.Second)
	for {
		if fi, err := os.Stat(path + ".1"); err == nil && fi.Size() > 4096 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("周期判定で退避されない（%s.1 が出来ない）＝無限に伸びる", path)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// 切り詰め後の本体は「有効化」と「ローテート」の通知だけ（daemon の以後の
	// 出力はここから続く）。
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(body), "ローテート:") {
		t.Fatalf("切り詰め後のログにローテート通知が無い（silent な破棄）: %q", truncStr(string(body), 200))
	}
	if int64(len(body)) > 4096/2 {
		t.Fatalf("切り詰めが効いていない（%d バイト）", len(body))
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("ctx cancel で rotator が終わらない")
	}
}

// TestAgentLogRotatorSkipsForeignLog は **自分が書いていないログを切り詰めない**
// ことを担保する（開発中に手で agent を起動したとき、launchd 稼働期の記録を
// 黙って消すのは害しかない）。
func TestAgentLogRotatorSkipsForeignLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.log")
	openAppendLog(t, path, 300)
	before := mustStat(t, path).Size()

	var buf bytes.Buffer
	lg := log.New(&buf, "", 0)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runAgentLogRotator(ctx, &buf, lg, path, 4096, 10*time.Millisecond) // 即 return する筈

	if got := mustStat(t, path).Size(); got != before {
		t.Fatalf("出力先が別なのに agent.log を触った: %d → %d バイト", before, got)
	}
	if _, err := os.Stat(path + ".1"); err == nil {
		t.Fatalf("出力先が別なのに退避した")
	}
	if !strings.Contains(buf.String(), "ローテート無効") {
		t.Fatalf("無効化した理由を報告していない: %q", buf.String())
	}

	// 別ファイルへ書いている場合も同じ（パス文字列でなく inode で判定する）。
	other, err := os.OpenFile(filepath.Join(dir, "other.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open other: %v", err)
	}
	defer other.Close()
	if ok, why := writerIsSameFile(other, path); ok || why == "" {
		t.Fatalf("別ファイルを同一と判定した: ok=%v why=%q", ok, why)
	}
}
