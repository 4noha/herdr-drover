package main

// pidfile（~/.herdr-drover/agent.pid）は nudge→daemon の SIGUSR1 経路と
// status の生存表示の唯一の根拠。SIGKILL や launchd 強制再起動では削除
// defer が走らず stale が残る（cm の dead PID sweep 教訓）ため、読む側は
// 常に pidAlive で実生存を確認し、pidfile の存在自体を信用しない。

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// pidfilePath は ~/.herdr-drover/agent.pid を返す。
func pidfilePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home ディレクトリ不明（pidfile 置き場を決められない）: %w", err)
	}
	return filepath.Join(home, ".herdr-drover", "agent.pid"), nil
}

// writePidfile は pid を tmp→rename で原子的に書く（cm WriteStatus 教訓:
// truncate 直書きは 0B の瞬間が実観測される＝reader が壊れた値を読み得る）。
// tmp 名は os.CreateTemp で呼出し毎に一意にする。固定 path+".tmp" は writer
// が並行すると tmp を共有し、片方が rename した後にもう片方の rename が
// ENOENT で失敗する（実バイナリ 8 並行起動 ×10 ラウンドの実測で全ラウンド
// 発生＝レビュー指摘で確定した実バグ。回帰: TestWritePidfileConcurrentWriters）。
func writePidfile(path string, pid int) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp") // 0600 で作られる
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.WriteString(strconv.Itoa(pid) + "\n"); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp) // rename 失敗時の tmp 残骸を掃除（best-effort）
		return err
	}
	return nil
}

// acquirePidfile は二重起動ゲートと pidfile 書込を「flock 保持」で原子化
// する。返る *os.File（ロックファイル）はプロセス生存期間中保持し続ける
// こと（Close で解放される）。
//
// 旧実装の readPidfile→pidAlive→writePidfile は check-then-write の TOCTOU
// で、同時起動（launchd KeepAlive と手動起動の競合等）に対しゲートが破れる
//（実バイナリ 8 並行起動 ×10 ラウンドの実測で 9 ラウンドが多重通過＝
// レビュー指摘で確定）。二重 agent は producer の in-memory 差分検出を壊す
//（agent.go の不変条件）ため、判定は flock(LOCK_EX|LOCK_NB) の成否のみに
// 一本化する:
//   - flock はカーネルがプロセス消滅（SIGKILL 含む）で自動解放する＝stale
//     lock が存在しない。O_CREATE|O_EXCL 方式に必要な「stale を unlink して
//     再試行」は、その unlink 自体が新たな race になる（A と B が同じ stale
//     を見て、B が A の獲得直後の lock を消し得る）ので採らない。
//   - pidfile 本体は従来どおり nudge/status の読み物として tmp→rename で
//     書く（ロックとは分離。SIGKILL 後に stale な pid が残るのは従来同様で、
//     読む側の pidAlive 検査が引き続き担保する）。
// 回帰: TestAcquirePidfileSingleWinner（本パッケージ）＋
// TestE2EConcurrentStartSingleWinner（test/・実バイナリ 8 並行起動）。
func acquirePidfile(path string, pid int) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		// 拒否の判定は flock のみ（原子）。pid の名指しは診断用の best-effort
		//（勝者が pidfile を書く前ならまだ旧値/不在のことがある）。
		if old, rerr := readPidfile(path); rerr == nil && pidAlive(old) {
			return nil, fmt.Errorf("agent は既に稼働中（pid %d・pidfile %s）。二重起動は producer の差分検出を壊すので拒否", old, path)
		}
		return nil, fmt.Errorf("agent は既に稼働中（%s.lock を別プロセスが保持）。二重起動は producer の差分検出を壊すので拒否", path)
	}
	if err := writePidfile(path, pid); err != nil {
		lock.Close()
		return nil, fmt.Errorf("pidfile 書込失敗: %w", err)
	}
	return lock, nil
}

// readPidfile は pidfile から pid を読む。不在は fs.ErrNotExist をそのまま
// 返す（呼び手が「daemon 不在」と区別できるように包まない）。
func readPidfile(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("pidfile %s が壊れている: %q", path, string(b))
	}
	return pid, nil
}

// pidAlive は pid の実生存を signal 0 で判定する。EPERM は「存在するが
// 権限がない」＝生存扱い（cm diag の isAlive と同じ規約）。
func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM)
}
