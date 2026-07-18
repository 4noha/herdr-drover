//go:build unix

package main

// ssh-forward — slave（共用 PC）へ owner（自機）の SSH エージェントを一時転送する
// （GitHub 操作。設計は DESIGN_SSH_FORWARD.md）。秘密鍵は owner の ssh-agent から
// 出ず、署名は owner Mac が実行する。転送路は agentfwd mux（実 relay 越し検証済）。
//
// 既存機構の丸ごと再利用（コマンド線・state・web の変更ゼロ）:
//   - トリガ = **wake**（attach と同じ）。owner が afSid で slave を起こす。
//   - 認可   = **grant**（owner=viewer / slave=source。attach/webterm と同一）。
//   - slave の handleWake が **afSid を bridge でなく agentfwd.Slave へ分岐**する。
//
// エージェント対エージェント運用（ユーザー確定の用途「repo A をローカルと slave
// 両方で検証」）: owner の claude が本コマンドで転送ウィンドウを開き、slave の
// claude に `SSH_AUTH_SOCK=<sock> git clone/pull …` をインライン実行させ、済んだら
// Ctrl-C（or プロセス kill）で撤去する。人間の毎署名 confirm は任意（監視用）。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/4noha/drover-cloud/relayclient"
	"github.com/4noha/drover-cloud/state"
	"github.com/4noha/herdr-drover/internal/agentfwd"
)

// sshfwdPrefix は agent-forward 用 sid の識別子。pane_id（例 w1:p2）とも
// 注入 sid（<sid>#inj）とも衝突しない＝slave handleWake が exact-prefix で
// 分岐できる（ヒューリスティックでない）。
const sshfwdPrefix = "afwd:"

// sshfwdGrantTTL は grant の寿命（webterm/attach と同値。接続時のみ検査され
// るので短くてよい＝漏洩 grant の悪用窓を最小化）。
const sshfwdGrantTTL = 60 * time.Second

func isSSHForwardSid(sid string) bool { return strings.HasPrefix(sid, sshfwdPrefix) }

