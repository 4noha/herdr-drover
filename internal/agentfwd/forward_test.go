package agentfwd

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"testing"
	"time"
)

// shortSock は macOS の sun_path 104 byte 制限を避ける短い /tmp パスを返す。
func shortSock(name string) string {
	return fmt.Sprintf("/tmp/herdr-af-%d-%s.sock", os.Getpid(), name)
}

func TestSlaveSocket(t *testing.T) {
	path := shortSock("slavesock")
	defer os.Remove(path)

	// 事前に残骸を置く（stale 除去の検証）。
	if err := os.WriteFile(path, []byte("stale"), 0o644); err != nil {
		t.Fatalf("pre-write stale: %v", err)
	}
	ln, err := SlaveSocket(path)
	if err != nil {
		t.Fatalf("SlaveSocket: %v", err)
	}
	defer ln.Close()

	// パーミッションは 0600。
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("socket perm = %o want 0600", perm)
	}

	// 実際に dial→accept できる。
	go func() {
		c, e := ln.Accept()
		if e == nil {
			_, _ = io.Copy(c, c) // echo
			_ = c.Close()
		}
	}()
	cli, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cli.Close()
	_, _ = cli.Write([]byte("ok"))
	_ = cli.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 2)
	if _, err := io.ReadFull(cli, buf); err != nil || string(buf) != "ok" {
		t.Fatalf("echo round-trip: buf=%q err=%v", buf, err)
	}
}

// TestOwnerSlaveRealSockets は Owner/Slave/SlaveSocket を実 unix socket ＋
// net.Pipe（relay 代役）で e2e に通す: client が slave socket へ繋ぐと owner 側の
// 実 ssh-agent 代役 socket へ届いて往復する。
func TestOwnerSlaveRealSockets(t *testing.T) {
	// owner 側 ssh-agent 代役（実 unix socket・echo）。
	agentPath := shortSock("agent")
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
	slavePath := shortSock("slave")
	defer os.Remove(slavePath)
	slaveLn, err := SlaveSocket(slavePath)
	if err != nil {
		t.Fatalf("SlaveSocket: %v", err)
	}
	defer slaveLn.Close()

	// relay パイプ代役。
	a, b := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = Slave(ctx, a, slaveLn) }()
	go func() { _ = Owner(ctx, b, agentPath) }()

	// slave 上の git/ssh 相当: SSH_AUTH_SOCK へ繋いで署名要求 → owner agent が応答。
	cli, err := net.Dial("unix", slavePath)
	if err != nil {
		t.Fatalf("client dial slave sock: %v", err)
	}
	defer cli.Close()
	msg := []byte("SSH2-AGENT-SIGN-REQUEST")
	if _, err := cli.Write(msg); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = cli.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(cli, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != string(msg) {
		t.Fatalf("e2e mismatch: got %q want %q", buf, msg)
	}
}
