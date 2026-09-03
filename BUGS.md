# 既知バグ / 引き継ぎ（2026-07-26 記録）

> ⚠ **BUG-4 / BUG-5 は実装済みだが Cloud Run 未デプロイ**（2026-08-16 時点）。
> どちらも Web 側の修正を含み、**relay の image を差し替えるまで利用者には届かない**。
> BUG-5 の drover 側（画像貼付本体）は release＋fleet 配信が要る。
>
> ⚠ **BUG-6 / BUG-7 は「再起動後に ↗窓 と Tab が壊れる」の 2 本柱**（どちらも
> 2026-09-03〜04 に対処）。**プロセス起動時にしか反映されない**ため、既存 pane は
> 旧バイナリを掴んだまま＝**効くのは次の再起動から**。BUG-7 は `claude` シムが実行する
> `~/.local/bin/herdr-drover` の版で決まる（`~/.herdr-drover/bin` だけ更新しても効かない）。
> **本機は両方デプロイ済（`v0.5.36-13-gd9504b2`）／tag は未作成＝他 PC は未配布**。
> なお BUG-7 の修正が**発火するのは復元でシムが走る pane だけ**で、明示 argv
> （実バイナリ）で作られた pane は元々倍増しない（BUG-7 §真因の表）。
>
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

### ✅ 解決（v0.5.30）— ⚠**v0.5.28 の「修正」は実経路に一度も当たっていなかった**

真因は 2 段階で判明した。**推定を消さずに経緯ごと残す**（同じ踏み方を繰り返さないため）。

#### 第 1 段（v0.5.28・機構としては正しいが効かなかった）

本書が当初推定した「source 側に周期的な張り直しループがある」は**誤り**だった
（`st.Wake` の呼び出しは viewer 側 2 箇所だけ・producer の `Tick` は session-list
同期であって bridge 確立ではない）。実経路は **viewer 側 `cmdAttach` の backoff
再接続**: 注入 pane が idle → 無通信 30s で conn が切れる → 旧 `cmdAttach` は
「サイクルが >5s 続いた＝健全接続の切断」とみなして backoff を 500ms へ**リセット
して即再接続→再 `st.Wake`** → source が bridge を再張り＝observe 再 spawn。

そこで `pumpFrames` に `idleClosed` を返させ、純関数 `attachBackoffReset` が
**idle 切断では backoff をリセットしない**ようにした。**この機構自体は正しい。**

#### 第 2 段（v0.5.30・なぜ効かなかったか）

v0.5.28/v0.5.29 の `idleClosed` は「**エラー型**が read deadline 由来か」で判定して
いた（`os.ErrDeadlineExceeded` / `net.Error.Timeout()`）。ところが実 conn は
`websocket.NetConn`（`relayclient.DialViewerFrom`）で、**deadline を内部 context の
cancel で実装している**。実測（`TestNetConnDeadlineIsNotATimeoutError`）:

```
err = failed to get reader: context canceled   型 = *fmt.wrapError
errors.Is(os.ErrDeadlineExceeded)   = false
errors.Is(context.DeadlineExceeded) = false
net.Error かつ Timeout()             = false
```

⇒ **どの判定にも当たらず `idleClosed` は常に false**。thrash は一度も止まっていな
かった。fleet 配信後の実測でも observe 再 spawn は **~31s 周期のまま**（13:04:50 /
13:05:22 / 13:05:53 / 13:06:24 / 13:06:56）で、backoff が伸びていないことが数字に
出ていた。

**修正**（v0.5.30・`cmd/herdr-drover/attach.go`）: 判定を**エラー型から経過時間へ**
変える。`pumpFrames` が「最後に 1 バイト受けた時刻」を持ち、切断時に
`isQuiescentClose(idle, time.Since(last))` で決める。実装差に依存せず、相手側の
quiescence 自切断が先に届く場合も同じ基準で拾える。

#### この件の教訓（鉄則 2 を踏んだ）

