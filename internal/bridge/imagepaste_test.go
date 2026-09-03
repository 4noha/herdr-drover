package bridge

// imagepaste_test.go — Web からの画像貼付（DROVER_WEB_IMAGE_PASTE）の担保。
//
// 既定 off の時に**従来どおり drop する**ことと、on の時に**実際にファイル化
// されて Ctrl+V が pane へ届く**ことの両方を見る。クリップボード投入は seam
// （setClipboardImage）で差し替えて実クリップボードを汚さない。
// Ctrl+V の到達だけは実 herdr の pane で byte 単位に確認する（合成ストリームで
// 緑にしない＝鉄則 2。BUG-3 の教訓）。

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCMWireKeepImage は KeepImage の有無で payload 保持が切り替わり、
// **どちらでもフレームは必ず consume される**ことを見る（consume が漏れると
// 画像バイトが打鍵として pane に流れる＝この規約が本機能の前提）。
func TestCMWireKeepImage(t *testing.T) {
	img := bytes.Repeat([]byte{0xAB}, 100)
	frame := append(imageBytes(len(img), 1, img), []byte("tail")...)

	t.Run("既定は drop（payload を持たない）", func(t *testing.T) {
		var p CMWireParser
		evs := p.Feed(frame)
		if len(evs) != 2 {
			t.Fatalf("イベント数 %d（IMAGE＋後続入力の 2 を期待）: %+v", len(evs), evs)
		}
		if evs[0].Kind != EvImage || evs[0].ImageLen != 100 || evs[0].ImageExt != 1 {
			t.Fatalf("IMAGE メタが不一致: %+v", evs[0])
		}
		if evs[0].Image != nil {
			t.Fatalf("KeepImage=false なのに payload を保持している（%dB）", len(evs[0].Image))
		}
		if evs[1].Kind != EvInput || string(evs[1].Input) != "tail" {
			t.Fatalf("後続入力が復帰していない: %+v", evs[1])
		}
	})

	t.Run("KeepImage で payload を複製して返す", func(t *testing.T) {
		var p CMWireParser
		p.KeepImage = true
		evs := p.Feed(frame)
		if len(evs) != 2 {
			t.Fatalf("イベント数 %d: %+v", len(evs), evs)
		}
		if !bytes.Equal(evs[0].Image, img) {
			t.Fatalf("payload が一致しない: got %dB want %dB", len(evs[0].Image), len(img))
		}
		// 複製であること: 以降の Feed で内部バッファが再確保されても
		// 既に返した Image が書き換わらない。
		p.Feed(bytes.Repeat([]byte("x"), 4096))
		if !bytes.Equal(evs[0].Image, img) {
			t.Fatal("後続 Feed で Image の中身が変わった（内部バッファを参照している）")
		}
		if evs[1].Kind != EvInput || string(evs[1].Input) != "tail" {
			t.Fatalf("後続入力が復帰していない: %+v", evs[1])
		}
	})

	t.Run("分割着信でも payload が揃う", func(t *testing.T) {
		var p CMWireParser
		p.KeepImage = true
		b := imageBytes(len(img), 3, img)
		var evs []Event
		for _, chunk := range [][]byte{b[:4], b[4:50], b[50:]} {
			evs = append(evs, p.Feed(chunk)...)
		}
		if len(evs) != 1 || evs[0].Kind != EvImage {
			t.Fatalf("IMAGE が 1 件にまとまらない: %+v", evs)
		}
		if !bytes.Equal(evs[0].Image, img) {
			t.Fatalf("分割着信で payload が壊れた: %dB", len(evs[0].Image))
		}
	})
}

