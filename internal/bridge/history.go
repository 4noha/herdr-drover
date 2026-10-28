// history.go はワイヤ v2（Web viewer 向け）の producer→viewer 制御列と
// 履歴スナップショットの組み立て。
//
// 背景（2026-10-29 スパイクで実測確定・BUGS/TODO 参照）:
//   - observe のフレームは `ESC[2J`＋絶対座標 CUP だけで一切スクロールしない
//     （3J/alt-screen/SU/SD/IL/DL/DECSTBM/LF を含まない）＝viewer の端末に
//     **先に履歴を書いておいても壊れない**。履歴は scrollback、ライブ画面は
//     フレーム、と層が分かれる。
//   - 履歴は `pane.read`(text) で取れる（ansi は可視画面のみ）。
//
// 制御列は OSC 7707 を使う: `ESC ] 7707 ; k=v;k=v ESC \`。relay は byte
// 透過、xterm.js も実端末も未知 OSC は無視、2026 同期シム（sync.js）とも
// 干渉しない。viewer（term.js の meta filter）は xterm へ流す前に剥がす。
package bridge

import (
	"fmt"
	"strings"
)

const (
	metaOSC = "\x1b]7707;"
	metaST  = "\x1b\\"

	// HistoryLines は履歴取得の行数上限。herdr 0.9.5 の pane.read は
	// これ以上指定しても 1000 行で truncated になる（実測）。
	HistoryLines = 1000
)

// MetaBegin はストリーム先頭の宣言。viewer はこれを見て v2 モードへ切り替える
// （端末を cols×rows に合わせ scrollback を有効化）。trunc は履歴が herdr 側の
// 上限で切れているか（UI の「これ以上遡れない」表示用）。
func MetaBegin(cols, rows int, truncated bool) []byte {
	tr := 0
	if truncated {
		tr = 1
	}
	return []byte(fmt.Sprintf("%sv=2;cols=%d;rows=%d;trunc=%d%s", metaOSC, cols, rows, tr, metaST))
}

// MetaHistEnd は履歴区間の終端。viewer は 2 回目以降の接続ではこの直前までを
// 捨てる（履歴は 1 ページにつき 1 回だけ scrollback に入れる）。
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