v0.5.28 のテストは `errConn` に `os.ErrDeadlineExceeded` を**直接注入**する合成
ストリームだった＝**単体テストは緑なのに本番では一度も当たらない**状態を作った。
v0.5.30 で **実 `websocket.NetConn` 越しの `TestPumpFramesDetectsIdleOnRealWebsocketConn`**
を追加（旧実装で FAIL することを確認済み）。**外部ライブラリの振る舞いは
「ドキュメントどおりのはず」で書かず、実物で確かめてから判定に使うこと。**

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

---

## BUG-4【P1】Web ターミナルが出力の途中で止まったまま復帰しない（2026-08-08 記録）

### 🚧 実装済み・**未デプロイ**（2026-08-08）
方針 (a)+(c) を `drover-cloud/web/static/term.js` に実装。判定は純粋関数
`cmWatchGate`（既存 `cmReconnectGate` と同じ流儀）に切り出し、実 node で真理値表を
固定する `web/termjs_gate_test.go` を追加（**旧 term.js で FAIL・新で PASS を確認**）。
gofmt/build/vet/`go test ./...` 全緑。
⚠ **Cloud Run 未デプロイ＝本番にはまだ効いていない**。反映には relay の image 差し替えが要る
（`deploy/deploy.sh` は初回構築専用＝env 全置換で事故る。SPEC.md のデプロイ節参照）。
⚠ 判定表は単体で担保したが、**実際に切れた線が張り直るかは実 relay 越しの確認が必要**。

### 症状
Web ターミナル（`/term`）を開いて眺めていると、**途中でフッと止まる**。
以後いくら出力が続いても画面が更新されない。ユーザが**何か入力すると復帰**する。

### 真因（実測で確定・推定ではない）
**「出力が再開した」ことをブラウザに伝える経路が存在しない。**
＝**切断の理由が何であれ、そのまま張り直されない**（stickiness が本体）。

1. 切断そのものは仕様どおりでバグではない。閉じているのは **PC 側**:
   `internal/bridge/bridge.go:61` `DefaultIdle = 30 * time.Second` が
   「両方向で 30s 無通信ならデータ線を自分から閉じる」。
   **ブラウザ脚には idle タイマーが無い**（`drover-cloud/web/web.go` `wsViewer` は
   `st.Wake` を撃って `rl.Accept(..., "viewer")` するだけ）。PC 側が閉じると
   ペアが落ち、ブラウザの ws も閉じる。
   - 副次的な契機として Cloud Run の `timeoutSeconds=3600`（実測値）があり、
     **1 時間を超える連続ストリーム**は途中でも切られる。頻度は低い。
   - どちらの契機でも症状は同一。**stickiness は「閉じ方」ではなく
     「再開を伝える手段が無いこと」から来る。**
2. ブラウザ側の再接続契機は**3 つだけ**（`drover-cloud/web/static/term.js`）:
   - `:228` onclose 時に**未送出入力が残っている場合**のみ自走再試行
   - `:261` ユーザ入力
   - `:335` Firestore `onSnapshot`（`pcs/{pc}/sessions/{sid}` の doc 変化）
3. その doc は **content_hash が変わった時しか書かれない**
   （`drover-cloud/state/state.go:95-127` `PushStatus`。差分なしは `continue`）。
4. doc の中身は `key / session_id / cwd / short_dir / window_name / is_active /
   agent_status / agent / local_ips` のみ（`internal/session/producer.go:238-260`）。
   **出力が流れても、どれ一つ変わらない。** 揮発フィールドを載せないのは
   near-$0 を守るための意図的な設計（同 `producer.go:20-23`）。

⇒ エージェントが `working` のまま **30 秒以上出力を止める**（思考中など）と線が
閉じる（これが常用の契機。エージェントの思考停止は日常的に 30s を超える）。
その後**出力が再開しても push が飛ばない**＝ブラウザは張り直さない。
`agent_status` が遷移する（working→done 等）か、ユーザが入力するまで凍ったまま。

### 実測エビデンス（2026-08-08）
`mac-studio-herdr` / `w1:p1B`（claude・連続出力中）を 2 分間 5s 間隔で観測:

```
10:20:07  version=212  updated_at=2026-08-08T01:15:03Z  agent_status=working
   （…24 サンプル全て同一…）
10:22:02  version=212  updated_at=2026-08-08T01:15:03Z  agent_status=working
```

