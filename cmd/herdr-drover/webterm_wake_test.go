package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeLiveBridge は wakeShouldYield の seam（liveBridge）の fake。
type fakeLiveBridge struct {
	received bool
	last     time.Time
	pingErr  error
	pings    int
}

func (f *fakeLiveBridge) ReceivedAny() bool          { return f.received }
func (f *fakeLiveBridge) LastTraffic() time.Time     { return f.last }
func (f *fakeLiveBridge) Ping(context.Context) error { f.pings++; return f.pingErr }

// TestWakeShouldYield — 「稼働中 bridge を新しい wake に譲るか」の判定表。
//   - 空振り（1 バイトも来ていない）→ 譲る・ping しない
//   - 直近に通信あり → 無視・ping しない（見ている画面を切らない）
//   - 無通信だが pong あり → 無視（生きているが止まっている画面）
//   - 無通信で pong なし → 譲る（黙って死んだ接続。15 分の idle を待たせない）
func TestWakeShouldYield(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name      string
		b         *fakeLiveBridge
		wantYield bool
		wantPings int
	}{
		{"空振り", &fakeLiveBridge{received: false, last: now.Add(-time.Hour)}, true, 0},
		{"直近に通信あり", &fakeLiveBridge{received: true, last: now.Add(-2 * time.Second), pingErr: errors.New("呼ばれてはいけない")}, false, 0},
		{"無通信だが pong あり", &fakeLiveBridge{received: true, last: now.Add(-wakeAliveRecent - time.Second)}, false, 1},
		{"無通信で pong なし", &fakeLiveBridge{received: true, last: now.Add(-5 * time.Minute), pingErr: context.DeadlineExceeded}, true, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, why := wakeShouldYield(context.Background(), c.b, now)
			if got != c.wantYield {
				t.Fatalf("yield=%v want %v（%s）", got, c.wantYield, why)
			}
			if c.b.pings != c.wantPings {
				t.Fatalf("ping 回数=%d want %d（%s）", c.b.pings, c.wantPings, why)
			}
			if why == "" {
				t.Fatalf("理由が空")
			}
		})
	}
	// 生成途中（b==nil）は触らない・無いものは譲る。
	if y, _ := yieldToNewWake(context.Background(), &bridgeRun{}, now); y {
		t.Fatalf("生成途中の bridge を止めようとした")
	}
	if y, _ := yieldToNewWake(context.Background(), nil, now); !y {
		t.Fatalf("稼働していない sid で譲らない")
	}
}
