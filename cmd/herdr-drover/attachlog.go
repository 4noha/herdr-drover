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
//     先頭に必ず書く（silent な破棄をしない）。判定は open 時だけでなく
//     **書いたバイト数を数えて周期的に**行う（2026-09-18 実測: open 時のみだと
//     長寿命の viewer プロセスでは永久に判定されず、退避された `attach.log.1` が
//     上限 8MiB の 4 倍＝31MB まで伸びていた）。退避は rename ではなく
//     copy＋truncate（inode を保つ＝共有している他プロセスを古い inode に
//     取り残さない。logrotate.go の rotateAppendLog）。
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

// attachLogCheckEvery は「自分が何バイト書いたら size を stat し直すか」。
// stat 1 回は安いが毎行やるのは無駄なので、上限の 1/8 相当を目安に間引く。
const attachLogCheckEvery = 1 << 20

// attachLogger は attach 1 プロセス分のログ出力。nil は no-op。
type attachLogger struct {
	mu     sync.Mutex
	f      *os.File
	prefix string
	path   string
	// ローテート判定（フィールドに持つのはテストから上限を下げるため＝
	// 8MiB を書かせるテストは遅すぎて実質書けない）。
	maxBytes   int64
	checkEvery int64
	written    int64 // 前回の判定からこのプロセスが書いたバイト数
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
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	l := &attachLogger{
		f:          f,
		prefix:     fmt.Sprintf("%s/%s[%d]", remotePC, sid, os.Getpid()),
		path:       path,
		maxBytes:   attachLogMaxBytes,
		checkEvery: attachLogCheckEvery,
	}
	// open 時に 1 回（前プロセスが伸ばしたぶん）。以後は Printf が数える。
	l.rotateCheck()
	return l, nil
}

// rotateCheck は上限超過なら 1 世代退避し、**何をしたかを必ず 1 行書く**
// （切り詰め直後に書くので新しいログの 1 行目に立つ）。
//
// 退避の実体は logrotate.go の rotateAppendLog（inode を保つ copy＋truncate＝
// **このファイルを共有している他の viewer プロセス**を古い inode に取り残さない。
// 排他マーカーで同時退避も潰す＝先に退避した世代を上書きしない）。
func (l *attachLogger) rotateCheck() {
	if l == nil || l.f == nil || l.path == "" || l.maxBytes <= 0 {
		return
	}
	note, err := rotateAppendLog(l.path, l.maxBytes)
	if err != nil {
		l.Printf("ローテート失敗（次の判定で再試行）: %v", err)
		return
	}
	if note != "" {
		l.Printf("%s", note)
	}
}

// Printf は 1 行書く（末尾改行は自動）。nil レシーバは no-op。
func (l *attachLogger) Printf(format string, args ...any) {
	if l == nil || l.f == nil {
		return
	}
	line := fmt.Sprintf("%s %s %s\n",
		time.Now().Format("2006-01-02 15:04:05.000"), l.prefix, fmt.Sprintf(format, args...))
	l.mu.Lock()
	n, _ := l.f.WriteString(line)
	// 自分が書いた量で判定を間引く（ローテート判定は size の stat だけ＝
	// **open 時に 1 回**では長寿命プロセスで永久に走らない。31MB の `.1` を
	// 実際に作った失敗の対処）。
	l.written += int64(n)
	due := l.checkEvery > 0 && l.written >= l.checkEvery
	if due {
		l.written = 0
	}
	l.mu.Unlock()
	if due {
		l.rotateCheck() // ⚠ mu の外（rotateCheck は Printf で書く）
	}
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