出力が流れ続けた 2 分間、**version も updated_at も一切動かない**
（`updated_at` は観測開始の 5 分前）。＝ push は飛ばない。

### 修正方針（未着手・要判断）
❌ **やってはいけない**: 接続を維持する / keepalive を入れる / idle 切断を止める
（near-$0 の設計目的そのものを壊す）。狙いは常に「**再開したら張り直す**」。

候補:
- **(a) ブラウザ側だけで完結**: `is_active===true` かつ
  `document.visibilityState==="visible"` の間だけ、控えめな間隔で
  `requestConnect("watch")` を試みる。コストは**人が実際に見ている時だけ**発生。
  Firestore 書込は増えない。`cmReconnectGate` の backoff 規律に載せる。
- **(b) 粗い output epoch**: producer が「出力があった」ことを 30s 粒度に丸めた
  フィールドで載せる。push は飛ぶが**書込が最大 1/30s/セッション**に増える
  （near-$0 とのトレードオフ。`producer.go:20-23` の方針に抵触するため要判断）。
- (c) `visibilitychange` での再接続（補助。見続けている場合は救えない）。

**推奨は (a)+(c)**。`/ws` ハンドラ自身が接続時に `st.Wake` を撃つので、ブラウザから
張り直せば PC 側は起きる＝**relay/protocol の変更が要らない**。さらに term.js:212-215
が `onopen` ごとに `resizeFrame` を送り、これが完全 frame の catch-up を兼ねるので
**復帰時の再描画も既存のまま正しい**。(b) は `producer.go:20-23` の明示的な設計方針に
正面から抵触するため、採るならユーザ判断（near-$0 とのトレードオフ）。

⚠ ↗窓 側は v0.5.33 の input-wake（`attach.go`）で同種の穴を塞いだが、
**Web 経路は完全に別コードで、同等の wake が無い**。

⚠ **修正時は実 relay の websocket 越しに検証すること**（BUG-3 の教訓＝合成
ストリームで緑にしても本番経路には一度も当たっていなかった）。

---

## BUG-5【P2】Web の画像ペーストが「送信しました」と出るのに何も起きない（2026-08-15 記録）

### ✅ 解決（2026-08-16）
**回帰ではなく未実装だった。** drover 経由では一度も動いたことがない。

- `internal/bridge/bridge.go` が `IMAGE フレームを破棄：v1 非対応` として捨てて
  いた（`DESIGN.md` にも parse-and-drop が仕様として書かれていた）。
- 一方 `term.js` の `sendImageBlob` は**送信キューに載せただけで**
  「画像を送信（リモートで Ctrl+V 注入）」と表示していた＝**成功表示が嘘**。
  この Web UI は cm 由来で、cm には実装がある（`WebImagePaste`・既定 off）。

対処（2 本立て）:
1. **機能を移植**（cm `ptyproxy/server.go` handleImagePaste の移植）。
   `DROVER_WEB_IMAGE_PASTE`（既定 false=opt-in・file `web_image_paste`）。
   `CMWireParser.KeepImage` で payload を保持 → 一時ファイル(0600/dir 0700・
   乱数名) → OS クリップボード(osascript) → pane へ `Ctrl+V(0x16)`。
   クリップボード投入に失敗したら**注入せずファイルも消す**。TTL 5 分で掃除。
   ⚠ **`role=slave` は強制 false**（共用 PC のクリップボードは同一アカウントの
   他人が読める＝DESIGN_SLAVE の脅威モデル）。無効化は必ずログに出す。
2. **UI が嘘をつかないように**。producer が session doc へ `image_paste: true` を
   載せ（true の時だけ＝`local_ips` と同じ後方互換規律）、`term.js` は
   ・確定 false → **送らずに**「この PC は画像貼付が無効です」
   ・確定 true → 従来の成功表示
   ・未確定（doc 未着/push 無効）→ 送るが「相手の対応状況は未確認」

### 実測エビデンス（2026-08-15）
```
2026/08/15 12:07:00 webterm: bridge sid="w1:p1X":
  IMAGE フレームを破棄 (527330B ext=1)：v1 非対応
```
Android から 527KB の PNG が**サーバまで正しく届いていた**（＝クライアント側の
クリップボード権限の問題ではない）。ログ全期間で画像の試行は 2 件・両方破棄。

