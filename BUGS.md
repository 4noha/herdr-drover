# 既知バグ / 引き継ぎ（2026-07-26 記録）

> ✅ **BUG-1 / BUG-2 / BUG-3 は 2026-07-26 に修正済み**（実 herdr テストで担保・
> gofmt/build/vet/test 緑）。各節冒頭に「解決」欄を追記した。BUG-3 は本書が推定して
> いた真因（source 側 tick / mirror producer が再張り）が**誤り**で、実経路は
> **viewer 側 attach の再 Wake** だった（下記 BUG-3 参照）。

本書は**別セッションが単独で修正着手できる**よう、症状・再現・真因・該当コード
（file:line）・修正方針・実測エビデンスを確定して記す。開発の鉄則は
[CLAUDE.md](CLAUDE.md) / [DESIGN.md](DESIGN.md) 末尾（実テスト担保・旧コード FAIL
確認・exact-match・silent 変更禁止）。契約は [SPEC.md](SPEC.md)、進行中は
[TODO.md](TODO.md)。

発見経緯: 2026-07-26、herdr clean 再起動後にローカル claude/cursor/codex の全 pane
が「起動しているのに空（ゾンビ）」になった調査中に確定。対象実測版: herdr-drover
**v0.5.27** / herdr **0.7.4** / drover daemon は launchd 常駐（`KeepAlive=true`）。

---

## BUG-1【P0】shim 再入で pane がゾンビ化（本物の agent が起動しない）

### ✅ 解決（2026-07-26）
`findAgentPaneByResumeRef`（`cmd/herdr-drover/claudeshim.go`）に **(a) 自 pane 除外
（`HERDR_PANE_ID` env と一致する pane を候補から外す）** と **(b) live 検証
（`agent_status=="unknown"`／空 の pane は attach 先にしない）** を追加。backstop の
契約「会話を**実行中の live pane**を返す」を実装で担保した。回帰テスト
`TestResumeBackstopSkipsZombieAndSelfPane`（`claudeshim_resume_test.go`・**旧コードで
zombie/self を返す FAIL を確認済み**）。既存の live pane を模すテスト 3 本は「状態→
session の順」で実 live 化するよう修正（herdr 0.7.4 の report 契約）。

### 症状
herdr 再起動でレイアウト復元すると、各 pane で
`herdr-drover claude/shim <agent> --resume <id>` が起動するが、**本物の
claude/codex/cursor が 1 つも立ち上がらず空 pane になる**。process-info では:

```
zsh (herdr server 配下)
 └─ herdr-drover claude --resume <uuid>      ← shim（S+ で常駐）
      └─ herdr terminal session observe <自分自身の pane>   ← 自己 observe
```

`herdr pane list` の該当 pane は `agent_status: "unknown"`、システム全体で実
claude プロセス 0。ユーザーには「ゾンビ pane」に見える。

### 再現
1. herdr で claude セッションを 1 本起動（pane に agent_session.value=uuid が付く）。
2. `herdr server stop` → herdr 再起動（session.json のレイアウト復元で shim が
   `--resume <uuid>` として各 pane で再起動）。
3. → 該当 pane がゾンビ化（本物の claude が起動しない）。

### 真因（確定）
`cmd/herdr-drover/claudeshim.go` の **resume backstop**（おおむね L172-186）:

```go
if ref := parseResumeRef(agent, args); ref != "" {
    if p, ferr := findAgentPaneByResumeRef(api, agent, ref); ferr == nil && p != nil {
        // 「その会話を実行中の既存セッション」とみなし attach（=observe）
        return attachOrReport(api, herdrapi.AgentInfo{PaneID: p.PaneID, ...}, stdout)
    }
    // 一致なし → 下で本物 claude を --resume 起動
}
```

意図は「同 uuid の**生きている** pane があれば dup を作らず attach」。ところが
**herdr 再起動時、復元された pane は古い `agent_session.value=uuid` メタデータを
保持したまま（だが live claude は無い）**。その pane で shim 自身が走るため、
`findAgentPaneByResumeRef` が **shim 自身の pane** にヒット →
自分自身を `observe` → 循環デッドロックで空 pane。`attachOrReport`
(`claudeshim.go:742`) が observe を張る。

