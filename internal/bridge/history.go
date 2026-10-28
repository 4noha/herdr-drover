// history.go はワイヤ v2（Web viewer 向け）の producer→viewer 制御列と
// 履歴スナップショットの組み立て・キャッシュ。
//
// 背景（2026-10-29 スパイクで実測確定・SPEC §5.1/§5.2 参照）:
//   - observe のフレームは `ESC[2J`＋絶対座標 CUP だけで一切スクロールしない
//     （3J/alt-screen/SU/SD/IL/DL/DECSTBM/LF を含まない）＝viewer の端末に
//     **先に履歴を書いておいても壊れない**。履歴は scrollback、ライブ画面は
//     フレーム、と層が分かれる。
//   - 履歴は `pane.read`(text) で取れる（ansi は可視画面のみ）。ただし本番の
//     長寿命 pane では **pane.read が 4〜16 秒かかる**（返る行数と無関係＝herdr
//     側の処理時間。隔離サーバでは再現しない）。初回描画をそれで待たせない
//     ため、読取には予算（DefaultHistoryBudget）を付け、間に合わなければ履歴
//     なしで先に描き、読取はバックグラウンドで完了させてキャッシュへ入れ、
//     次の接続（quiescence 再接続・リロード）で送る。
//
// 制御列は OSC 7707 を使う: `ESC ] 7707 ; k=v;k=v ESC \`。relay は byte
// 透過、xterm.js も実端末も未知 OSC は無視、2026 同期シム（sync.js）とも
// 干渉しない。viewer（term.js の meta filter）は xterm へ流す前に剥がす。
package bridge

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/4noha/herdr-drover/internal/herdrapi"
)

const (
	metaOSC = "\x1b]7707;"
	metaST  = "\x1b\\"

	// HistoryLines は履歴取得の行数上限。herdr 0.9.5 の pane.read は
	// これ以上指定しても 1000 行で truncated になる（実測）。
	HistoryLines = 1000

	// DefaultHistoryBudget は履歴読取を初回描画の前に待つ上限（冒頭コメント）。
	DefaultHistoryBudget = 1500 * time.Millisecond

	// HistoryTTL はキャッシュの鮮度。これより古い履歴は読み直す（その間は
	// 古い履歴も使わない＝末尾 rows 行の除外が現在の画面とズレるため）。
	HistoryTTL = 60 * time.Second
)

// MetaBegin はストリーム先頭の宣言。viewer はこれを見て v2 モードへ切り替える
// （端末を cols×rows に合わせ scrollback を有効化）。
//   - trunc: 履歴が herdr 側の上限で切れているか（UI の「これ以上遡れない」用）
//   - hist: この接続で履歴本文が続くか（0 なら hist_end まで空＝viewer は
//     「履歴受領済み」と見なさず、次の接続の履歴を受け入れる）
//   - scrolled: 元 pane のローカル表示が最下部から何行遡っているか
//     （>0 なら viewer は［最新へ］の案内を出す。observe はこの表示位置を描く）
func MetaBegin(cols, rows int, truncated, hist bool, scrolled int) []byte {
	b2i := func(v bool) int {
		if v {
			return 1
		}
		return 0
	}
	return []byte(fmt.Sprintf("%sv=2;cols=%d;rows=%d;trunc=%d;hist=%d;scrolled=%d%s",
		metaOSC, cols, rows, b2i(truncated), b2i(hist), scrolled, metaST))
}

// MetaHistEnd は履歴区間の終端。viewer は履歴を受け取り済みなら、以降の接続では
// この直前までを捨てる（履歴はページにつき 1 回だけ scrollback に入れる）。
func MetaHistEnd() []byte {
	return []byte(metaOSC + "hist_end" + metaST)
}

// HistoryPayload は pane.read(text, source=recent) の本文を viewer の端末へ
// 書く形にする:
//   - 末尾 rows 行（＝可視画面。直後の full frame が描き直す）を落とす
//   - LF → CRLF（pty を通らず xterm.js に直接書くため onlcr 変換が無い）
//   - 非空なら rows 行ぶん CRLF を足す。full frame の `ESC[2J` は画面上の行を
//     **消す**のであって押し上げないので、先に全行を scrollback へ押し込む
//     （スパイクで実測: これを欠くと最後の 1 画面分の履歴が黙って消える）
//
// 履歴が可視画面ぶん以下しか無ければ nil（何も書かない）。
func HistoryPayload(text string, rows int) []byte {
	if rows <= 0 || text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1] // 末尾改行ぶんの空要素
	}
	if len(lines) <= rows {
		return nil
	}
	lines = lines[:len(lines)-rows]
	var sb strings.Builder
	for _, l := range lines {
		sb.WriteString(strings.TrimRight(l, " \r"))
		sb.WriteString("\r\n")
	}
	for i := 0; i < rows; i++ {
		sb.WriteString("\r\n")
	}
	return []byte(sb.String())
}

// HistoryCache は pane ごとの履歴スナップショット（daemon で 1 つを全 bridge が
// 共有）。読取は pane ごとに同時 1 本（inflight）で、予算内に終わらなくても
// 継続して完了時にキャッシュへ入る。
type HistoryCache struct {
	mu       sync.Mutex
	m        map[string]histEntry
	inflight map[string]chan struct{}
}

type histEntry struct {
	text      string
	truncated bool
	at        time.Time
}

func NewHistoryCache() *HistoryCache {
	return &HistoryCache{m: map[string]histEntry{}, inflight: map[string]chan struct{}{}}
}

// fetch は sid の履歴を budget 内で返す。新鮮なキャッシュがあればそれ、無ければ
// read を（未進行なら）起動して budget まで待つ。間に合わなければ ok=false で
// 戻る（read は継続し、完了時にキャッシュへ入る）。
func (c *HistoryCache) fetch(sid string, budget time.Duration,
	read func() (*herdrapi.PaneReadInfo, error), logf func(string, ...any)) (histEntry, bool) {
	c.mu.Lock()
	if e, ok := c.m[sid]; ok && time.Since(e.at) < HistoryTTL {
		c.mu.Unlock()
		return e, true
	}
	done, running := c.inflight[sid]
	if !running {
		done = make(chan struct{})
		c.inflight[sid] = done
		go func() {
			t0 := time.Now()
			rd, err := read()
			c.mu.Lock()
			if err == nil {
				c.m[sid] = histEntry{text: rd.Text, truncated: rd.Truncated, at: time.Now()}
			}
			delete(c.inflight, sid)
			c.mu.Unlock()
			if err != nil {
				logf("v2: 履歴読取失敗（%.1fs）: %v", time.Since(t0).Seconds(), err)
			} else {
				logf("v2: 履歴読取 %.1fs（pane.read %d 行・truncated=%v）→ キャッシュ",
					time.Since(t0).Seconds(), strings.Count(rd.Text, "\n"), rd.Truncated)
			}
			close(done)
		}()
	}
	c.mu.Unlock()
	select {
	case <-done:
	case <-time.After(budget):
		return histEntry{}, false
	}
	c.mu.Lock()
	e, ok := c.m[sid]
	c.mu.Unlock()
	return e, ok
}
