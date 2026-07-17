// cmwire_test.go は CMWireParser の純関数テスト（テーブル駆動）。
// ワイヤ規約自体は cm コード実読の抽出（RESIZE/SCROLL/IMAGE の magic と
// バイト順）が一次情報＝ここでは「分割着信の再結合」「IMAGE またぎ」
// 「不正長 resync」「順序保存」を機械検証する。
package bridge

import (
	"bytes"
	"reflect"
	"testing"
)

// feedAll は chunks を順に Feed し全イベントを連結して返す。
func feedAll(p *CMWireParser, chunks ...[]byte) []Event {
	var evs []Event
	for _, c := range chunks {
		evs = append(evs, p.Feed(c)...)
	}
	return evs
}

// resizeBytes は RESIZE magic フレームを組み立てる（rows→cols の順は
// cm server.go 実コードの抽出どおり）。
func resizeBytes(rows, cols int) []byte {
	return []byte{0xff, 0xff, byte(rows >> 8), byte(rows), byte(cols >> 8), byte(cols)}
}

func scrollBytes(dy int16) []byte {
	return []byte{0xff, 0xfe, byte(uint16(dy) >> 8), byte(uint16(dy))}
}

func imageBytes(payloadLen int, ext byte, payload []byte) []byte {
	h := []byte{0xff, 0xfd,
		byte(payloadLen >> 24), byte(payloadLen >> 16), byte(payloadLen >> 8), byte(payloadLen),
		ext}
	return append(h, payload...)
}