### テスト
`internal/bridge/imagepaste_test.go`:
- `TestCMWireKeepImage`（drop/保持の切替・複製であること・分割着信）
- `TestHandleImageDisabled`（既定は捨てる・クリップボードに触らない）
- `TestHandleImageEnabled`（0600・中身一致・ext 一致）
- `TestLandImageClipboardFailureLeavesNothing`（失敗時に残さない）
- `TestHandleImageInjectsCtrlV`（**実 herdr の pane** に 0x16 到達）
- `TestHandleImageWithoutPayloadDoesNotInject`（配線が外れても空を貼らない）

`cmd/herdr-drover/config_test.go` に `TestResolveConfigWebImagePaste`
（既定 off・env>file・不正値エラー・**slave 強制 false**）。
`internal/session/producer_test.go` に `TestBuildSessionsImagePaste`。

---

## BUG-6【P1】↗窓 の client が死んでも reconcile が「健全」と報告し続ける（2026-08-28）

### ✅ 解決（2026-08-28）
`reconcileRemote` に **client 生存確認**（`ensureInjectClient`）を追加した。
pane の前景に `<selfExe> attach <pc> <sid>` が **argv exact-match** で居なければ
「死んでいる」と判定し、前景が既知シェルなら**同じ pane のまま**同じ argv を
打ち込んで復活させる（`REVIVE` をログ）。前景がシェル以外なら**触らず理由を出す**
（ユーザーが自分で動かしている pane に打ち込まないため）。

### 症状
マシン/herdr の再起動後、**↗窓 が全滅する**。pane と Tab は残り、token も title も
正しいのに中身は**素のシェル**で、リモートの画面は二度と出てこない。
reconcile は毎周 `[reconcile] desired=12 cur(injected)=12 pending=0` と
**健全を報告し続ける**ので、ログを見ても異常に気づけない。

⚠ **この状態の pane に打鍵すると、ローカル PC のシェルでコマンドが実行される**
（リモートに届かない）。2026-08-28 の実機調査で、4 PC 分 12 枚すべてがこの状態
だった（`pane process-info` の前景が全部 `-zsh`／`herdr-drover attach` プロセスは
システム上に 1 つも無し。8/24 の再起動以降ずっと）。

### 再現
1. 注入 pane が出ている状態で herdr（またはマシン）を再起動する。
2. レイアウト復元後、`herdr pane process-info --pane <↗pane>` を見る
   → 前景が `-zsh`（`herdr-drover attach` ではない）。
3. `~/.herdr-drover/agent.log` は `desired=N cur(injected)=N pending=0` のまま。

### 真因（確定）
`cur` は **pane.list の token（＋injectindex）だけ**で構築されており、
「pane が在る」ことと「client が生きている」ことを区別していなかった
（`reconcile.go` の cur 構築コメントは *判定の権威は token / index* と明記して
いるが、それは**存在**の権威であって**生存**の権威ではない）。
desired 側に一致する pane が 1 枚見つかれば `else` 分岐（＝既存扱い）に落ち、
title と agent_status を転記するだけで**プロセスを一切見ていなかった**。

**BUG-1 と同型**（metadata 一致だけで live を見ない）。BUG-1 は shim の
`findAgentPaneByResumeRef`、本件は reconcile の cur ——同じ罠を 2 箇所で踏んだ。

### 修正で踏まないようにしたこと
- **「command が exit した pane」はこの状態にならない**。実測すると herdr は
  **pane ごと閉じる**（`TestExploreDeadInjectPane` で確認 → 通常の CREATE 経路が
  拾う）。復元由来の「pane は在るが client が居ない」だけが対象。
- **pane を作り直さない**。close→CREATE だと pane_id・Tab・配置が変わり、
  ユーザーの窓が消える。残ったシェルに打ち込む方式なら継続する。
- **`pane.run` は wire API に無い**（`herdr api schema` で実測。CLI 専用）。
  復活は `pane.send_text` ＋ `pane.send_keys("enter")`＝手作業と同じ経路。