要は **`findAgentPaneByResumeRef` が「uuid メタデータ一致」だけで判定し、その pane
に実 agent が生きているか（`agent_status` / 前景プロセス）を見ていない**、かつ
**自分自身の pane を除外していない**。

### 修正方針（候補）
- `findAgentPaneByResumeRef` で **(a) 自 pane（`HERDR_PANE_ID` env と一致）を除外**、
  かつ **(b) 候補 pane が実 agent 稼働中か検証**（`agent_status != unknown` か、
  pane process-info に実 agent 前景プロセスがあるか）。どちらも満たさなければ
  「一致なし」扱いにして本物 `--resume` 起動へ落とす。
- 併せて、`claude`/`codex`/`cursor-agent` が drover shim への alias/symlink である
  ため、shim 内の新規起動は既に `lookupAgentBin`（`exec.LookPath`）で実バイナリを
  解決している（`claudeshim.go` L192 付近）＝この経路自体は正しい。問題は
  **attach 判定が誤って新規起動を抑止する**点のみ。

### エビデンス / 暫定復旧手順（このセッションで実施）
壊れた shim の PG を kill → 実バイナリをフルパスで `--resume`（alias 迂回）で復旧:
```
kill -TERM -<foreground_process_group_id>
herdr pane run <pane> "cd <cwd> && /Users/4noha/.local/bin/claude --resume <uuid>"
```
実バイナリ: claude=`~/.local/bin/claude`(→`~/.local/share/claude/versions/<ver>`) /
cursor=`~/.local/share/cursor-agent/versions/<ver>/cursor-agent --resume <chatId>` /
codex=`/opt/homebrew/bin/codex resume <session-uuid>`。
transcript が無い会話は復元不可（本件と別の永続化事情。下 BUG-2 参照）。

---

## BUG-2【P1】リモート pane 注入を止める設定が無い / `mirror_agents` が誤解を招く

### ✅ 解決（2026-07-26）
注入専用の opt-out フラグ **`DROVER_INJECT_REMOTE` / `inject_remote_panes`（既定
true）** を新設（`cmd/herdr-drover/config.go` の `Config.InjectRemotePanes`・env >
file > 既定）。false のとき `runRemoteInject`（`reconcile.go`）は st の代わりに
**`emptyRemoteSource{}`** を渡す＝reconcile の `desired` が常に空になり、既存の close
ロジックで**注入 pane が全撤去され、新規注入は作られない**。producer（自 PC の
セッションを push＝Web/スマホ閲覧）は別経路なので止めない。運用: `DROVER_INJECT_REMOTE`
を off にして daemon を kickstart で反映（`launchctl bootout` 不要になった）。
テスト `TestResolveConfigInjectRemotePanes`（config）/ `TestReconcileTeardownWithEmptyRemoteSource`
（実 herdr で「リモート健在でも全撤去・再注入なし」を確認）。`mirror_agents` は
従来どおり metadata 転記の gate（意味を変えず、注入 gate はこの新フラグに分離）。

### 症状
別 PC の agent pane を relay 経由で大量にローカル herdr へ `↗` 注入する挙動を
**設定で止められない**。`config.json` の `mirror_agents:false` にしても注入は
継続（daemon 再起動しても `[reconcile] desired=N` を維持）。daemon は launchd
`KeepAlive=true` のため `kill` しても復活し、注入 pane を `herdr pane close` しても
reconcile が別 pane id で貼り直す。実質、daemon を `launchctl bootout` する以外に
止める手段が無い。

### 真因
- `mirror_agents`（`cmd/herdr-drover/config.go:33-135`、env `DROVER_MIRROR_AGENTS`）は
  コメント上「リモート session の **agent_status/window_name を注入**」＝
  **メタデータのミラーであって、pane 注入そのものの gate ではない**
  （`internal/herdrapi/types.go:108` / `internal/agentid/agentid.go:27,48` の記述と整合）。
- `↗` pane 注入の desired は `inject-index.json`（`internal/injectindex/index.go`・
  永続ファイル）＋ reconcile（`cmd/herdr-drover/reconcile.go`、`[reconcile] desired=`）
  で駆動され、`mirror_agents` とは独立。

