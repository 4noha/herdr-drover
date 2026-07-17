//go:build !windows

package e2e

import (
	"bytes"
	"sync"
)

// syncBuf は並行書込（exec の pipe pump goroutine）と並行読出（テスト本体の
// ログ検査）を安全にするパッケージ共有ヘルパ（bytes.Buffer は非スレッド
// セーフ）。稼働中の子プロセスの stderr/stdout を String() で読むテストは
// 必ずこれを使う: revoke_e2e_test.go は agent 稼働中に dormant ログを検査
// するため素の bytes.Buffer だと go test -race が実 FAIL する（検出済み）。
// Wait() 完了後にのみ読む場合も、失敗経路（timeout Fatalf の引数評価等）で
// 稼働中読みに化けるので syncBuf に統一するのが安全。
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