- **thrash 防止**: 生存確認は 1 pane あたり `injLivenessEvery`（既定 60s）に間引く。
  client が即死する環境で毎 tick 打ち込み続けない。ndjson は 1 接続=1 リクエスト
  なので毎周 全 pane に `process_info` を投げないためでもある。

### テスト（`cmd/herdr-drover/reconcilelive_test.go`）
実 herdr（隔離ソケット）で「pane は在るが前景がシェル」を再現する:
- `TestReconcileRevivesDeadInjectClient` — 復活すること＋**pane_id が変わらない**こと。
  **`ensureInjectClient` の呼び出しを外すと FAIL することを確認済み**（旧挙動）。
- `TestReconcileLeavesUnknownForegroundAlone` — 前景がシェル以外なら打ち込まない。
- `TestInjClientAliveExactMatch` — 別 pc/別 sid/別バイナリ/余分な引数は「生存」にしない。
- `TestInjForegroundShell` — 既知シェルのみ復活対象（混在は対象外）。

### 併せて直したもの
`herdr-drover help` の「未実装（後続フェーズ）: attach」は**誤り**（attach は
実装済み・reconcile が注入 pane 内で起動している）。実際の説明に差し替えた。

---

## BUG-7【P1】再起動のたびに Tab が倍増する（復元起動が新しい Tab を作る）（2026-09-04）

### ✅ 解決（2026-09-04・`d9504b2`／⚠**未リリース＝tag なし・他 PC 未配布**）
本機（`mac-studio-herdr`）は両バイナリを `v0.5.36-13-gd9504b2` へデプロイ済
（`~/.local/bin` と `~/.herdr-drover/bin` の両方）。
シムの resume backstop に **復元行**を足した（`cmd/herdr-drover/claudeshim.go`）。
`HERDR_PANE_ID` が指す**自 pane 自身**が `--resume <ref>` の会話の持ち主で、かつ
**live agent が居ない**なら、新しい Tab を作らず**その pane で本体へ exec** する
（`selfPaneOwningResumeRef`）。exec なので pane_id・Tab・配置・前景プロセスが
そのまま本体に置き換わる＝倍増もビューア化も起きない。決定表は SPEC.md §2.3。

### 症状
マシン/herdr を再起動するたび **Tab が倍増する**。1 会話につき pane が 2 枚になり、
片方は `agent_status: "unknown"` の殻（実体は observe ビューア）、もう片方に本物の
agent が居る。ユーザーはこれを「タブが壊れた」と呼び、**毎回 手で双子を突き合わせて
閉じる運用**になっていた（skill `herdr-drover` の「再起動後のタブ修復」手順そのもの。
2026-09-03 の実機修復では 7 組を手作業で畳んだ）。

### 再現
1. claude セッションを何本か起動（各 pane に `agent_session.value` が付く）。
2. マシンまたは herdr を再起動する（レイアウト復元）。
3. `herdr pane list` → 同じ `agent_session.value` を持つ pane が 2 枚ずつ現れる。
   `ps` で旧 pane 側に `herdr terminal session observe <新 pane>` が居る（＝ビューア）。

### 真因（確定）
復元で `herdr-drover <agent> --resume <ref>`（シム）が pane 内で走る。シムの契約は
「引数あり＝**常に新しい Tab**」なので、復元で走ったシムは本体を**別 Tab**に立て、
自分が居る元の pane は `attachOrReport` の observe ビューアに化ける。

#### シムが復元で走る条件（2026-09-04 に実測して確定）

herdr の復元経路は **2 つあり、pane が launch argv を持つかで分かれる**
（回帰テスト `TestHerdrPersistsLaunchArgvNotForegroundProcess` で契約を固定）:

| pane の作られ方 | session.json に残るもの | 再起動で起きること |
|---|---|---|
| 明示 argv 付き（シムの `applyClaudeTab`＝**実バイナリ**絶対パス） | `command`＋cwd＋agent_session | **herdr server が直接 spawn**。実 claude が `--resume` で同 pane に戻る＝**倍増しない** |
| 既定シェル pane（ユーザーが素シェルで `claude` と打った側） | cwd＋**agent_session だけ**（`command` は無い） | launch argv が無いので再実行できず、**herdr が `agent_session` から自前で resume を起こす**。それは**対話シェル経由**なので `alias claude`（＝drover シム）に当たる → シムが新 Tab を作る＝**倍増の入口** |

