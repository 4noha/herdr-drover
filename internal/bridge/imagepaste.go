// imagepaste.go は Web ターミナルから届いた IMAGE フレームを、この PC の
// OS クリップボードへ載せる部分（cm internal/ptyproxy/server.go の
// handleImagePaste / setMacClipboardImage の移植）。
//
// なぜクリップボード経由か: **パス文字列を打鍵しても Claude Code は添付しない**
// （cm 側で実機検証済み・同じ轍を踏まないこと）。ホストのクリップボードに画像
// そのものを載せて Ctrl+V を注入するのが唯一効く経路。
//
// cm との差異:
//   - 注入先が pty ではなく herdr の pane＝Ctrl+V は既存の sendInput 経路で送る
//     （bridge.go 側）。ここはクリップボードへ載せるところまでを担う。
//   - 一時ファイルは drover の状態ディレクトリ配下（既定 ~/.herdr-drover/paste）。
//
// セキュリティ:
//   - 一時ファイルは dir 0700 / file 0600、名前は乱数 16 桁 hex＋既知拡張子
//     （traversal 不可＝ext は code から引いた定数のみ）。
//   - クリップボード投入に失敗したら**注入せずファイルも消す**（中途半端に
//     残さない）。
//   - TTL で掃除（claude が読み終えた頃）。
//   - ⚠ 共用 PC(slave) では呼ばれない。同一アカウントの他人がクリップボードを
//     読めるため（DESIGN_SLAVE の脅威モデル）。gate は config 側。
package bridge

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

// imagePasteTTL は一時ファイルを消すまでの猶予（cm と同じ 5 分）。
const imagePasteTTL = 5 * time.Minute

// imageExtClass は ext コード（term.js の extOf と同値）を拡張子と
// AppleScript のクラス指定へ写す。cm imageExtClass と同一表。
func imageExtClass(code byte) (ext, asClass string, ok bool) {
	switch code {
	case 1:
		return "png", "«class PNGf»", true
	case 2:
		return "jpg", "«class JPEG»", true
	case 3:
		return "gif", "«class GIFf»", true
	}
	return "", "", false
}

// setClipboardImage は差し替え可能な seam（テストで実クリップボードを
// 汚さないため）。既定は macOS の osascript 経路。
var setClipboardImage = setMacClipboardImage

// setMacClipboardImage は画像ファイルを macOS の OS クリップボードへ載せる。
// Linux/Windows は未対応（cm と同じく v1 は macOS 主対象）＝明示エラーを返し、
// 呼び出し側がログに残して注入しない。
func setMacClipboardImage(path, ext string) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("画像ペーストは macOS のみ対応（GOOS=%s）", runtime.GOOS)
	}
	var asClass string
	switch ext {
	case "png":
		asClass = "«class PNGf»"
	case "jpg":
		asClass = "«class JPEG»"
	case "gif":
		asClass = "«class GIFf»"
	default:
		return fmt.Errorf("未対応拡張子: %s", ext)
	}
	scr := fmt.Sprintf("set the clipboard to (read (POSIX file %q) as %s)", path, asClass)
	out, err := exec.Command("osascript", "-e", scr).CombinedOutput()
	if err != nil {
		return fmt.Errorf("osascript: %v (%s)", err, out)
	}
	return nil
}

// landImage は payload を一時ファイル化してクリップボードへ載せる。
// 成功したら書いたパスを返す（呼び出し側は Ctrl+V を注入する）。
// 失敗時はファイルを残さない。
func landImage(dir string, payload []byte, code byte) (string, error) {
	ext, _, ok := imageExtClass(code)
	if !ok {
		return "", fmt.Errorf("未対応の ext コード: %d", code)
	}
	if len(payload) == 0 || len(payload) > maxImageBytes {
		return "", fmt.Errorf("payload 長が範囲外: %dB", len(payload))
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("paste ディレクトリ: %w", err)
	}
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", fmt.Errorf("乱数: %w", err)
	}
	path := filepath.Join(dir, hex.EncodeToString(rnd[:])+"."+ext)
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		return "", fmt.Errorf("一時ファイル: %w", err)
	}
	if err := setClipboardImage(path, ext); err != nil {
		_ = os.Remove(path) // 載せられないなら残さない・注入もしない
		return "", fmt.Errorf("クリップボード投入: %w", err)
	}
	time.AfterFunc(imagePasteTTL, func() { _ = os.Remove(path) })
	return path, nil
}

// defaultImagePasteDir は一時ファイルの既定置き場（~/.herdr-drover/paste）。
// HOME が取れない環境では OS の一時ディレクトリへ退避する。
func defaultImagePasteDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), "herdr-drover-paste")
	}
	return filepath.Join(home, ".herdr-drover", "paste")
}

// handleImage は EvImage を処理する。ImagePaste が false（既定）なら
// 監査ログだけ残して捨てる＝従来の挙動。
//
// silent に落とさない（鉄則 5）: 受け付けない時も、失敗した時も必ずログに出す。
// Web 側は「送った」としか分からないため、ここが唯一の一次情報になる。
func (b *Bridge) handleImage(ev Event) {
	if !b.ImagePaste {
		b.logf("IMAGE フレームを破棄 (%dB ext=%d)：画像貼付は無効（DROVER_WEB_IMAGE_PASTE）",
			ev.ImageLen, ev.ImageExt)
		return
	}
	dir := b.ImagePasteDir
	if dir == "" {
		dir = defaultImagePasteDir()
	}
	path, err := landImage(dir, ev.Image, ev.ImageExt)
	if err != nil {
		b.logf("IMAGE 取り込み失敗 (%dB ext=%d): %v", ev.ImageLen, ev.ImageExt, err)
		return
	}
	// クリップボードに載った後で Ctrl+V。順序を逆にすると空を貼る。
	if serr := b.sendInput([]byte{0x16}); serr != nil {
		b.logf("IMAGE: Ctrl+V 注入失敗 (%s): %v", path, serr)
		return
	}
	b.logf("IMAGE を貼付 (%dB ext=%d → %s)", ev.ImageLen, ev.ImageExt, path)
}
