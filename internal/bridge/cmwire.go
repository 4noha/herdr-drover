// cmwire.go は cm（claude-master-go）の client→server ワイヤ規約の
// 受信側パーサ。cm の Web viewer（term.js）/ CLI viewer（internal/client）が
// relay 越しに送ってくるバイト列から magic フレームを剥がし、残りを
// 「pane への生入力」として切り出す。
//
// 規約（cm internal/ptyproxy/server.go の実コード抽出＝一次情報）:
//
//	RESIZE = 0xff 0xff + uint16BE rows + uint16BE cols
//	SCROLL = 0xff 0xfe + int16BE dy（負=遡り）
//	IMAGE  = 0xff 0xfd + uint32BE len + uint8 extCode + payload
//	その他 = pane への生入力
//
// cm の parseClientInput と同じ判定順（RESIZE→SCROLL→IMAGE→生入力）・同じ
// resync 規則（IMAGE 不正長はヘッダ 7B だけ捨てて再同期）を踏襲する。
// herdr-drover では:
//   - SCROLL は v1 では無視（DESIGN: herdr の terminal.scroll は共有 runtime
//     状態＝ローカル表示にも影響するため、リモート scrollback 非対応が明示制約）。
//     ただし規約上は必ず parse-and-consume する（漏らすと 4B が打鍵化する）。
//   - IMAGE は parse-and-drop 必須（DESIGN: 漏れると画像バイトが打鍵として
//     pane に流れる）。payload は保持しない。
//
// cm との意図的な差異（改良）: cm は「先頭でない末尾孤立 0xff」を即 master へ
// 転送する（parseClientInput の cut スキャンは i+1 < len までしか見ない）ため、
// magic ヘッダが 1 バイト目で分割着信すると 0xff が入力に漏れる。本パーサは
// 末尾の孤立 0xff を次 read まで繰り越す（キーボード/paste 入力は UTF-8 で
// 0xff を含まないので保留による実害はなく、分割着信の再結合が確実になる）。
package bridge

import "encoding/binary"

// EventKind は Feed が返すイベント種別。
type EventKind int

const (
	// EvInput は pane へ届けるべき生入力バイト。
	EvInput EventKind = iota
	// EvResize は viewer の表示サイズ通知（observe respawn の契機）。
	EvResize
	// EvScroll は viewer のスクロール要求（v1 は呼び出し側で無視）。
	EvScroll
	// EvImage は画像貼付フレーム（payload は drop 済み。監査ログ用の
	// メタ情報のみ保持）。
	EvImage
)

// maxImageBytes は IMAGE payload の上限（8MiB）。cm と同じく DoS／メモリ
// 膨張防止で、これを超える長さ宣言は「不正長」としてヘッダごと捨てて
// resync する。
const maxImageBytes = 8 << 20

// Event は cm ワイヤから切り出した 1 イベント。
type Event struct {
	Kind EventKind

	// EvResize
	Rows, Cols int

	// EvScroll（v1 無視だが監査のため値は運ぶ）
	Dy int

	// EvImage（payload は drop 済み）
	ImageLen int
	ImageExt byte

	// EvInput（内部バッファから複製済み＝呼び出し側が保持してよい）
	Input []byte
}

// CMWireParser は分割着信を繰り越しながら cm ワイヤを解析する状態機械。
// ゼロ値で使用可。goroutine 安全ではない（1 conn = 1 読み手の規律は
// cm relay の takeover 修正と同じ）。
type CMWireParser struct {
	in []byte // 未解析の繰越しバッファ
}

// Feed は受信バイトを追加し、確定したイベント列を順序保存で返す。
// ヘッダ／payload が未着なら内部に繰り越し、次回の Feed で再結合する。
func (p *CMWireParser) Feed(data []byte) []Event {
	p.in = append(p.in, data...)
	var evs []Event
	for {
		// --- RESIZE（cm parseClientInput と同順・同判定） ---
		if len(p.in) >= 2 && p.in[0] == 0xff && p.in[1] == 0xff {
			if len(p.in) < 6 {
				return evs // ヘッダ途中＝繰越し
			}
			evs = append(evs, Event{
				Kind: EvResize,
				Rows: int(binary.BigEndian.Uint16(p.in[2:4])),
				Cols: int(binary.BigEndian.Uint16(p.in[4:6])),
			})
			p.in = p.in[6:]
			continue
		}
		// --- SCROLL ---
		if len(p.in) >= 2 && p.in[0] == 0xff && p.in[1] == 0xfe {
			if len(p.in) < 4 {
				return evs
			}
			evs = append(evs, Event{
				Kind: EvScroll,
				Dy:   int(int16(binary.BigEndian.Uint16(p.in[2:4]))),
			})
			p.in = p.in[4:]
			continue
		}
		// --- IMAGE（parse-and-drop） ---
		if len(p.in) >= 2 && p.in[0] == 0xff && p.in[1] == 0xfd {
			if len(p.in) < 7 {
				return evs // ヘッダ未着（2 magic + 4 len + 1 ext）
			}
			n := int(binary.BigEndian.Uint32(p.in[2:6]))
			ext := p.in[6]
			if n <= 0 || n > maxImageBytes {
				// 不正長はヘッダ 7B だけ捨てて resync（cm と同一規則）。
				// 以降のバイトは通常解析に戻る。
				p.in = p.in[7:]
				continue
			}
			if len(p.in) < 7+n {
				return evs // payload 未着＝繰越し（上限 8MiB は上で保証済み）
			}
			// payload は drop。監査用メタのみ通知。
			p.in = p.in[7+n:]
			evs = append(evs, Event{Kind: EvImage, ImageLen: n, ImageExt: ext})
			continue
		}
		// --- 末尾の孤立 0xff は magic 先頭の可能性があるため繰り越す ---
		if len(p.in) == 1 && p.in[0] == 0xff {
			return evs
		}
		if len(p.in) == 0 {
			return evs
		}
		// --- 生入力: 次の magic 候補（0xff+{ff,fe,fd}）まで切り出す ---
		// 0xff の直後が magic 第 2 バイトでなければ 0xff も生入力として
		// 素通し（cm と同一挙動）。末尾が 0xff で終わる場合はそこで切り、
		// 0xff は次 read まで保留する（cm からの改良点・冒頭コメント参照）。
		cut := len(p.in)
		for i := 0; i < len(p.in); i++ {
			if p.in[i] != 0xff {
				continue
			}
			if i+1 >= len(p.in) {
				cut = i // 末尾 0xff＝判定保留
				break
			}
			if b := p.in[i+1]; b == 0xff || b == 0xfe || b == 0xfd {
				cut = i
				break
			}
		}
		if cut == 0 {
			// 先頭が magic（上の分岐で処理済み）か孤立 0xff（上で保留済み）
			// なのでここには来ないはずだが、防御的に無限ループを避ける。
			return evs
		}
		evs = append(evs, Event{
			Kind:  EvInput,
			Input: append([]byte(nil), p.in[:cut]...), // 繰越しバッファと分離
		})
		p.in = p.in[cut:]
	}
}
