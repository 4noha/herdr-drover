package main

// logrotate — **O_APPEND で共有追記されているログ**の 1 世代ローテート。
//
// ## なぜ要るか（2026-09-18 実測）
//
// `agent.log` は launchd の `StandardOutPath` / `StandardErrorPath`（install.go の
// plist）＝**launchd がファイルを開いて daemon の fd 1/2 に渡す**もので、drover 側に
// ローテートが一切無かった。結果:
//
//   - mac-studio: **126,497,212 バイト**（2026/07/18 の初回起動から 2 か月ぶん全部）
//   - 共用 slave 2 台: 99.8MB / 88.5MB（webterm bridge の RESIZE thrash 期に
//     observe を 49 万回 spawn したぶんが全部残っていた）
//
// 巨大化したログは「読めない」＝障害調査の一次情報が実質失われる（`tail` はできても
// 期間比較ができない）。加えて `attach.log` 側にはローテートが在るのに **open 時しか
// 判定していなかった**ため、長寿命の viewer プロセスでは無限に伸び、退避された
// `attach.log.1` が上限 8MiB の 4 倍（31MB）になっていた（同じ日の実測）。
// ＝**周期的に size を見る**のがこのファイルの役目。
//
// ## rename ではなく copy＋truncate なのはなぜか（両方とも fd を止められない）
//
//   - `agent.log`: fd を開いているのは **launchd**。rename すると daemon の以後の
//     出力はすべて `agent.log.1`（＝rename 後の inode）へ行き、`agent.log` は
//     daemon 再起動まで空のまま＝「今のログ」が読めなくなる。
//   - `attach.log`: **全 viewer プロセスが 1 本を共有**する設計（attachlog.go）。
//     rename すると退避を実行しなかった他プロセスは古い inode に取り残され、
//     その行は次の退避で `.1` ごと消える。
//
// どちらも **inode を保つ**必要がある＝現物を `.1` へコピーしてから本体を 0 へ
// 切り詰める。これが安全なのは fd が **O_APPEND** だから（write ごとに末尾へ
// seek する＝切り詰め後は offset 0 から書く。offset を保つ fd なら穴あき
// ファイルを再生産してしまう）。実測で確認済み:
//
//	$ lsof +fg -p <daemon pid>
//	herdr-dro 32859 ... 1u REG R,W,AP        ... /Users/…/.herdr-drover/agent.log
//	herdr-dro 32859 ... 2u REG R,W,AP,0x10000 ... /Users/…/.herdr-drover/agent.log
//	                            ^^ AP = O_APPEND
//
// `attach.log` 側は drover 自身が `os.O_APPEND` で開いている（attachlog.go）。
// それでも万一 append でない fd で走った場合に穴あきファイルを毎周期作り続けない
// よう、ローテート直後に**効果を検証して止める**（runAgentLogRotator 参照）。

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"
)

const (
	// agentLogMaxBytes は agent.log の上限（超過で 1 世代退避＝最大でも約 2 倍）。
	//
	// ⚠**上限を上げることは対処ではない**（attach.log で学習済み: 8MiB を上げても
	// 1 日で押し流されるだけで、効いたのは量を減らすこと）。ここも同じ順序で、
	// まず量の元凶（bridge の RESIZE respawn thrash）を潰し、その上で上限を置く。
	// 16MiB は「日常運用の数か月ぶん」と「1 台の全期間を 1 ファイルで読める」の
	// 折り合い（thrash が無い平常時の実測は概ね数 MB/月）。
	agentLogMaxBytes = 16 << 20

	// agentLogCheckEvery は size を stat する周期（stat 1 回だけなので安い）。
	// 周期判定にしたのは attach.log の「open 時しか見ない」失敗の裏返し
	// ＝daemon は数か月落ちないので、開いた瞬間の判定は無意味に等しい。
	agentLogCheckEvery = time.Minute

	// rotateClaimStale は退避マーカー（`<log>.rotating`）を「退避中に落ちた残骸」
	// とみなす年齢。退避自体は数百 ms で終わる（16MiB のコピー）ので 2 分は十分に
	// 保守的。
	rotateClaimStale = 2 * time.Minute
)

// agentLogPath は launchd が daemon の stdout/stderr に割り当てるログのパス。
// **install が plist に焼く値と同じ関数から採る**（別々に組み立てると片方を
// 直したときに黙ってずれる）。
func agentLogPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return resolveInstallPaths(home).logPath, nil
}

