//go:build unix

package main

// attachlog — ↗窓 viewer（cmdAttach）の接続ライフサイクルを永続ログに残す。
//
// ## なぜ要るか（2026-07-26）
//
// 「viewer が再接続に失敗したまま張り付き、注入をやり直すまで復帰しない」という
// 障害が長く続いていたが、**事後診断が構造的に不可能**だった: attach の診断出力は
// すべて pane 画面向けで、しかも各エラーは `\x1b[2J`（画面クリア）してから書くため
// **次のフレームが 1 枚来た瞬間に消える**。再注入すれば直るので、原因を追う手がかりが
// 毎回失われていた。
//
// ## 設計
//
//   - **全 viewer が 1 本のファイルを共有する**（`~/.herdr-drover/attach.log`）。
//     この障害で最初に知りたいのは「全 viewer が同時に落ちたのか（＝ネットワーク/
//     relay 側）／個別に落ちたのか（＝プロセス個別の詰まり）」で、別ファイルに分けると
//     その突合が手作業になる。O_APPEND の短い行は атомарに追記されるので混線しない。
//   - **1 サイクル粒度**で書く（フレームごとには書かない）。BUG-3 の thrash で
//     agent.log が 16.8MB に膨れた前例があるため、粒度は粗く保つ。
//   - **サイズ上限で 1 世代だけローテート**する。⚠ローテートしたことは新ファイルの
//     先頭に必ず書く（silent な破棄をしない）。
//   - ログを開けなくても attach は動き続ける（nil レシーバは no-op）。↗窓 の表示と
//     入力がログ都合で壊れる方が害が大きい。

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// attachLogMaxBytes を超えたら 1 世代だけローテートする。
//
// ⚠**当初「11 viewer が 1 分に数行なので 8MB は月単位で足りる」と見積もったが
// 完全に外した**（実測 2026-08-01）: 1 サイクルで 3 行（dial / pump / cycle）× 17
// viewer × 30〜60 秒周期 ＝ **1 日 10MB**。`.1` ごと 1 日で押し流され、**肝心の
// 「機ごとの比較」ができなくなっていた**（ログに 1 台ぶんしか残っていなかった）。
//
// 対処はサイズを増やすことではなく**量を減らすこと**にした（`Cycle` の集約）。
// 正常系は「同じ結果が N 回続いた」の 1 行に畳むので、上限はこのままで足りる。
const attachLogMaxBytes = 8 << 20

// attachLogSummaryEvery は「同じ結果が続いている」ときに、それでも生存確認として
// 1 行出す間隔。これが無いと数時間まったく書かれず「動いているのか止まったのか」が
// ログから読めなくなる。
const attachLogSummaryEvery = 15 * time.Minute

// attachLogger は attach 1 プロセス分のログ出力。nil は no-op。
type attachLogger struct {
	mu     sync.Mutex
	f      *os.File
	prefix string
	// Cycle の集約状態（同じ kind が続く間は畳む）。
	lastKind string
	lastLine string
	repeat   int
	runSince time.Time
}

// attachLogPath は共有ログのパス（config.json / inject-index.json と同 dir）。
func attachLogPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".herdr-drover", "attach.log"), nil
}