// TestHandleImageDisabled は既定（off）で従来どおり捨てることを見る。
// クリップボードにも触らない＝seam が呼ばれない。
func TestHandleImageDisabled(t *testing.T) {
	called := 0
	orig := setClipboardImage
	setClipboardImage = func(path, ext string) error { called++; return nil }
	t.Cleanup(func() { setClipboardImage = orig })

	var logs []string
	b := &Bridge{Logf: func(f string, a ...any) { logs = append(logs, f) }}
	// ImagePaste は既定 false。
	b.handleImage(Event{Kind: EvImage, ImageLen: 3, ImageExt: 1, Image: []byte{1, 2, 3}})

	if called != 0 {
		t.Fatalf("無効なのにクリップボードへ触った（%d 回）", called)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "破棄") {
		t.Fatalf("破棄を silent に落としている（ログ: %v）", logs)
	}
}

// TestHandleImageEnabled は有効時に一時ファイル化＋クリップボード投入まで
// 到達し、パーミッションと中身が正しいことを見る。
func TestHandleImageEnabled(t *testing.T) {
	dir := t.TempDir()
	payload := bytes.Repeat([]byte{0x7f}, 512)

	var gotPath, gotExt string
	orig := setClipboardImage
	setClipboardImage = func(path, ext string) error { gotPath, gotExt = path, ext; return nil }
	t.Cleanup(func() { setClipboardImage = orig })

	got, err := landImage(dir, payload, 1)
	if err != nil {
		t.Fatalf("landImage: %v", err)
	}
	if got != gotPath || gotExt != "png" {
		t.Fatalf("seam に渡った値が不一致: path=%q(%q) ext=%q", gotPath, got, gotExt)
	}
	if filepath.Ext(got) != ".png" {
		t.Fatalf("拡張子が ext コードと一致しない: %s", got)
	}
	fi, err := os.Stat(got)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("一時ファイルの権限が %v（0600 であること＝同一マシンの他人に読ませない）", fi.Mode().Perm())
	}
	b, err := os.ReadFile(got)
	if err != nil || !bytes.Equal(b, payload) {
		t.Fatalf("payload が一致しない（err=%v len=%d）", err, len(b))
	}
}

// TestLandImageClipboardFailureLeavesNothing はクリップボード投入に失敗したら
// **注入せずファイルも残さない**ことを見る（中途半端な状態を作らない）。
func TestLandImageClipboardFailureLeavesNothing(t *testing.T) {
	dir := t.TempDir()
	orig := setClipboardImage
	setClipboardImage = func(path, ext string) error { return os.ErrPermission }
	t.Cleanup(func() { setClipboardImage = orig })

	if _, err := landImage(dir, []byte{1, 2, 3}, 1); err == nil {
		t.Fatal("クリップボード失敗がエラーとして返っていない")
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(ents) != 0 {
		t.Fatalf("失敗時にファイルが残った: %v", ents)
	}
}

// TestHandleImageWithoutPayloadDoesNotInject は **配線が外れた時に黙って
// 空を貼らない**ことを見る退行ガード。
//
// ImagePaste=true なのに Event.Image が空（＝CMWireParser.KeepImage の配線を
// 落とした状態＝この機能の前の drover の挙動そのもの）で Ctrl+V を注入して
// しまうと、利用者には「貼れたのに中身が古い/空」という最も分かりにくい壊れ方に
// なる。landImage が先に失敗し、注入せずログに残ることを固定する。
func TestHandleImageWithoutPayloadDoesNotInject(t *testing.T) {
	called := 0
	orig := setClipboardImage
	setClipboardImage = func(path, ext string) error { called++; return nil }
	t.Cleanup(func() { setClipboardImage = orig })

	var logs []string
	b := &Bridge{Logf: func(f string, a ...any) { logs = append(logs, f) }}
	b.ImagePaste = true
	b.ImagePasteDir = t.TempDir()
	// Herdr=nil＝注入まで到達したら nil 参照で panic する＝到達しないことの担保。
	b.handleImage(Event{Kind: EvImage, ImageLen: 100, ImageExt: 1, Image: nil})

	if called != 0 {
		t.Fatalf("payload 無しでクリップボードへ触った（%d 回）", called)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "取り込み失敗") {
		t.Fatalf("失敗を silent に落としている（ログ: %v）", logs)
	}
}
