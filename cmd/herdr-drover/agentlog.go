// agentlog.go — daemon のログ（~/.herdr-drover/agent.log）をサイズ上限で 1 世代ローテートする。
//
// 従来は launchd の StandardOut/ErrorPath が agent.log で、daemon は stderr に書くだけ＝
// 誰も切らず増え続けた（2026-11-03 実測: BUG-16 の張り直し嵐で 828MB）。daemon が自分で
// 開いた agent.log に書き、上限（agentLogMaxBytes）を超えたら agent.log.1 へ退避して
// 新しいファイルを作る（attach.log と同じ規律・捨てずに 1 世代残す・退避したことは新
// ファイルの先頭に必ず書く）。
//
// 配線（agentLogWriter）:
//   - stderr が端末（手動起動）なら従来どおり stderr だけ（ファイルを二重に持たない）。
//   - stderr が agent.log そのもの（launchd）なら、ファイルだけに書く（stderr に書くと
//     launchd 経由で同じファイルに二重に入る）。Go ランタイムの panic は stderr の fd に
//     直接出るので、launchd 経由で（ローテート後は agent.log.1 に）残る。
//   - それ以外（パイプ等＝テストや別の起動方法）は stderr とファイルの両方に書く。
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

const agentLogMaxBytes = 16 << 20

type rotatingLog struct {
	mu   sync.Mutex
	path string
	max  int64
	f    *os.File
	n    int64
}

// openRotatingLog は path を追記で開く。起動時に既に上限を超えていれば先に退避する。
// 戻り値 note は退避したときの説明（空なら退避なし）。
func openRotatingLog(path string, max int64) (*rotatingLog, string, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, "", err
	}
	note := ""
	if fi, err := os.Stat(path); err == nil && fi.Size() > max {
		if rerr := os.Rename(path, path+".1"); rerr == nil {
			note = fmt.Sprintf("（%s が %d バイトを超えたので %s.1 へ退避した）", filepath.Base(path), max, filepath.Base(path))
		}
	}
	r := &rotatingLog{path: path, max: max}
	if err := r.open(); err != nil {
		return nil, "", err
	}
	return r, note, nil
}

func (r *rotatingLog) open() error {
	f, err := os.OpenFile(r.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	r.f, r.n = f, fi.Size()
	return nil
}

// Write は上限を超えるなら先に退避してから書く。退避に失敗しても書き続ける（ログの
// 都合で daemon を止めない）。
func (r *rotatingLog) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.n+int64(len(p)) > r.max {
		if err := r.rotateLocked(); err != nil {
			fmt.Fprintf(r.f, "ローテート失敗（このまま書き続ける）: %v\n", err)
		}
	}
	n, err := r.f.Write(p)
	r.n += int64(n)
	return n, err
}

func (r *rotatingLog) rotateLocked() error {
	r.f.Close()
	if err := os.Rename(r.path, r.path+".1"); err != nil {
		_ = r.open() // 元のファイルを開き直す（追記）
		return err
	}
	if err := r.open(); err != nil {
		return err
	}
	_, err := fmt.Fprintf(r.f, "ローテート（%s が %d バイトを超えたので %s.1 へ退避した）\n",
		filepath.Base(r.path), r.max, filepath.Base(r.path))
	return err
}

// agentLogPath は daemon のログの既定パス（launchd の StandardOutPath と同じ）。
func agentLogPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".herdr-drover", "agent.log"), nil
}

// agentLogWriter は cmdAgent のログ出力先を決める（ファイル冒頭の配線を参照）。
// 戻り値 w が nil なら従来どおり stderr だけに書く（端末・パス不明）。
func agentLogWriter(stderr io.Writer) (w io.Writer, note string, err error) {
	f, ok := stderr.(*os.File)
	if !ok {
		return nil, "", nil
	}
	sfi, err := f.Stat()
	if err != nil || sfi.Mode()&os.ModeCharDevice != 0 {
		return nil, "", nil // 端末（手動起動）
	}
	path, err := agentLogPath()
	if err != nil {
		return nil, "", err
	}
	// 退避の前に判定する（退避後は stderr の fd が旧ファイル＝.1 を指し、同一判定が崩れる）。
	launchdOwns := false
	if lfi, err := os.Stat(path); err == nil && os.SameFile(sfi, lfi) {
		launchdOwns = true
	}
	rl, note, err := openRotatingLog(path, agentLogMaxBytes)
	if err != nil {
		return nil, "", err
	}
	if launchdOwns {
		return rl, note, nil
	}
	return io.MultiWriter(stderr, rl), note, nil
}