実測の根拠:
- `layout.export` / `session.json` の `command` は **pane 生成時の launch argv だけ**。
  あとから前景に来た子プロセスは記録されない（＝素シェルで打った
  `claude`＝シムは復元 argv にならない）。既定シェル pane は `cwd` と
  `agent_session` のみが永続化される（実採取 JSON で確認）。
- 本機 2026-09-03 22:22:22 の herdr server 再起動では、4 秒後に 7 本の
  `~/.local/bin/claude --resume <uuid>` が **server の直子**として一斉起動し、
  **倍増しなかった**（`ps -eo pid,ppid,lstart` で確認）＝上表 1 行目の経路。
- 一方 BUG-1（2026-07-26）が記録した process tree は
  `zsh（herdr server 配下）└─ herdr-drover claude --resume <uuid> └─ observe` で、
  シムは**シェルの子**だった。launch argv 経路なら server の直子になる（上記実測）
  ので、これは**シェル経由の resume**＝上表 2 行目。倍増していた pane はこちら。

⚠ **未確認の 1 点**: headless（TUI クライアント未 attach）では復元で pane
プロセスが一切 spawn されないため、上表 2 行目をテスト harness で再現できていない
（隔離 herdr を stop→再起動しても marker は走らず前景は空のまま）。2 行目の断定は
上記 2 つの process tree の差分からの推論であり、**次の実機再起動で確定させる**。

**BUG-1（2026-07-26）はこの同じ復元シナリオの前半だけを直していた**。あのとき
足したのは「自 pane を attach 候補から除外する（＝自己 observe デッドロックで
空 pane になるのを止める）」＋「zombie を live とみなさない」で、その結果
「本物が起動する」までは正しくなった。**残っていたのは「本物を*どこに*立てるか*」**
＝自 pane がその会話の持ち主だと分かっているのに、わざわざ新しい Tab に立てる点。

材料は両方とも既に手元にあった（**推測は不要だった**）:
- `HERDR_PANE_ID` は herdr が各 pane env に入れる権威値（BUG-1 の修正と
  `attach.go` の自己 identity 再表明が既に使っている）。
- `agent_session.value` は `pane.get` が返す会話 ref。

### 修正で踏まないようにしたこと
- **live な自 pane は対象外**。自 pane に実 agent が走っているのに exec すると
  同じ会話 jsonl に 2 本目のプロセスを生む（cm で実害のあった型）。復元 pane の
  `agent_status` は前景プロセス基準で復元されるため `unknown`（BUG-1 の実測）＝
  復元シナリオだけを exact に拾える。
- **ヒューリスティックにしない**。「前景が復元シムだから復元だろう」ではなく
  `HERDR_PANE_ID` × `agent_session.value` の exact-match のみ（鉄則③）。
  ↗窓 注入 pane は `agentid.Resolve` が token で先に弾く。
- **非 TTY×引数ありは従来どおり**素通し（`echo prompt | claude -p …` の透過）。
  人が pane 内で手で `claude --resume <他の会話>` と打つ場合は ref が自 pane と
  一致しないので、既存の「常に新しい Tab」がそのまま効く。
- **silent に経路を変えない**。exec の前に「この pane（<id>）が会話 <ref> の
  持ち主です＝新しい Tab を作らず…」を stdout に出す。

### テスト（`cmd/herdr-drover/claudeshim_restore_test.go`）
実 herdr（隔離ソケット）＋ HOME 隔離＋ stub claude で、復元 pane
（`agent_session` あり・`agent_status=unknown`）の上でシムを走らせる:
- `TestShimRestoreExecsInPlaceWithoutNewTab` — exec seam に
  `[<実バイナリ> --resume <ref>]` が渡る／**Tab 総数が増えない**／pane が残る／
  ビューア（`runViewer`）が呼ばれない。**分岐を無効化すると FAIL することを確認済み**
  （`calls=0＝新しい Tab 経路に落ちた（viewer 化 1 回）` ＋ stdout に
  `claude セッションを新規起動しました`）。
