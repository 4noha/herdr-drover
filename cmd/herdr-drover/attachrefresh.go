package main

// attachrefresh — drover の版数が変わった起動で **注入 pane を 1 回だけ作り直す**。
//
// ## なぜ要るか（2026-07-26 実測・v0.5.28 の配信で判明した実害）
//
// 注入 pane（↗窓）の中身は `<selfExe> attach <pc> <sid>` という**別プロセス**で、
// 親は herdr（pane の持ち主）＝**drover daemon が exit/再起動しても入れ替わらない**。
// ローカル配信手順が `pkill -f 'herdr-drover attach'` → `launchctl kickstart -k` を
// 要求してきたのはこれが理由だが、**遠隔 `self-update` / `update-all` はこの pkill を
// しない**（`internal/commands` の実体は selfupdate.Update → DoExit だけ）。
// つまり **attach.go の変更は遠隔更新では他 PC に永久に届かない**。
//
// 実害: v0.5.28 で BUG-3（#inj bridge の thrash）を直して fleet 配信したのに、
// source 側の observe 再 spawn 間隔は ~31s のまま変わらなかった。再 Wake していた
// viewer が他 PC の **旧バイナリのままの attach 子プロセス**だったため。
//
// ## 対処
//
// 「前回この処理を回した時の版数」をローカルに記録し、**版数が変わった起動の 1 回だけ**
// 既存の注入 pane を撤去する。撤去は BUG-2 で入れた `emptyRemoteSource`（desired=∅ →
// 既存注入 pane を全 close）をそのまま使い、再生成は `runRemoteInject` の**起動時 kick**
// が行う＝新しい機構を足さない。どちらも実 herdr テスト済みの既存経路。
//
// ⚠ 版数が同じ起動では何もしない（通常の daemon 再起動で ↗窓 を瞬断させない）。
// ⚠ ldflags で版数を焼かない素の `go build`（version="dev"）では初回しか働かない
// ＝開発中に attach.go を触ったら従来どおり手動の pkill→kickstart が要る。
// `scripts/build.sh` / `make` は `git describe --tags --always --dirty` を焼くので
// commit ごとに別版数になり、この仕組みが効く。

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/4noha/herdr-drover/internal/herdrapi"
	"github.com/4noha/herdr-drover/internal/injectindex"
)

// attachStampFile は「注入 pane を最後に作り直した drover 版数」を記録するファイル名。
// 置き場は config.json / inject-index.json と同じ ~/.herdr-drover。
const attachStampFile = "attach-version"

// attachStampPath は attachStampFile の絶対パス（openInjectIndex と同流儀＝home 直下固定）。
func attachStampPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home ディレクトリ不明: %w", err)
	}
	return filepath.Join(home, ".herdr-drover", attachStampFile), nil
}

// needsAttachRefresh は記録済み版数 prev と現在の版数 cur から「注入 pane を作り直す
// 必要があるか」を決める**純関数**。
//
//   - cur が空（版数不明）→ **false**。判断材料が無いのに毎起動で作り直すと ↗窓 が
//     起動のたび瞬断する。silent に既定へ倒すのではなく「判定しない」を選ぶ。
//   - prev が空（スタンプ未記録＝**この仕組みが入る前のバイナリが作った注入 pane が
//     残っている起動**）→ **true**。配信 1 回目でまさにこれを直したい。
//   - prev != cur → true（更新された）／prev == cur → false（通常の再起動）。
func needsAttachRefresh(prev, cur string) bool {
	return cur != "" && prev != cur
}

// readAttachStamp はスタンプを読む。不在・読取失敗はどちらも "" ＝「記録なし」扱い
// （壊れたスタンプで作り直しが永久に止まるより、1 回余計に作り直す方が安全）。
func readAttachStamp(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// writeAttachStamp はスタンプを書く（dir 不在なら作る）。
// ⚠**撤去に成功してから**呼ぶこと。先に書くと、撤去できなかった起動を取りこぼして
// 旧バイナリの attach がそのまま残り続ける。
func writeAttachStamp(path, ver string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("%s の dir 作成: %w", path, err)
	}
	return os.WriteFile(path, []byte(ver+"\n"), 0o600)
}

// refreshStaleAttachPanes は版数が変わっていたら既存の注入 pane を**撤去する**。
// 再生成はしない（呼び手 runRemoteInject の起動時 kick が desired どおり新バイナリで
// 作り直す＝ここは「古い attach を落とす」だけを担う）。
//
// 撤去が完走しなかった周はスタンプを更新しない＝次回起動で再試行する。
//
// ⚠ 撤去はローカル herdr の操作だけで完結する（emptyRemoteSource はネットワークに
// 触らない）ので、**クラウド不達でも撤去は成功する**。その周は再生成側が abort して
// ↗窓 が一時的に 0 枚になりうるが、起動時 kick に続く backstop poll
// （remoteInjectBackstopPoll）が復帰時に作り直す＝自己修復に乗る。版数変化は更新直後
// にしか起きず、その更新自体が GitHub 到達を要する＝実際に重なる窓は狭い。
func refreshStaleAttachPanes(ctx context.Context, api *herdrapi.Client, cl Cloud,
	selfExe string, idx *injectindex.Index, lg *log.Logger, reported map[string]string,
	stampPath, ver string) {

	prev := readAttachStamp(stampPath)
	if !needsAttachRefresh(prev, ver) {
		return
	}
	lg.Printf("[reconcile] drover 版数が変わった（%q → %q）＝注入 pane を作り直す"+
		"（attach は別プロセスで daemon 再起動では入れ替わらない）", prev, ver)

	rctx, rcancel := context.WithTimeout(ctx, remoteInjectTimeout)
	ok := reconcileRemote(rctx, api, emptyRemoteSource{}, cl, selfExe, idx, lg, reported)
	rcancel()
	if !ok {
		lg.Printf("[reconcile] 注入 pane の撤去が完走しなかった＝スタンプを更新しない（次回起動で再試行）")
		return
	}
	if werr := writeAttachStamp(stampPath, ver); werr != nil {
		lg.Printf("[reconcile] attach 版数スタンプの書込失敗（次回起動でもう一度作り直す）: %v", werr)
		return
	}
	lg.Printf("[reconcile] 注入 pane を撤去した（直後の reconcile が新バイナリで作り直す）")
}
