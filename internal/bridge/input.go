// input.go は cm ワイヤから剥がした生入力バイトを herdr pane へ届ける。
// 経路選択は隔離 herdr 0.7.4 の実測バッテリ（raw capture pane で 2004
// ON/OFF 両方・hexdump 照合）で確定した決定木にのみ従う（推測・
// ヒューリスティック禁止）:
//
//	primary  : pane.send_text（ndjson API）
//	  実測: 「send_text・control terminal.input の 3 経路は、2004 ON/OFF に
//	  関わらず全テストバイト（ESC[A/\x03/\x1b単独/\t/\x7f/あ/絵文字/\r/\n/
//	  混合）を paste 括りなしで byte-perfect に透過する（真のキーストローク
//	  経路）」「send_text は 50KB payload も 0.65s で成功」
//	fallback : `herdr terminal session control <pane>` 一時接続の
//	  terminal.input{bytes:base64}（非 UTF-8 バイト列のみ）
//	  実測: 「API socket の send_input{bytes} と違い、control の bytes 形は
//	  別実装で正常に動く」。JSON 文字列は生の非 UTF-8 を運べないため、
//	  text 形で送れないバイト列はこの経路しかない。
//
// 不採用（実測で棄却・再導入禁止）:
//   - pane.send_input{text}: 「pane アプリが DECSET 2004 を有効化していると
//     payload 全体を \x1b[200~..\x1b[201~ で括って届ける（ESC[A も \x03 も
//     括りの中）」＝claude は ?2004h を有効化するため矢印・Ctrl-C が全て
//     paste 化して壊れる。さらにブラウザ xterm.js の paste は自分で 200~
//     括りするため二重括りになる。
//   - pane.send_input{bytes}: 「応答 {"type":"ok"} を返すが pane には
//     1 バイトも届かない（全 10 テストケースでマーカー間が空）」＝沈黙不達。
//     ok 応答を到達の証拠にしてはならない。
package bridge

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"time"
	"unicode/utf8"
)

// utf8LeadLen は b が UTF-8 先頭バイトなら rune 全長（1..4）、継続バイト
// （10xxxxxx）・不正先頭（0xF8-0xFF）なら 0 を返す（RFC 3629 の機械規則＝
// ヒューリスティックではない）。
func utf8LeadLen(b byte) int {
	switch {
	case b < 0x80:
		return 1
	case b&0xE0 == 0xC0:
		return 2
	case b&0xF0 == 0xE0:
		return 3
	case b&0xF8 == 0xF0:
		return 4
	default:
		return 0
	}
}

// splitIncompleteRune は data 末尾が「不完全な UTF-8 rune の先頭断片」
// （先頭バイト＋継続バイト 0-2 個で、宣言長に足りない）なら、その断片を
// tail として分離する。cmwire の末尾孤立 0xff 繰越しと同じ規律の UTF-8 版:
// relay は 32KB chunk で中継するため、CJK/絵文字ペーストは read 境界が
// rune を割り得る（実再現: 両断片が utf8.Valid false → control fallback →
// attach 副作用で実 PTY が 120x40 に resize された）。断片は次 read で
// 再結合して primary（send_text）に留める。
//
// 真に不正な列（不正先頭 0xFE/0xFF・孤立継続バイト・断片後の非継続バイト）
// は繰り越さず即返す＝fallback（control bytes）行きの判定は utf8.Valid が
// 従来どおり行う。
func splitIncompleteRune(data []byte) (head, tail []byte) {
	n := len(data)
	// 不完全断片は最大 3 バイト（utf8.UTFMax-1）＝末尾 3 バイトだけ見れば良い。
	for i := n - 1; i >= 0 && i >= n-3; i-- {
		l := utf8LeadLen(data[i])
		if l == 0 {
			continue // 継続 or 不正バイト＝さらに前の先頭バイトを探す
		}
		if i+l <= n {
			return data, nil // 末尾 rune は長さ充足（正当性判定は utf8.Valid）
		}
		// 宣言長に足りない。先頭バイト以降が全て継続バイトの時のみ
		// 「rune の先頭断片」＝繰越し対象（混じっていれば不正列＝即送出）。
		for j := i + 1; j < n; j++ {
			if data[j]&0xC0 != 0x80 {
				return data, nil
			}
		}
		return data[:i], data[i:]
	}
	return data, nil
}

// sendInput は生入力 1 チャンクを pane へ届ける。呼び出しは readConn の
// 1 goroutine から逐次＝キーストローク順序は保存される。
func (b *Bridge) sendInput(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	// xterm.js の onData は常に UTF-8 文字列（キー・IME・bracketed paste
	// いずれも）＝実運用の入力はほぼ 100% primary を通る。非 UTF-8 は
	// json.Marshal が U+FFFD へ置換してバイトを壊すため text 経路に
	// 載せてはならず、control bytes へ回す。
	if utf8.Valid(data) {
		return b.Herdr.PaneSendText(b.Sid, string(data))
	}
	return b.sendControlBytes(data)
}

// sendControlBytes は control 一時接続で terminal.input{bytes} を送る
// fallback（非 UTF-8 専用の保険経路）。
//
// ⚠実測済みの副作用: control は「attach しただけで実 PTY を resize する
// （フラグ無し既定 120x40、明示フラグはその値）」＋ローカル resize lock。
// PTY の現 cols は API から取れない（viewport_rows のみ）ため原寸復元は
// 不可能で、v1 はこの副作用を許容する（xterm.js 経路では実質発生しない
// 保険経路であることが許容の根拠）。--takeover は付けない＝既に writable
// owner（ローカル利用者）が居る場合に追放するより失敗する方を選ぶ。
func (b *Bridge) sendControlBytes(data []byte) error {
	cmd := exec.Command(b.herdrBin(), "terminal", "session", "control", b.Sid)
	cmd.Env = b.procEnv()
	// control は observe 同様 stdout に frame を吐く。読み捨てないと
	// pipe が詰まって入力処理ごと止まり得る。
	cmd.Stdout = io.Discard
	// stderr は捨てない（cm tmux.go new-window silent fail の教訓）: 失敗の
	// 真因（socket 不達・writable owner 競合等）は stderr にしか出ない。
	st := &tailBuf{}
	cmd.Stderr = st
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	msg, err := json.Marshal(struct {
		Type  string `json:"type"`
		Bytes string `json:"bytes"`
	}{"terminal.input", base64.StdEncoding.EncodeToString(data)})
	if err != nil {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return err
	}
	msg = append(msg, '\n')
	_, werr := stdin.Write(msg)
	// pipe は「データ→EOF」の順序を保証する＝サーバは入力行を読み切って
	// から Detach（stdin EOF）を見る。sleep 等の時間待ちは不要。
	_ = stdin.Close()
	if werr != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return fmt.Errorf("control stdin 書込失敗: %w (stderr=%q)", werr, st.String())
	}

	// Detach で自然終了するはず。ハングした場合の保険で kill（リーク禁止）。
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("control 失敗: %w (stderr=%q)", err, st.String())
		}
		if s := st.String(); s != "" {
			// 成功でも stderr の警告は診断に残す（沈黙禁止）。
			b.logf("control stderr: %q", s)
		}
		return nil
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		return fmt.Errorf("control が Detach 後 10s 終了せず kill (stderr=%q)", st.String())
	}
}