// rotateAppendLog は path が maxBytes を超えていたら `path.1` へ 1 世代退避し、
// **inode を保ったまま**本体を 0 バイトへ切り詰める（理由はファイル冒頭）。
//
// 戻り値の note は「何をしたか」の 1 行（空＝何もしていない）。呼び手は非空なら
// **必ずログへ出す**（鉄則⑤: silent な破棄・skip の禁止）。切り詰め直後に書けば
// 新しいログの 1 行目がローテート通知になる＝attach.log と同じ流儀。
func rotateAppendLog(path string, maxBytes int64) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil // まだ誰も書いていない＝何もしない
		}
		return "", err
	}
	if fi.Size() <= maxBytes {
		return "", nil
	}
	n := fi.Size()

	// 退避の排他（`attach.log` は最大十数プロセスが共有する＝同時に退避すると
	// 後者が「切り詰め済みの空ファイル」を `.1` に上書きして**前者が保存した
	// 世代を丸ごと失う**）。O_EXCL の作成成否だけを使う＝flock と違って
	// Windows でも同じコードで動く。
	claim := path + ".rotating"
	cf, cerr := os.OpenFile(claim, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if cerr != nil {
		if st, serr := os.Stat(claim); serr == nil && time.Since(st.ModTime()) >= rotateClaimStale {
			// 退避中に死んだ残骸。奪うのではなく除去して次周に回す（この周期で
			// 続行すると「本当に生きている退避」と競合しうる）。
			if rerr := os.Remove(claim); rerr != nil {
				return "", fmt.Errorf("退避マーカー %s の残骸を除去できない: %w", claim, rerr)
			}
			return fmt.Sprintf("退避マーカー %s が %s 以上古い＝退避中に落ちた残骸として除去した（次の周期で退避する）",
				filepath.Base(claim), rotateClaimStale), nil
		}
		return fmt.Sprintf("退避は他プロセスが実行中（%s が在る）＝今回は見送る", filepath.Base(claim)), nil
	}
	_ = cf.Close()
	defer os.Remove(claim)

	gen := path + ".1"
	src, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer src.Close()
	dst, err := os.OpenFile(gen, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	// **stat した n バイトだけ**コピーする（コピー中に来た追記まで追いかけると
	// 終わらない）。
	copied, cpErr := io.CopyN(dst, src, n)
	if closeErr := dst.Close(); cpErr == nil {
		cpErr = closeErr
	}
	if cpErr != nil {
		return "", fmt.Errorf("%s へ %d/%d バイト退避して失敗（本体は切り詰めない）: %w", gen, copied, n, cpErr)
	}

	// コピー〜切り詰めの窓に来た追記は切り詰めで消える（共有 O_APPEND fd は
	// 止められない）＝**バイト数を申告する**。0 でない値が日常的に出るなら
	// 書き込み量そのものを疑う手がかりになる。
	lost := int64(0)
	if cur, serr := os.Stat(path); serr == nil && cur.Size() > n {
		lost = cur.Size() - n
	}
	if err := os.Truncate(path, 0); err != nil {
		return "", fmt.Errorf("%s を切り詰められない（退避 %s は残す）: %w", path, gen, err)
	}

	note := fmt.Sprintf("ローテート: %s が %d バイト（上限 %d）を超えたので %s へ %d バイト退避し、本体を 0 に切り詰めた",
		filepath.Base(path), n, maxBytes, filepath.Base(gen), copied)
	if lost > 0 {
		note += fmt.Sprintf("。⚠退避〜切り詰めの窓で最低 %d バイトの追記を取りこぼした", lost)
	}
	return note, nil
}

// writerIsSameFile は「ログ出力先が path そのものか」を exact に判定する
// （os.SameFile＝dev+inode。パス文字列比較では symlink や隔離 HOME を取り違える）。
//
// これを噛ませる理由: 開発中に手で `herdr-drover agent` を起動したときの stderr は
// 端末で、agent.log は**誰も書いていない過去の記録**になる。そこで切り詰めると
// launchd 稼働期の履歴を黙って消す＝害しかない。
func writerIsSameFile(w io.Writer, path string) (bool, string) {
	f, ok := w.(*os.File)
	if !ok {
		return false, fmt.Sprintf("ログ出力先が実ファイルでない（%T）", w)
	}
	wfi, err := f.Stat()
	if err != nil {
		return false, fmt.Sprintf("出力先 %s を stat できない: %v", f.Name(), err)
	}
	pfi, err := os.Stat(path)
	if err != nil {
		return false, fmt.Sprintf("%s を stat できない: %v", path, err)
	}
	if !os.SameFile(wfi, pfi) {
		return false, fmt.Sprintf("出力先（%s）が %s と別ファイル", f.Name(), path)
	}
	return true, ""
}

// runAgentLogRotator は daemon 常駐中に agent.log を周期監視して 1 世代退避する。
// ctx が切れるまで走る（呼び手は go で起動する）。
//
// 出力先が agent.log 自身でなければ**何もせず理由を 1 行残して終わる**
// （writerIsSameFile の doc 参照）。
func runAgentLogRotator(ctx context.Context, w io.Writer, lg *log.Logger, path string, maxBytes int64, every time.Duration) {
	if ok, why := writerIsSameFile(w, path); !ok {
		lg.Printf("agent.log ローテート無効: %s＝このプロセスは %s を書いていないので触らない", why, path)
		return
	}
	lg.Printf("agent.log ローテート有効: 上限 %d バイト・%s ごとに確認・1 世代（%s.1）", maxBytes, every, filepath.Base(path))
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			note, err := rotateAppendLog(path, maxBytes)
			if err != nil {
				lg.Printf("agent.log ローテート失敗（次周で再試行）: %v", err)
				continue
			}
			if note == "" {
				continue
			}
			lg.Print(note) // 切り詰め直後＝新しいログの 1 行目に残る

			// **効果の検証**: 切り詰め直後の size が上限の半分を超えているなら、
			// この fd は offset を保つ（O_APPEND でない）＝穴あきファイルを毎周期
			// 作り直す thrash になる。1 回で気づいて止める（退避済みの `.1` は
			// 残っているので記録は失わない）。
			if fi, serr := os.Stat(path); serr == nil && fi.Size() > maxBytes/2 {
				lg.Printf("agent.log ローテート停止: 切り詰め直後に %d バイト＝追記モードでない fd（offset が保たれる環境）。"+
					"以後の退避は行わない（%s.1 は保存済み・plist の StandardOutPath を見直すこと）", fi.Size(), filepath.Base(path))
				return
			}
		}
	}
}