// newAttachLogger は共有ログを追記モードで開く。開けなければ (nil, err) を返し、
// 呼び手は**画面に 1 行だけ出して続行する**（ログが無いだけで attach は止めない）。
func newAttachLogger(remotePC, sid string) (*attachLogger, error) {
	path, err := attachLogPath()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	rotated := ""
	if fi, serr := os.Stat(path); serr == nil && fi.Size() > attachLogMaxBytes {
		// ⚠ 破棄ではなく 1 世代退避。新ファイルの先頭でその旨を必ず書く。
		if rerr := os.Rename(path, path+".1"); rerr == nil {
			rotated = fmt.Sprintf("（%s が %d バイトを超えたので %s.1 へ退避した）",
				filepath.Base(path), attachLogMaxBytes, filepath.Base(path))
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	l := &attachLogger{f: f, prefix: fmt.Sprintf("%s/%s[%d]", remotePC, sid, os.Getpid())}
	if rotated != "" {
		l.Printf("ローテート %s", rotated)
	}
	return l, nil
}

// Printf は 1 行書く（末尾改行は自動）。nil レシーバは no-op。
func (l *attachLogger) Printf(format string, args ...any) {
	if l == nil || l.f == nil {
		return
	}
	line := fmt.Sprintf("%s %s %s\n",
		time.Now().Format("2006-01-02 15:04:05.000"), l.prefix, fmt.Sprintf(format, args...))
	l.mu.Lock()
	_, _ = l.f.WriteString(line)
	l.mu.Unlock()
}

// Cycle は 1 サイクルの結果を書く。**同じ種類の結果が続く間は 1 行に畳む**。
//
// ⚠これが本文の量を決める。↗窓 は無操作でも 30〜60 秒ごとに 1 サイクル回る
// （near-$0 の quiescence 設計上そうなる）ので、素直に毎回書くと 1 日 10MB に
// 達してローテートで**比較材料ごと消える**（実測でそうなった）。正常系は
// 「同じ結果 ×N 回」に畳み、**種類が変わった瞬間だけ**素の行を出す＝異常は
// 必ず 1 行目に立つ。
//
// kind は集約キー（"quiescence" / "peer-eof" / "frame-error" / "dial-fail" 等）。
// 同じ kind が続く限り畳むが、attachLogSummaryEvery ごとには必ず 1 行出す
// （生存確認。無いと数時間沈黙して「止まったのか」が分からない）。
func (l *attachLogger) Cycle(kind, line string) {
	if l == nil || l.f == nil {
		return
	}
	l.mu.Lock()
	same := kind == l.lastKind
	stale := time.Since(l.runSince) >= attachLogSummaryEvery
	if same && !stale {
		l.repeat++
		l.lastLine = line
		l.mu.Unlock()
		return
	}
	pendLine, n, since := l.lastLine, l.repeat, l.runSince
	l.lastKind, l.lastLine, l.repeat, l.runSince = kind, line, 0, time.Now()
	l.mu.Unlock()

	if n > 0 {
		l.Printf("（同じ結果が続いた ×%d 回 / %s〜 直近: %s）", n,
			since.Format("15:04:05"), pendLine)
	}
	l.Printf("%s", line)
}

// flushCycle は畳んでいる分を吐き出す（Close 前・異常時に呼ぶ）。
func (l *attachLogger) flushCycle() {
	if l == nil || l.f == nil {
		return
	}
	l.mu.Lock()
	n, line, since := l.repeat, l.lastLine, l.runSince
	l.repeat = 0
	l.mu.Unlock()
	if n > 0 {
		l.Printf("（同じ結果が続いた ×%d 回 / %s〜 直近: %s）", n, since.Format("15:04:05"), line)
	}
}

// Close はファイルを閉じる（nil は no-op）。畳んでいる分は必ず吐く
// （黙って捨てると「最後に何が起きていたか」が消える）。
func (l *attachLogger) Close() {
	if l == nil || l.f == nil {
		return
	}
	l.flushCycle()
	l.mu.Lock()
	_ = l.f.Close()
	l.f = nil
	l.mu.Unlock()
}

// cycleKind は pumpFrames の終了理由をログ集約キーへ畳む。**エラー文言そのものを
// キーにしない**（可変部分＝バイト数や時刻が混ざると畳めず量が減らない）。
func cycleKind(idleClosed bool, why string) string {
	switch {
	case idleClosed:
		return "quiescence" // 自分の read deadline＝near-$0 設計どおりの正常
	case strings.Contains(why, "pane への書込"):
		return "pane-write-fail"
	case strings.Contains(why, "EOF"):
		return "peer-eof" // 相手が正常手順で閉じた
	case strings.Contains(why, "use of closed"):
		return "self-close" // forceClose 等
	case strings.Contains(why, "failed to read frame"):
		return "frame-error" // 異常切断
	}
	return "other"
}
