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
	"sync"
	"time"
)

// attachLogMaxBytes を超えたら 1 世代だけローテートする。11 viewer が 1 分に
// 数行なので 8MB は月単位で足りる（thrash 時でも .1 に退避されて追える）。
const attachLogMaxBytes = 8 << 20

// attachLogger は attach 1 プロセス分のログ出力。nil は no-op。
type attachLogger struct {
	mu     sync.Mutex
	f      *os.File
	prefix string
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

// Close はファイルを閉じる（nil は no-op）。
func (l *attachLogger) Close() {
	if l == nil || l.f == nil {
		return
	}
	l.mu.Lock()
	_ = l.f.Close()
	l.f = nil
	l.mu.Unlock()
}
