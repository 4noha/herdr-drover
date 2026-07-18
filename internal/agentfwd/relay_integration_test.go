package agentfwd_test

// 実 drover-cloud relay.Server（no-auth モード）を httptest で立て、owner
// (viewer) ↔ slave (source) の SSH agent 転送が **実ペアリング**越しに成立する
// ことを GCP 無しで実証する。net.Pipe 代役でなく本物の relay を通す＝転送路の確証。

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/4noha/drover-cloud/relay"
	"github.com/4noha/drover-cloud/relayclient"
	"github.com/4noha/herdr-drover/internal/agentfwd"
)

func sock(name string) string {
	return fmt.Sprintf("/tmp/herdr-afint-%d-%s.sock", os.Getpid(), name)
}

func TestForwardOverRealRelay(t *testing.T) {
	// 実 relay.Server（Grant/SlaveGate 未設定＝no-auth。転送路の検証が目的）。
	rsrv := relay.NewServer()
	ts := httptest.NewServer(http.HandlerFunc(rsrv.ServeHTTP))
	defer ts.Close()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") // http→ws

	const afSid = "repoA-verify|af"

	// owner 側 ssh-agent 代役（実 unix socket・echo）。
	agentPath := sock("agent")
	defer os.Remove(agentPath)
	_ = os.Remove(agentPath)
	agentLn, err := net.Listen("unix", agentPath)
	if err != nil {
		t.Fatalf("agent listen: %v", err)
	}
	defer agentLn.Close()
	go func() {
		for {
			c, e := agentLn.Accept()
			if e != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); _ = c.Close() }()
		}
	}()

	// slave 側 SSH_AUTH_SOCK。
	slavePath := sock("slave")
	defer os.Remove(slavePath)
	slaveLn, err := agentfwd.SlaveSocket(slavePath)
	if err != nil {
		t.Fatalf("SlaveSocket: %v", err)
	}
	defer slaveLn.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// slave: relay へ source として dial → Slave。
	go func() {
		conn, e := relayclient.Dial(ctx, wsURL, afSid, "source")
		if e != nil {
			return
		}
		_ = agentfwd.Slave(ctx, conn, slaveLn)
	}()
	// owner: relay へ viewer として dial → Owner。
	go func() {
		conn, e := relayclient.Dial(ctx, wsURL, afSid, "viewer")
		if e != nil {
			return
		}
		_ = agentfwd.Owner(ctx, conn, agentPath)
	}()

	// slave 上の git/ssh 相当: SSH_AUTH_SOCK へ繋いで署名要求 → 実 relay 越しに
	// owner agent が応答。relay の writePeer は相手到着を待つので多少の接続順は
	// 吸収されるが、念のため往復成立まで数回リトライ。
	msg := []byte("SSH2-SIGN-OVER-RELAY")
	var lastErr error
	for attempt := 0; attempt < 30; attempt++ {
		cli, e := net.Dial("unix", slavePath)
		if e != nil {
			lastErr = e
			time.Sleep(100 * time.Millisecond)
			continue
		}
		_ = cli.SetDeadline(time.Now().Add(2 * time.Second))
		if _, e = cli.Write(msg); e != nil {
			_ = cli.Close()
			lastErr = e
			time.Sleep(100 * time.Millisecond)
			continue
		}
		buf := make([]byte, len(msg))
		if _, e = io.ReadFull(cli, buf); e != nil {
			_ = cli.Close()
			lastErr = e
			time.Sleep(100 * time.Millisecond)
			continue
		}
		_ = cli.Close()
		if string(buf) != string(msg) {
			t.Fatalf("relay 越し往復 mismatch: got %q want %q", buf, msg)
		}
		return // 成功
	}
	t.Fatalf("relay 越し往復が成立しなかった: lastErr=%v", lastErr)
}
