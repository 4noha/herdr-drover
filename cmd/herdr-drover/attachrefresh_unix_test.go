//go:build unix

package main

// 実 herdr を起こす検証（harness が /tmp 前提＝macOS の sun_path 104B 制約のため
// unix 限定。reconcile_test.go と同じ理由・同じタグ）。純関数側は untagged の
// attachrefresh_test.go にある。

import (
	"context"
	"io"
	"log"
	"path/filepath"
	"testing"
	"time"

	"github.com/4noha/herdr-drover/internal/herdrapi"
)

// TestRefreshStaleAttachPanesOnVersionChange は本命の実 herdr テスト。
//
// 注入 pane の中身（`<selfExe> attach ...`）は herdr が親の**別プロセス**で daemon の
// 再起動では入れ替わらない＝attach.go の変更を反映するには pane を作り直すしかない。
// その「作り直し」の起点である撤去が、
//
//	(1) 版数が変わった起動では**実際に既存の注入 pane を全撤去**し、スタンプを更新する
//	(2) 版数が同じ起動では**何もしない**（通常の daemon 再起動で ↗窓 を瞬断させない）
//
// の両方を満たすことを実 herdr で確認する。(2) が無いと「毎起動で全 ↗窓 が作り直され
// る」という別の実害になるので、両方向を必ず見る（実際、判定を常時 true / 常時 false に
// 壊すとそれぞれ (2) / (1) が FAIL することを確認済み）。
func TestRefreshStaleAttachPanesOnVersionChange(t *testing.T) {
	sock := startHerdrForTest(t)
	api := herdrapi.New(sock)
	lg := log.New(io.Discard, "", 0)
	stub := reconcileStub(t)
	ctx := context.Background()
	idx := newTestIndex(t) // 生成と撤去で同一 index を持ち回る（daemon 相当）
	stamp := filepath.Join(t.TempDir(), "attach-version")

	fr := &fakeRemote{
		pcs: []string{"self-herdr", "remoteA"},
		sessions: map[string][]map[string]any{
			"remoteA": {fakeSess("w9:pA", "projA"), fakeSess("w9:pB", "projB")},
		},
	}
	const selfPC = "self-herdr"
	cl := Cloud{PCName: selfPC}

	create := func(what string) {
		reconcileRemote(ctx, api, fr, cl, stub, idx, lg)
		waitCond(t, 15*time.Second, what, func() bool {
			inj := injectedPanes(t, api)
			return len(inj) == 2 && hasInj(inj, "remoteA", "w9:pA") && hasInj(inj, "remoteA", "w9:pB")
		})
	}

	// 前提: 注入 pane が 2 枚ある（旧バイナリが作ったもの、という想定）。
	create("注入 pane 2 枚出現")

	// (1) スタンプ未記録（＝この仕組みが入る前のバイナリ製）→ 撤去される。
	refreshStaleAttachPanes(ctx, api, cl, stub, idx, lg, nil, stamp, "v-new")
	waitCond(t, 15*time.Second, "版数変化で注入 pane が全撤去", func() bool {
		return len(injectedPanes(t, api)) == 0
	})
	if got := readAttachStamp(stamp); got != "v-new" {
		t.Fatalf("撤去後にスタンプが更新されていない: %q", got)
	}

	// 撤去だけが仕事＝再生成は呼び手（runRemoteInject の起動時 kick）が行う。
	// ここではそれを手で回して 2 枚を復元する。
	create("撤去後の reconcile で 2 枚が復活")

	// (2) 同一版数の起動では**触らない**（通常の daemon 再起動で ↗窓 を瞬断させない）。
	refreshStaleAttachPanes(ctx, api, cl, stub, idx, lg, nil, stamp, "v-new")
	time.Sleep(700 * time.Millisecond) // 撤去が走るなら十分に間に合う時間
	if inj := injectedPanes(t, api); len(inj) != 2 {
		t.Fatalf("同一版数なのに注入 pane が変化した（%d 枚）＝毎起動で ↗窓 が瞬断する", len(inj))
	}

	// (3) さらに版数が変われば再び撤去される（1 回きりの特別扱いになっていないこと）。
	refreshStaleAttachPanes(ctx, api, cl, stub, idx, lg, nil, stamp, "v-newer")
	waitCond(t, 15*time.Second, "次の版数変化でも撤去される", func() bool {
		return len(injectedPanes(t, api)) == 0
	})
	if got := readAttachStamp(stamp); got != "v-newer" {
		t.Fatalf("2 度目の撤去後にスタンプが更新されていない: %q", got)
	}
}