- `TestSelfPaneOwningResumeRefIsExact` — `HERDR_PANE_ID` 無し／ref 不一致／
  種別違い（claude の ref を codex として）／**live な自 pane** はいずれも対象外。

`cmd/herdr-drover/herdr_restorearg_test.go` は前提側（herdr が何を復元するか）の
契約を固定する: `TestHerdrPersistsLaunchArgvNotForegroundProcess`（launch argv は
前景が別物でも記録される／既定シェル pane は `command` を持たない）。herdr 側が
変わればここが落ちて、上表の発生条件を再調査できる。

### 運用上の注意
⚠ この修正は**シムのプロセス起動時にしか反映されない**。既に走っている pane が
掴んでいるのは旧バイナリなので、**次の再起動から**効く。また `claude` シムが実行
するのは `~/.local/bin/herdr-drover` の版であって `~/.herdr-drover/bin` ではない
（デプロイ時は両方を揃えること）。

⚠ **本機の現状（2026-09-04 実測）では発火しない**。2026-09-03 の手作業修復で
作り直された 7 枚はいずれも明示 argv（`~/.local/bin/claude --resume <uuid>`）を
持つため、次の再起動でも herdr server が実 claude を直接 spawn する＝倍増しない
代わりに新分岐も通らない。発火するのは**素シェルで `claude` と打って作った
pane**（`command` 無し × `agent_session` あり）を含む再起動と、そういう pane が
残っている他 PC。次の再起動時に `ps -eo pid,ppid,command` で
`herdr-drover claude --resume` の親が zsh かどうかを見れば、上表 2 行目の推論も
同時に確定する。

---

## 補足【誤診断の記録】「daemon 再起動で ↗ pane が作り直される」はバグではない（2026-08-28）

**報告した本人が同日に否定した。誤診断の経緯ごと残す**（同じログでまた誤読しないため）。

当初「daemon 再起動で reconcile が `cur(injected)=0` になり注入 workspace ごと新 id で
再生成される。injectindex の救済が効いていない疑い」と報告したが、**agent.log に理由が
明記されていた**:

```
12:01:17 [reconcile] drover 版数が変わった（"v0.5.36" → "v0.5.36-2-gd205480"）
                     ＝注入 pane を作り直す（attach は別プロセスで daemon 再起動では入れ替わらない）
12:01:17 [reconcile] desired=0 cur(injected)=11 → CLOSE ×11 → 注入 pane を撤去した
12:01:18 [reconcile] desired=11 cur(injected)=0 → CREATE ×11
```

`cmd/herdr-drover/attachrefresh.go` の**意図的な設計**（版数が変わった起動の 1 回だけ）。
版数が同じ通常の daemon 再起動では何もしない。**今回作り直されたのは新版をデプロイしたから。**

### ここから出た本物の欠陥（✅ 修正済み・2026-08-28）

撤去ループ（`reconcile.go`）が desired に無い pane を close する際、**常に
`（リモート消滅）`** と出していた。desired が空になる経路は 2 つあり意味が正反対:

- (a) 実際にリモート session が消えた（通常の `st`）
- (b) **撤去指示**＝`emptyRemoteSource`（`DROVER_INJECT_REMOTE=off` / 版数更新後の
  attachrefresh）。このとき**リモートは健在**

旧実装は (b) でも「リモート消滅」と出すため、**正常な作り直しを「↗窓 が壊れた」と
誤診断させた**（実際に踏んだ）。sentinel 型の exact-match で理由を出し分けるよう修正:
`CLOSE %s（撤去指示＝desired 空（リモートは健在））`。
回帰テスト `TestCloseReasonDistinguishesTeardownFromRemoteLoss`
（`reconcilelive_test.go`・**旧コードで FAIL することを確認済み**）。

### 教訓
**嘘の理由を出すログは、読む人に誤った真因を掴ませる。** BUG-6 の
`desired=N cur(injected)=N`（壊れていても健全時と同じ文字列）と同じ性質。
**「健全」「消滅」と主張するログは、何を根拠にそう言っているかを実装で確かめる。**
