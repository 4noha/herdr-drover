package main

// 端末応答（DA1 など）を入力から捨てるフィルタ（BUG-14）。
//
// 何が起きていたか（実測 2026-10-26）:
//
//	mac-studio の素シェル 17 枚の入力行に `62;22c62;22c…` が最大 197 回打ち込まれていた。
//	↗窓 の attach は herdr の pane の中で動き、その pane 入力（stdin）を raw のまま
//	リモートへ転送する。herdr 0.9.5 は pane 内のプログラムが端末へ問い合わせると、
//	**応答を pane の入力として返す**（隔離 server で再現: `ESC[c` → `ESC[?62;22c`）。
//	その応答がキー入力と同じ道でリモートへ届き、リモートのシェルに打ち込まれていた。
//	リモートの問い合わせには、リモート自身の端末（リモート側の herdr）が既に答えて
//	いる＝こちらの応答は**重複**で、捨てて失うものは無い。
//
// 方針（鉄則③: ヒューリスティック分類をしない）:
//   - 捨てるのは、herdr 0.9.5 が実際に返した応答（下の採取表）のうち、**キー入力・
//     マウス・貼り付けと構造的に区別できる形**だけ。迷う形は通す（取りこぼした
//     打鍵はユーザーに見えないが、混入は見える＝通す側に倒す）
//   - **途中で切れた列は保留せず通す**（保留すると単独 Esc などの打鍵が次の入力まで
//     遅れる）。herdr は応答を 1 回の write で返すので、通常は 1 回の Read に収まる
//   - 貼り付け（ESC[200~ … ESC[201~）の中身には手を付けない
//
// herdr 0.9.5 の採取表（2026-10-26・隔離 server・問い合わせ → 応答）:
//
//	DA1      ESC[c         → ESC[?62;22c        捨てる（CSI ? … c）
//	DA2      ESC[>c        → ESC[>1;0;0c        捨てる（CSI > … c）
//	DSR      ESC[5n        → ESC[0n             捨てる（CSI … n。n で終わるキーは無い）
//	DECRQM   ESC[?2026$p   → ESC[?2026;2$y      捨てる（CSI … $y）
//	kitty    ESC[?u        → ESC[?0u            捨てる（CSI ? … u。キー事象は ? 無し）
//	DA3      ESC[=c        → ESC P !|00000000 ESC\   捨てる（完結した DCS）
//	XTVERSION ESC[>0q      → ESC P >|libghostty ESC\ 捨てる（完結した DCS）
//	DECRQSS  ESC P $qm ESC\ → ESC P 1$r0m ESC\       捨てる（完結した DCS）
//	OSC 4    ESC]4;0;?BEL  → ESC]4;0;rgb:…ESC\       捨てる（完結した OSC）
//	CPR      ESC[6n        → ESC[1;1R           **通す**（修飾付き F3 = ESC[1;2R と同形）
//
// Alt+] は ESC ]、Alt+Shift+P は ESC P になるが、1 打鍵は終端（BEL / ESC\）まで
// 揃わないので OSC/DCS としては捨てない。

// termReplyFilter は pane 入力から端末応答を取り除く。貼り付け中かどうかだけを
// Read をまたいで覚える（それ以外の状態は持たない＝途中の列は保留しない）。
type termReplyFilter struct {
	inPaste bool
}

// Filter は p から端末応答を除いた列と、除いた応答の一覧を返す。
func (f *termReplyFilter) Filter(p []byte) (out []byte, dropped [][]byte) {
	out = make([]byte, 0, len(p))
	for i := 0; i < len(p); {
		if p[i] != 0x1b || i+1 >= len(p) {
			out = append(out, p[i])
			i++
			continue
		}
		var n int     // ESC から始まる列の長さ（0 = 完結していない／列ではない）
		var drop bool // 端末応答として捨てるか
		switch p[i+1] {
		case '[':
			n, drop = f.csi(p[i:])
		case ']':
			n = stringSeqLen(p[i:], true)
			drop = n > 0 && !f.inPaste
		case 'P':
			n = stringSeqLen(p[i:], false)
			drop = n > 0 && !f.inPaste
		}
		if n == 0 {
			out = append(out, p[i])
			i++
			continue
		}
		if drop {
			dropped = append(dropped, append([]byte(nil), p[i:i+n]...))
		} else {
			out = append(out, p[i:i+n]...)
		}
		i += n
	}
	return out, dropped
}

// csi は p（ESC [ で始まる）を CSI として読み、完結していれば長さと、端末応答として
// 捨てるべきかを返す。貼り付けの開始・終了もここで追う。
func (f *termReplyFilter) csi(p []byte) (n int, drop bool) {
	j := 2
	for j < len(p) && p[j] >= 0x30 && p[j] <= 0x3f { // パラメータ（数字・; ? > = <）
		j++
	}
	params := p[2:j]
	for j < len(p) && p[j] >= 0x20 && p[j] <= 0x2f { // 中間バイト（$ など）
		j++
	}
	inter := p[2+len(params) : j]
	if j >= len(p) || p[j] < 0x40 || p[j] > 0x7e {
		return 0, false // 終端が無い＝完結していない（保留せず通す）
	}
	final := p[j]
	n = j + 1

	// 貼り付けの開始・終了（ESC[200~ / ESC[201~）。境界そのものは通す。
	if final == '~' && len(inter) == 0 {
		switch string(params) {
		case "200":
			f.inPaste = true
		case "201":
			f.inPaste = false
		}
		return n, false
	}
	if f.inPaste {
		return n, false
	}

	prefix := byte(0)
	if len(params) > 0 && (params[0] == '?' || params[0] == '>' || params[0] == '=' || params[0] == '<') {
		prefix = params[0]
	}
	switch {
	case final == 'c' && (prefix == '?' || prefix == '>') && len(inter) == 0:
		return n, true // DA1 / DA2 の応答
	case final == 'n' && len(inter) == 0 && (prefix == 0 || prefix == '?'):
		return n, true // DSR の応答
	case final == 'y' && string(inter) == "$":
		return n, true // DECRQM の応答（DECRPM）
	case final == 'u' && prefix == '?' && len(inter) == 0:
		return n, true // kitty keyboard のフラグ応答
	}
	return n, false
}

// stringSeqLen は p（ESC ] か ESC P で始まる）が**終端まで揃った** OSC/DCS なら長さを
// 返す。osc は BEL 終端も認める。本体は表示可能 ASCII だけを許す（応答はそうなっている。
// 制御文字が混ざるなら応答ではない＝通す）。終端が無ければ 0。
func stringSeqLen(p []byte, osc bool) int {
	for j := 2; j < len(p); j++ {
		switch {
		case osc && p[j] == 0x07:
			return j + 1
		case p[j] == 0x1b:
			if j+1 < len(p) && p[j+1] == '\\' {
				return j + 2
			}
			return 0
		case p[j] < 0x20 || p[j] > 0x7e:
			return 0
		}
	}
	return 0
}