// sshForwardSockName は afSid からファイル名安全な basename を導出する
// （owner/slave 双方が同一に計算＝owner が表示する ~ パスと slave が作る実体が
// 一致する）。英数と - 以外を - に落とす（exact 変換＝非ヒューリスティック）。
func sshForwardSockName(afSid string) string {
	var b strings.Builder
	for _, r := range afSid {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

// sshForwardSockPath は slave 上の SSH_AUTH_SOCK 絶対パス（実体作成用）。
// ~/.herdr-drover/agent-fwd/<name>.sock（macOS の sun_path 104 上限に収まる短さ）。
func sshForwardSockPath(afSid string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".herdr-drover", "agent-fwd", sshForwardSockName(afSid)+".sock"), nil
}

// sshForwardSockDisplay は owner が slave の claude に伝える ~ 相対パス
// （slave のシェルが ~ を展開＝実体と同一ファイル）。
func sshForwardSockDisplay(afSid string) string {
	return "~/.herdr-drover/agent-fwd/" + sshForwardSockName(afSid) + ".sock"
}

// cmdSSHForward は owner 側 CLI: `herdr-drover ssh-forward <pc> [label]`。
// afSid で slave を起こし、owner の ssh-agent を relay 越しに転送する。Ctrl-C
// （SIGINT/SIGTERM）まで生存し、切断は backoff 再接続する。
func cmdSSHForward(args []string, stdout, stderr io.Writer) error {
	if len(args) < 1 {
		return fmt.Errorf("%w: herdr-drover ssh-forward <pc> [label]", errUsage)
	}
	remotePC := args[0]

	authSock := os.Getenv("SSH_AUTH_SOCK")
	if authSock == "" {
		return fmt.Errorf("SSH_AUTH_SOCK 未設定: owner で ssh-agent が要る（推奨: 専用 deploy key を `ssh-add -c ~/.ssh/<key>` で confirm 付き登録）")
	}

	// afSid = afwd:<label|nonce>。label 指定なら予測可能（agent が socket パスを
	// 構成しやすい）／未指定は乱数（衝突/stale 回避）。
	tail := ""
	if len(args) >= 2 && args[1] != "" {
		tail = sshForwardSockName(args[1]) // ラベルも安全化
	} else {
		var b [8]byte
		_, _ = rand.Read(b[:])
		tail = hex.EncodeToString(b[:])
	}
	afSid := sshfwdPrefix + tail

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	disp := sshForwardSockDisplay(afSid)
	fmt.Fprintf(stdout, "SSH agent 転送を開始: pc=%s afSid=%s\n", remotePC, afSid)
	fmt.Fprintf(stdout, "  owner agent : %s\n", authSock)
	fmt.Fprintf(stdout, "  slave sock  : %s\n", disp)
	fmt.Fprintf(stdout, "  ⇒ slave 側でこう実行させる:\n")
	fmt.Fprintf(stdout, "      SSH_AUTH_SOCK=%s git clone/pull ...\n", disp)
	fmt.Fprintf(stdout, "  （毎署名の confirm を出したい場合は owner で `ssh-add -c` 登録。Ctrl-C で撤去）\n")

	backoff := 500 * time.Millisecond
	for {
		if ctx.Err() != nil {
			return nil
		}
		start := time.Now()
		sshForwardCycle(ctx, remotePC, afSid, authSock, stderr)
		if ctx.Err() != nil {
			return nil
		}
		if time.Since(start) > 5*time.Second {
			backoff = 500 * time.Millisecond
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// sshForwardCycle は 1 接続サイクル（設定解決→state→grant/wake→viewer dial→
// agentfwd.Owner）。どの段階の失敗でも exit せず戻る（上位 backoff が再試行）。
func sshForwardCycle(ctx context.Context, remotePC, afSid, authSock string, stderr io.Writer) {
	cfg, err := resolveConfig()
	if err != nil {
		fmt.Fprintf(stderr, "ssh-forward: 設定解決失敗（再試行）: %v\n", err)
		return
	}
	clouds := cfg.LoadClouds()
	if len(clouds) == 0 {
		fmt.Fprintf(stderr, "ssh-forward: 接続先クラウド未設定（再試行）\n")
		return
	}
	cl := clouds[0] // primary（reconcile と同じクラウドで他 PC を見ている）

	creds := cl.SAKeyPath
	if os.Getenv("FIRESTORE_EMULATOR_HOST") != "" {
		creds = ""
	}
	cctx, ccancel := context.WithCancel(ctx)
	defer ccancel()
	st, err := state.NewWithCredentials(cctx, cl.Project, cl.PCName, creds)
	if err != nil {
		fmt.Fprintf(stderr, "ssh-forward: Firestore 接続失敗（再試行）: %v\n", err)
		return
	}
	defer st.Close()

	// viewer 許可を置き、slave を起こす（slave が source を agentfwd.Slave に張る）。
	gctx, gcancel := context.WithTimeout(cctx, 10*time.Second)
	_ = st.PutRelayGrant(gctx, afSid, "viewer", sshfwdGrantTTL)
	_ = st.Wake(gctx, remotePC, afSid)
	gcancel()

	dctx, dcancel := context.WithCancel(cctx)
	defer dcancel()
	// spc=remotePC ＝ relay KeyFor が slave source PC の時 slaveSessionKey で
	// ペアさせる（attach/↗窓 と同一。master source PC なら spc 無視＝同一 wire）。
	conn, err := relayclient.DialViewerFrom(dctx, cl.RelayURL, afSid, remotePC)
	if err != nil {
		fmt.Fprintf(stderr, "ssh-forward: relay 接続失敗（再試行）: %v\n", err)
		return
	}
	defer conn.Close()

	// owner 端: 各チャネルを owner の ssh-agent socket へ dial して転送。
	// ctx 終了（Ctrl-C）/切断で戻る。
	_ = agentfwd.Owner(dctx, conn, authSock)
}

// handleSSHForwardWake は afSid の wake を処理する（webterm.handleWake から分岐）。
// bridge の代わりに SSH_AUTH_SOCK 用 local socket を作り agentfwd.Slave を回す。
// owner 切断/ctx 終了で戻り socket を撤去する。dedup（active map）・grant・
// dialSource seam は bridge 経路と共有＝slave の bearer 認可も同一。
func (w *webTerm) handleSSHForwardWake(ctx context.Context, afSid string) {
	bctx, bcancel := context.WithCancel(ctx)
	defer bcancel()
	done := make(chan struct{})
	w.mu.Lock()
	if w.active[afSid] != nil {
		w.mu.Unlock()
		bcancel()
		w.lg.Printf("webterm: 多重 wake 無視 afSid=%q（SSH転送 稼働中）", afSid)
		return
	}
	w.active[afSid] = &bridgeRun{cancel: bcancel, done: done}
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		delete(w.active, afSid)
		w.mu.Unlock()
		close(done)
	}()

	// source 許可 → relay へ source dial（bridge と同一。slave は bearer 付き seam）。
	_ = w.st.PutRelayGrant(bctx, afSid, "source", sourceGrantTTL)
	dial := w.dialSource
	if dial == nil {
		dial = func(c context.Context, s string) (net.Conn, error) {
			return relayclient.Dial(c, w.relayURL, s, "source")
		}
	}
	conn, err := dial(bctx, afSid)
	if err != nil {
		w.lg.Printf("webterm: SSH転送 relay dial 失敗 afSid=%q: %v", afSid, err)
		return
	}
	defer conn.Close()

	// SSH_AUTH_SOCK 用の local unix socket（0600・stale 除去は SlaveSocket 内）。
	sockPath, err := sshForwardSockPath(afSid)
	if err != nil {
		w.lg.Printf("webterm: SSH転送 sock パス解決失敗 afSid=%q: %v", afSid, err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		w.lg.Printf("webterm: SSH転送 dir 作成失敗 afSid=%q: %v", afSid, err)
		return
	}
	ln, err := agentfwd.SlaveSocket(sockPath)
	if err != nil {
		w.lg.Printf("webterm: SSH転送 socket 作成失敗 afSid=%q: %v", afSid, err)
		return
	}
	defer func() { _ = ln.Close(); _ = os.Remove(sockPath) }()

	w.lg.Printf("webterm: SSH転送 開始 afSid=%q sock=%s", afSid, sockPath)
	_ = agentfwd.Slave(bctx, conn, ln) // owner 切断/ctx 終了で戻る→撤去
	w.lg.Printf("webterm: SSH転送 終了 afSid=%q（socket 撤去）", afSid)
}
