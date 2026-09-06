package main

// wake dedup の「在る ≠ 繋がっている」是正（yieldToNewWake）の検証。
//
// 旧実装は「その sid の bridge が在れば wake を捨てる」だった。空振り bridge
// （相手から 1 バイトも来ていない）が 30〜45 秒居座る間、入り直しが全部捨てられ、
// ユーザーには「Web コンソールが 3 回に 1 回しか出ない」に見えた
// （2026-09-06 実測: この列がログに 110 回）。

import (
	"testing"

	"github.com/4noha/herdr-drover/internal/bridge"
)

func TestYieldToNewWake(t *testing.T) {
	// 相手から何も受けていない bridge（＝空振り）。New しただけで Run しない。
	fresh := bridge.New("w1:p1", nil, nil)
	if fresh.ReceivedAny() {
		t.Fatal("New 直後に ReceivedAny=true（空振り判定が常に false になる）")
	}

	cases := []struct {
		name string
		h    *bridgeRun
		want bool
	}{
		{"稼働なし＝そのまま張る", nil, true},
		{"生成途中（b=nil）＝不明なので触らない", &bridgeRun{}, false},
		{"空振り（1 バイトも来ていない）＝譲る", &bridgeRun{b: fresh}, true},
	}
	for _, c := range cases {
		if got := yieldToNewWake(c.h); got != c.want {
			t.Errorf("%s: yieldToNewWake=%v want %v", c.name, got, c.want)
		}
	}
}
