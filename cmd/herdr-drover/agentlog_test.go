package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotatingLogRotatesOnceAtLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.log")
	rl, note, err := openRotatingLog(path, 200)
	if err != nil {
		t.Fatal(err)
	}
	if note != "" {
		t.Fatalf("新規ファイルで退避が起きた: %q", note)
	}
	line := strings.Repeat("x", 59) + "\n" // 60B
	for i := 0; i < 5; i++ {
		if _, err := rl.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	// 3 行（180B）まで入り、4 行目で 240B > 200B になるので退避。新ファイルは印＋4,5 行目。
	old, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("agent.log.1 が無い: %v", err)
	}
	if strings.Count(string(old), "\n") != 3 {
		t.Fatalf("退避ファイルは 3 行のはず: %q", old)
	}
	cur, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(cur), "ローテート（agent.log が 200 バイトを超えたので agent.log.1 へ退避した）\n") {
		t.Fatalf("新ファイルの先頭に退避の印が無い: %q", cur)
	}
	if strings.Count(string(cur), "x\n") != 2 {
		t.Fatalf("新ファイルは 4,5 行目の 2 行のはず: %q", cur)
	}
}

func TestOpenRotatingLogRotatesOversizedAtStart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.log")
	if err := os.WriteFile(path, []byte(strings.Repeat("y", 300)), 0o600); err != nil {
		t.Fatal(err)
	}
	rl, note, err := openRotatingLog(path, 200)
	if err != nil {
		t.Fatal(err)
	}
	if note == "" {
		t.Fatal("起動時に上限超過なら退避の説明が返るはず")
	}
	if b, _ := os.ReadFile(path + ".1"); len(b) != 300 {
		t.Fatalf("旧ファイルが .1 に退避されていない: %d", len(b))
	}
	if _, err := rl.Write([]byte("after\n")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != "after\n" {
		t.Fatalf("新ファイルに書けていない: %q", b)
	}
}

// 端末（char device）なら従来どおり stderr だけ（nil）。パイプなら stderr＋ファイル。
func TestAgentLogWriterChoosesByStderrKind(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if w, _, err := agentLogWriter(&strings.Builder{}); w != nil || err != nil {
		t.Fatalf("*os.File 以外は nil のはず: %v %v", w, err)
	}
	r, wpipe, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer wpipe.Close()
	w, _, err := agentLogWriter(wpipe)
	if err != nil || w == nil {
		t.Fatalf("パイプなら stderr＋ファイルのはず: %v %v", w, err)
	}
	if _, err := w.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	p, _ := agentLogPath()
	if b, _ := os.ReadFile(p); string(b) != "hello\n" {
		t.Fatalf("ファイルに書けていない: %q", b)
	}
	buf := make([]byte, 16)
	n, _ := r.Read(buf)
	if string(buf[:n]) != "hello\n" {
		t.Fatalf("stderr（パイプ）にも書くはず: %q", buf[:n])
	}
}
