package agentfwd

import (
	"context"
	"net"
	"os"
)

// SlaveSocket は path に新しい unix socket を作って listener を返す
// （SSH_AUTH_SOCK として使う slave 端の入口）。前回の異常終了で残った stale
// socket は除去してから listen する。呼び出し側は Slave() に渡し、撤去時に
// path を削除する。
//
// パーミッションは 0600（別 UID ユーザーには閉じる）。⚠同一 UID を共有する
// 共用 PC の他人はこのパーミッションでは防げない＝**owner Mac 側の
// `ssh-add -c`（毎署名 confirm）が本命の安全弁**（DESIGN_SSH_FORWARD.md）。
func SlaveSocket(path string) (net.Listener, error) {
	_ = os.Remove(path) // stale 除去（残骸 socket を掴まない）
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		_ = os.Remove(path)
		return nil, err
	}
	return ln, nil
}

// Owner は owner（自機）端を回す: relay パイプ上の各チャネルを owner の
// ssh-agent socket（authSock＝$SSH_AUTH_SOCK）へ dial して転送する。署名は
// owner の agent が実行＝秘密鍵は slave へ出ない。ctx 終了/切断で戻る。
func Owner(ctx context.Context, relay net.Conn, authSock string) error {
	return ServeDialer(ctx, relay, func() (net.Conn, error) {
		return net.Dial("unix", authSock)
	})
}

// Slave は slave（共用 PC）端を回す: relay パイプ上のチャネルを local socket
// （ln＝SlaveSocket が作ったもの）へ落とす。git/ssh が SSH_AUTH_SOCK 経由で
// この ln に繋ぎ、署名要求が relay 越しに owner へ渡る。ctx 終了/切断で戻る。
func Slave(ctx context.Context, relay net.Conn, ln net.Listener) error {
	return ServeListener(ctx, relay, ln)
}