### 修正方針（候補）
- **pane 注入自体を無効化する設定**を新設（例: `inject_remote_panes:false` /
  `DROVER_INJECT_REMOTE=off`）し、reconcile の desired 計算で gate する。
- または `mirror_agents` の名称/ドキュメントを「agent メタデータのミラー」と明確化し、
  注入 on/off は別フラグに分離。
- `herdr-drover` CLI に「注入を今すぐ全撤去」サブコマンド（inject-index を空にして
  reconcile）を追加すると運用が楽（現状は daemon 停止しかない）。

---

## BUG-3【P2】外向き #inj bridge の thrash（30s quiescence→即再オープンの無限ループ）

### ✅ 解決（2026-07-26）＋真因の訂正
**真因は本書の推定と違った。** source 側に周期的に bridge を張り直すループは
**存在しない**（`st.Wake` の呼び出しは viewer 側 2 箇所＝`attach.go:441` /
`sshforward.go:172` のみ・producer の `Tick` は session-list 同期で bridge 確立では
ない）。実経路は **viewer 側 `cmdAttach` の backoff 再接続**: 注入 attach pane は
idle で source が quiescence 自切断すると `pumpFrames` が idle 超過で戻り、旧
`cmdAttach` は「サイクルが >5s 続いた＝健全接続」とみなして backoff を 500ms へ
**リセットして即再接続→再 `st.Wake`** → source（`webterm.handleWake`）が bridge を
再張り＝observe 再 spawn。これが 30s 周期で回り 59k spawn になっていた。

修正（`cmd/herdr-drover/attach.go`）: `pumpFrames` に **`idleClosed`（idle 超過の
read deadline で戻った印）** を返させ、`attachOnce`→`attachCycle`→`cmdAttach` へ伝播。
純関数 `attachBackoffReset` が **idle 切断では backoff をリセットしない**（伸ばして
再接続を疎にする＝quiescence の「無通信なら畳む」意図を活かす）。実データ接続が
>5s 続いてから不意に切れた場合だけ素早い復帰のためリセット。テスト
`TestPumpFramesReportsIdleClose` / `TestAttachBackoffReset`（純関数・既存 pumpFrames
テストも後方互換で緑）。

### 症状
daemon 稼働中、ローカル各 pane に対する外向き bridge `sid="wX:pY#inj"` が
**30 秒の無通信 quiescence で自切断 → 即再オープン**を延々繰り返す。`observe spawn`
が累計 **59,329 回**、`~/.herdr-drover/agent.log` が **16.8MB** に膨張。pane が
busy に見える一因。

### 真因
- `internal/bridge/bridge.go`:
  - `DefaultIdle=30s`（L58-59）で無通信自切断（L222 `quiescence: %s 無通信＝データ線を自切断`）。
  - L252 コメントは「**quiescence 自切断後は respawn しない**」＝bridge.go 内では
    再張りしない設計。
- にも関わらず観測では即再オープン → **再オープンは上位の周期ループ側**（daemon の
  tick `DROVER_TICK=5s` による bridge 確立、mirror producer 系）。idle-close した
  bridge を**活動が無いのに毎周期張り直している**。

### 修正方針（候補）
- 上位ループで **idle-closed の bridge を記録し、対象 pane に実際の出力活動が出るまで
  再オープンしない**（activity-gated）／もしくは指数バックオフ。
- quiescence の意図（無通信なら畳む）を活かすなら、再オープン条件を「pane の
  new-output イベント受信時のみ」に変更するのが筋。

---

## 補足: このセッションの環境復旧結果（参考）
- drover daemon を `launchctl bootout gui/$(id -u)/com.4noha.herdr-drover` で停止
  （KeepAlive 無効化＝復活しない）。再開は
  `launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.4noha.herdr-drover.plist`。
- claude 3 本（audio-router / zvw30-hack / obsidian-vault）は transcript 実在→
  実 claude 直接 `--resume` で会話ごと復元。claude-2(herdr-drover) は transcript
  喪失で復元不可。