func TestCMWireTable(t *testing.T) {
	img := bytes.Repeat([]byte{0xAB}, 100)
	cases := []struct {
		name   string
		chunks [][]byte
		want   []Event
	}{
		{
			name:   "RESIZE 一括着信",
			chunks: [][]byte{resizeBytes(24, 80)},
			want:   []Event{{Kind: EvResize, Rows: 24, Cols: 80}},
		},
		{
			name: "RESIZE 1バイトずつ分割着信（繰越し再結合）",
			chunks: func() [][]byte {
				b := resizeBytes(33, 100)
				var cs [][]byte
				for i := range b {
					cs = append(cs, b[i:i+1])
				}
				return cs
			}(),
			want: []Event{{Kind: EvResize, Rows: 33, Cols: 100}},
		},
		{
			name:   "SCROLL 負値（遡り）",
			chunks: [][]byte{scrollBytes(-5)},
			want:   []Event{{Kind: EvScroll, Dy: -5}},
		},
		{
			name: "SCROLL ヘッダ分割",
			chunks: [][]byte{
				{0xff}, {0xfe}, {0x00}, {0x03},
			},
			want: []Event{{Kind: EvScroll, Dy: 3}},
		},
		{
			name:   "IMAGE 一括（parse-and-drop・payload 非漏洩）",
			chunks: [][]byte{imageBytes(len(img), 1, img)},
			want:   []Event{{Kind: EvImage, ImageLen: 100, ImageExt: 1}},
		},
		{
			name: "IMAGE 3分割またぎ（ヘッダ途中/payload 途中で切る）",
			chunks: func() [][]byte {
				b := imageBytes(len(img), 2, img)
				return [][]byte{b[:4], b[4:50], b[50:]}
			}(),
			want: []Event{{Kind: EvImage, ImageLen: 100, ImageExt: 2}},
		},
		{
			name: "IMAGE の後続入力",
			chunks: [][]byte{
				append(imageBytes(len(img), 1, img), []byte("xy")...),
			},
			want: []Event{
				{Kind: EvImage, ImageLen: 100, ImageExt: 1},
				{Kind: EvInput, Input: []byte("xy")},
			},
		},
		{
			name: "IMAGE 不正長 0 は 7B resync（後続が入力に復帰）",
			chunks: [][]byte{
				append(imageBytes(0, 1, nil), []byte("abc")...),
			},
			want: []Event{{Kind: EvInput, Input: []byte("abc")}},
		},
		{
			name: "IMAGE 不正長 8MiB+1 は 7B resync",
			chunks: [][]byte{
				append(imageBytes(maxImageBytes+1, 1, nil), []byte("ok")...),
			},
			want: []Event{{Kind: EvInput, Input: []byte("ok")}},
		},
		{
			name:   "IMAGE 上限ちょうど 8MiB は受理（drop）",
			chunks: [][]byte{imageBytes(maxImageBytes, 3, bytes.Repeat([]byte{0x01}, maxImageBytes))},
			want:   []Event{{Kind: EvImage, ImageLen: maxImageBytes, ImageExt: 3}},
		},
		{
			name: "混在ストリームの順序保存",
			chunks: [][]byte{bytes.Join([][]byte{
				[]byte("ab"), resizeBytes(24, 80), []byte("cd"),
				scrollBytes(7), []byte("ef"),
			}, nil)},
			want: []Event{
				{Kind: EvInput, Input: []byte("ab")},
				{Kind: EvResize, Rows: 24, Cols: 80},
				{Kind: EvInput, Input: []byte("cd")},
				{Kind: EvScroll, Dy: 7},
				{Kind: EvInput, Input: []byte("ef")},
			},
		},
		{
			name: "入力末尾の孤立 0xff は保留→次 read で magic 再結合",
			chunks: [][]byte{
				append([]byte("ab"), 0xff),     // 0xff は保留（"ab" だけ確定）
				{0xff, 0x00, 0x18, 0x00, 0x50}, // 残りの RESIZE 本体
			},
			want: []Event{
				{Kind: EvInput, Input: []byte("ab")},
				{Kind: EvResize, Rows: 24, Cols: 80},
			},
		},
		{
			name: "0xff+非magic 第2バイトは生入力として素通し（cm 同一挙動）",
			chunks: [][]byte{
				{0xff, 0x41, 0x42},
			},
			want: []Event{{Kind: EvInput, Input: []byte{0xff, 0x41, 0x42}}},
		},
		{
			name: "生入力中間の 0xfe/0xfd 単独は magic でない（0xff 前置が必要）",
			chunks: [][]byte{
				{0x61, 0xfe, 0x62, 0xfd, 0x63},
			},
			want: []Event{{Kind: EvInput, Input: []byte{0x61, 0xfe, 0x62, 0xfd, 0x63}}},
		},
		{
			name: "入力→IMAGE payload に 0xff 0xff を含む（payload は magic 判定しない）",
			chunks: [][]byte{bytes.Join([][]byte{
				[]byte("hi"),
				imageBytes(4, 1, []byte{0xff, 0xff, 0x00, 0x18}),
				[]byte("lo"),
			}, nil)},
			want: []Event{
				{Kind: EvInput, Input: []byte("hi")},
				{Kind: EvImage, ImageLen: 4, ImageExt: 1},
				{Kind: EvInput, Input: []byte("lo")},
			},
		},
		{
			name: "連続 RESIZE（最後を採用するのは呼び出し側の責務・パーサは全部返す）",
			chunks: [][]byte{bytes.Join([][]byte{
				resizeBytes(24, 80), resizeBytes(50, 160),
			}, nil)},
			want: []Event{
				{Kind: EvResize, Rows: 24, Cols: 80},
				{Kind: EvResize, Rows: 50, Cols: 160},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &CMWireParser{}
			got := feedAll(p, tc.chunks...)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("events mismatch\n got=%+v\nwant=%+v", got, tc.want)
			}
		})
	}
}

// TestCMWireInputIsCopied は EvInput.Input が内部バッファと分離している
// （後続 Feed で書き換わらない）ことを検証する。イベントを channel 越しに
// 非同期処理する将来の呼び出し側でも安全なことの担保。
func TestCMWireInputIsCopied(t *testing.T) {
	p := &CMWireParser{}
	evs := p.Feed([]byte("hello"))
	if len(evs) != 1 || string(evs[0].Input) != "hello" {
		t.Fatalf("unexpected first events: %+v", evs)
	}
	saved := evs[0].Input
	_ = p.Feed([]byte("WORLD_OVERWRITE_ATTEMPT"))
	if string(saved) != "hello" {
		t.Fatalf("Input aliased internal buffer: %q", saved)
	}
}
