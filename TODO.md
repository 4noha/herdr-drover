# 実装手順メモ / 引き継ぎ（2026-07-26 更新）

**開発の鉄則は [CLAUDE.md](CLAUDE.md) / DESIGN.md 末尾**（実テスト担保・旧コード
FAIL 確認・exact-match・AGPL 衛生・silent 変更禁止・対外操作はユーザー確認後）。
**再開時はまずこの TODO.md を通読**（in-flight・残課題・デプロイ手順の正）。
**既知バグ 3 件は [BUGS.md](BUGS.md) で修正済み**（2026-07-26）:
BUG-1 shim 再入ゾンビ化【P0・claudeshim.go `findAgentPaneByResumeRef` に自 pane 除外＋
live 検証・**v0.5.28 で fleet 配信済**】／ BUG-2 リモート注入 off 設定を新設
【`DROVER_INJECT_REMOTE`・reconcile.go `emptyRemoteSource`・**v0.5.28 配信済**】／
**BUG-3 `#inj` bridge thrash【v0.5.28 の修正は実経路に一度も当たっておらず、v0.5.30 で
再修正】**。

🔴 **BUG-3 の教訓（鉄則 2 を踏んだ）**: v0.5.28 は `idleClosed` を**エラー型**
（`os.ErrDeadlineExceeded` / `net.Error.Timeout()`）で判定していたが、実 conn の
`websocket.NetConn` は **deadline を内部 context cancel で実装**しており返るのは
`failed to get reader: context canceled`＝**どの判定にも当たらない**。単体テストは
合成エラーを直接注入していたので緑だったが、本番では一度も効いていなかった
（fleet 配信後も observe 再 spawn は ~31s 周期のまま＝backoff が伸びない数字が出ていた）。
v0.5.30 で**判定を経過時間ベースへ**変更し、**実 websocket 越しの回帰テスト**を追加。
**外部ライブラリの振る舞いは実物で確かめてから判定に使うこと。**

---

> ⚠ **この TODO の本文は v0.5.24 時点で止まっている**（2026-07-26 時点の実状: main =
> **v0.5.28** / drover-cloud **v0.1.14**・いずれも push・release 済み・working tree clean。
> ⚠go.mod は drover-cloud **v0.1.13** 固定のままで正しい＝v0.1.14 の差分は
> `web/static/devices.js` のみ＝drover バイナリに無関係）。
> v0.5.25〜v0.5.28 の内容は **各 tag のメッセージが実質の正**（`git tag -l -n30 v0.5.28`）。
> 下記「0. Windows 移植」の *未 push* / *branch `windows-port`* / *Windows は self-update
> できない* の記述は**すべて解消済み**（PR #1 merge・`e9bab96` で release に windows 資産。
> Windows 機の self-update 成功を 7/25 15:52 の Ack で実確認）。

### 2026-07-26: v0.5.29 の fleet 配信記録（＝新機構の実地検証）

| PC | 版数 | 経路 |
|---|---|---|
| `mac-studio-herdr` | **v0.5.29** | 手動 rm→cp＋codesign＋kickstart（**pkill attach は意図的にしない**＝新機構の検証） |
| `d24wt27c3j-herdr` | **v0.5.29** | 遠隔 `self-update` |
| `lph77xyyc7-herdr` | **v0.5.29** | 同上 |
| `n9htqcr6g0-herdr` | v0.5.28→適用中 | 同上 |
| `desktop-djb9pfr-herdr`（Windows） | v0.5.26 | オフライン＝pending 2 件 |

✅ **`attachrefresh` が実地で動いた**（`pkill` 一切なしで attach 子プロセスが入れ替わった）:

```
[reconcile] drover 版数が変わった（"" → "v0.5.29"）＝注入 pane を作り直す
[reconcile] desired=0 cur(injected)=11 → CLOSE ×11
[reconcile] 注入 pane を撤去した（直後の reconcile が新バイナリで作り直す）
[reconcile] desired=11 cur(injected)=0 → CREATE ×11
```
旧 attach PID 11 本すべて消滅 → 新 PID 11 本。`~/.herdr-drover/attach-version` = `v0.5.29`（600）。

- ⚠ **撤去パスでは必ず `[reconcile] CAP: 注入 pane=N > 8 → 作成停止し整理のみ` が出る**
  （`maxPanes = len(desired)*3 + 8` に `desired=0` を入れた結果）。**撤去だけが目的の
  パスなので実害はない**（直後のパスで全数作り直されている）。ログを読む人が
  「作成が止まった」と誤解しないよう注記しておく。
- 🔴 **この配信で BUG-3 が直っていないことが判明した**（下記・v0.5.30 で再修正）。
  配信後も observe 再 spawn は **~31s 周期のまま**（13:04:50 / 13:05:22 / 13:05:53 /
  13:06:24 / 13:06:56）＝backoff が伸びていない。**source 側の quiescence ログは 0 件**
  だったので閉じているのは viewer 自身の read deadline と分かり、そこから
  「エラー型判定が当たっていない」に辿り着いた。

### 2026-07-26: v0.5.28 の fleet 配信記録

| PC | 版数 | 経路 |
|---|---|---|
| `mac-studio-herdr`（owner/本作業機） | **v0.5.28** | 手動 rm→cp＋codesign＋kickstart。CLI(`~/.local/bin`) も同時に v0.5.22→v0.5.28 へ同期 |
| `d24wt27c3j-herdr`（master） | **v0.5.28** | 遠隔 `self-update` → `update-all` |
| `lph77xyyc7-herdr`（slave） | **v0.5.28** | 同上 |
| `n9htqcr6g0-herdr`（slave） | **v0.5.28** | 同上 |
| `desktop-djb9pfr-herdr`（Windows） | v0.5.26 | **オフライン**＝`self-update` を pending で投入済（次回オンライン時に自動適用） |

- **投入順序は `self-update` → `update-all`**（この順にした理由: `update-all` の第 1 段
  ＝エージェント本体更新＋セッション再起動は**まだ旧版の drover で走る**。BUG-1 は
  まさに shim の resume 経路なので、**先に self-update で新コードを載せてから**
  セッションを触る段を走らせた）。
- ⚠**遠隔命令を投げる CLI サブコマンドは無い**（投入経路は Web UI か
  `drover-cloud/state.PushCommand`）。今回は後者を叩く使い捨てツールを scratchpad に
  書いて投入した。恒常運用するなら CLI に口を足すのが筋（残バックログへ）。
- `update-all` は 3 台とも **セッション再起動 0 件**（全 pane が「会話 ref
  （agent_session）が取れない」で skip）。7/25 の再起動でセッションが作り直され、
  **まだ発話していない＝jsonl も agent_session も無い**状態のため。これは既知の
  正常挙動（claude は初回発話まで jsonl を書かない）。
- d24wt27c3j では **codex / cursor も更新対象**として動いた（`update-all` 一般化の実証。
  codex-cli 0.145.0 / cursor 2026.07.23-e383d2b とも既に最新）。

#### 🔴 この配信で判明: **遠隔 self-update は attach.go の変更を配れない**

配信後に mac-studio の source 側 `observe spawn` 間隔を実測したところ、**BUG-3 修正後も
~31s のまま**だった（修正前と同じ）。

```
sid="w1:p15#inj": 11:52:17 11:52:48 … 11:57:19 11:57:50 11:58:20   ← 全機 v0.5.28 後も 31s 周期
```

真因は**測っている側の非対称**: `w1:p15#inj` は **mac-studio が source**（自分の claude
pane を他 PC の ↗窓 へ配信している）で、再 Wake しているのは**他 PC の viewer**。
そして **viewer の実体である `herdr-drover attach` 子プロセスは herdr の pane 内で
走るので、daemon が exit/再起動しても死なない**（＝ローカル配信手順に
`pkill -f 'herdr-drover attach'` が要る理由と同じ）。

**`self-update` / `update-all` のハンドラは attach 子プロセスに一切触らない**
（`internal/commands/commands.go:120` → `selfupdate.Update` → `DoExit`＝launchd 再起動
だけ）。よって **attach.go の変更は遠隔更新では永久に反映されない**。BUG-1（shim）と
BUG-2（daemon/reconcile）は新プロセスなので配信済みだが、**BUG-3 は mac-studio でしか
効いていない**（このマシンだけ手順どおり pkill→kickstart した）。

##### ✅ 解決（v0.5.29・`cmd/herdr-drover/attachrefresh.go`）

検討した案は 2 つあった:

- 案 A: `self-update` / `restart-daemon` の**実行前**に自 PC の attach を終了させる
  （ローカル手順の自動化）。⚠**採らなかった**: これだと「その命令を処理する側＝旧
  バイナリ」に実装が要るので、**入れた版から次の版へ更新する回にしか効かない**
  （今まさに困っている v0.5.28→v0.5.29 の回を救えない）。
- 案 B: 遠隔命令に `restart-inject` を新設。⚠**採らなかった**: allowlist（drover-cloud）
  と Web の両方を触る必要があり、しかも人が明示的に投げないと効かない。

採ったのは **案 C: 起動時に自分で気づく**。daemon 起動時に
`~/.herdr-drover/attach-version`（前回作り直した版数）と自版数を比べ、**変わっていた
起動の 1 回だけ**注入 pane を撤去する。**判定するのが新バイナリ側**なので、
**v0.5.28→v0.5.29 の回から効く**（スタンプ不在＝作り直す側に倒す設計）。

- 撤去は BUG-2 で入れた `emptyRemoteSource`（desired=∅ → 全 close）、再生成は
  `runRemoteInject` の起動時 kick。**新しい機構をひとつも足していない**。
- **同一版数の起動では何もしない**＝通常の daemon 再起動で ↗窓 を瞬断させない
  （これが無いと「毎起動で全 ↗窓 が作り直される」という別の実害になる）。
- 実 herdr テスト `TestRefreshStaleAttachPanesOnVersionChange` が両方向を見る。
  判定を常時 true / 常時 false に壊すとそれぞれ落ちることを確認済み。
- 仕様は **SPEC §6.3**。

## 最新状態（2026-07-25・最新タグ v0.5.23 / drover-cloud v0.1.11）

**マルチエージェント対応 P0〜P7 完了・配信済み**。全て build/vet/test 緑
（実 herdr・実 Firestore エミュレータ）。

| PC | 版数 | 状態 |
|---|---|---|
| `mac-studio-herdr`（owner/本作業機） | **v0.5.23** | 配信済（手動 rm→cp＋kickstart） |
| `d24wt27c3j-herdr`（master） | **v0.5.23** | 配信済（遠隔 `update-all`・5 セッション再起動） |
| `lph77xyyc7-herdr`（slave） | **v0.5.23** | 配信済（遠隔 `update-all`・1 セッション再起動） |
| `n9htqcr6g0-herdr`（slave） | **v0.5.23** | 配信済（遠隔 `update-all`・2 セッション再起動） |
| `desktop-djb9pfr-herdr` | dev | **未配信**（7/25 14:25 以降オフライン）。`update-all` を **pending で投入済**＝次回オンライン時に自動適用（古い pending の破棄処理は無い） |

✅ **slave への遠隔 self-update が実機で確認できた**（TODO の長年の未検証項目）。
`/slave/commands` の long-poll は `commands/{pc}/q` を relay 経由で読む同一経路で、
**`updated_at` が 6 時間古くても命令は即座に届いた**（＝producer の state push が
止まっていても命令経路は生きている＝`updated_at` は死活監視に使えない）。
「remote self-update は slave 非到達＝各機で `herdr-drover update` 要」という
**旧記述は誤り**（v0.5.9 以降は到達する）。

✅ **P2 の window_name 改善が実データで効いた**。slave の `w1:p5` / `w1:p9` は
更新前 `window_name` が pane_id（`w1:p5`）だったが、更新後は `claude` になった
＝herdr UI から直接起動され drover が命名していないセッション。これらは P1 の
穴塞ぎにより **restart/update の対象にもなった**（従来は organize だけが拾っていた）。

Cloud Run `claude-master-relay` = **rev 00049**（drover-cloud v0.1.11・環境変数 7 個
の生存を確認済み）。

**配信後の実機確認**（2026-07-25 20:35〜）:
- v0.5.23 の PC は session doc に `agent` を発行（`claude` / **`codex`**）。
  v0.5.22 の PC は `agent` キー無し＝**後方互換が実データで確認できた**。
- d24wt27c3j に**実際の codex セッションが稼働中**（`w1:pN`）。P2 の window_name
  改善で `pane_id` ではなく `codex` と表示されるようになった。
- 遠隔命令 e2e（存在しない sid で非破壊確認）:
  - 新名 `restart-agent-session` + `agent=claude` → Cloud→agent まで貫通。
  - 旧名 `restart-claude` → `restart-agent-session`/`agent="claude"` へ写像され、
    **その旨が Ack detail に残る**（未更新 PC からの投入が生きている証拠）。
  - 未知の agent（`nosuch`）は **投入時点で拒否**（受け手が広く degrade しない）。

⚠ **relay の 502**（`webterm: relay dial 失敗 … got 502`）は **7/22 から継続する
既存事象**で今回のデプロイ起因ではない（7/22:1 → 7/23:3 → 7/24:166 → 7/25:549 と
増加傾向）。bridge は自動復帰しており reconcile も 12/12 で健全だが、**増加傾向は
別途調査に値する**（Cloud Run のコールドスタート/スケール由来と推定・未確認）。

- ✅ **Tab 単位着地ルール**（`organize`/`--capture`/live 学習・`internal/wsmap`）。
- ✅ **自動 min ローカルビューア**（`localview.go`・observe/control 自動切替）。
- ✅ **mv-tab**（Tab を別 Workspace へ丸ごと引っ越し・`--self`/`--dst-ws-label`・
  Claude Code Skill 同梱）。
- ✅ **リモート pane 注入（↗窓）**: 他 PC のセッションをローカル herdr へ pane
  注入（reconcile・自己修復）。**実クラウド 2 PC 越しの実機 e2e 済み**
  （旧 README/TODO の「未実施」注記は解消）。派生 sid `<pane_id>#inj`。
- ✅ **slave 機能（共用 PC 漏れ防止）**: 1 アカウントを複数人で使う共用 PC で
  owner の私物セッションが漏れないよう、slave は制限クレデンシャル（relay 仲介
  state・SlaveGate・pc 名前空間キー）で動く。↗窓 は slave も注入対象
  （`DialViewerFrom(spc=slavePC)`）。設計 **[DESIGN_SLAVE.md](DESIGN_SLAVE.md)** /
  **[DESIGN_SLAVE_SPEC.md](DESIGN_SLAVE_SPEC.md)**。
- ✅ **↗窓 の owner→remote 入力修復（v0.4.4）**: 注入 pane の `attach` client が
  pane PTY を raw モードにしていなかった既存バグ（canonical で Enter まで
  stdin が返らない）を `enterRaw`/`restoreRaw` で修正。Web は xterm.js が raw
  相当なので効いていた＝↗窓 だけ入力不能だった。
- ✅ **slave への一時 SSH エージェント転送（v0.5.0）**: 下記「進行中」参照。
- ✅ **↗窓 配置の永続化 inject_placement（v0.5.5）**: 注入 pane を (出所PC, session
  label) → workspace label で `~/.herdr-drover/workspaces.json` に記録し、再注入時も同 WS
  へ自動着地。`organize --capture` が exact(claude cwd) と inject_placement を両方保存
  （record-only＝pane は動かさない）。
- ✅ **selfupdate の CLI 同期（v0.5.6/v0.5.7）**: `herdr-drover update` が daemon に加え
  CLI バイナリ（`~/.local/bin/herdr-drover`）も同期・「既に最新」でも同期チェック・SHA 一致で
  無駄書き回避（別 PC が発行）。
- ✅ **↗窓 の agent 状態同期（v0.5.8・opt-in）**: リモートの agent_status を注入 pane に
  `pane.report_agent` で転記し herdr に agent 検出させる（tab/workspace の agent_status・
  `agent list/wait/focus` が ↗窓 に効く）。`DROVER_MIRROR_AGENTS`(env) / `mirror_agents`
  (config.json)・**既定 OFF**。producer 改修ゼロ。実 herdr 隔離テスト＋実機（9/10 の ↗窓 が
  実状態）で確認。herdr trap: seq は送らない／agent_status の done/idle→API idle 写像。
- ✅ **inject workspace のゴミ root pane 掃除（v0.5.8）**: workspace 新規作成時の空 root
  pane(tab label='1') を inject pane 追加後に close（`wsmap.ResolveWorkspaceIDWithRoot`）。
  herdr 再起動は session 復元で空にしない＝作成経路固有。⚠ organize/claudeshim も同 create
  経路で同種ゴミ（稀）＝下記 follow-up。
- ✅ **slave の遠隔コマンド受信（v0.5.9）**: slave daemon の
  `relayState.WatchCommands`(long-poll) / `AckCommand` を実装。owner の Web/CLI からの
  self-update・restart-agent が slave にも届く（drover-cloud v0.1.5 の `/slave/commands`・
  `/slave/command-ack` と対）。master 経路・CommandRunner・relay.go byte tunnel は無改変。
- ✅ **タスク完了 Web Push 通知（v0.5.10/v0.5.11）**: herdr ネイティブ agent_status の
  working→idle/done/blocked 遷移を検知し、Web に登録済ブラウザへ FCM push
  （drover-cloud v0.1.6/v0.1.7 と対）。`producer.PushStatus`/`DeleteSession` は無改変で
  `WithOnSessions` フックが BuildSessions 結果を副作用専用に渡す（`isInjected` 同型の
  後付け注入）。master 限定＝slave 対象外。SA 鍵/push 鍵が無ければ no-op で無影響。
  v0.5.11 で通知タイトル＝`short_dir`（プロジェクト名）／body＝`<PC名> · タスク完了`／
  tag＝`<PC名>:<pane key>` を追加（固定 tag だと通知一覧で上書きされ「どれが終わったか」
  不明だった問題の修正）。
- ✅ **terminal_title にリモート PC 名＋実 cwd（v0.5.12）**: 実運用事故（inject pane の
  cwd/foreground_cwd は herdr が同一 workspace の既存 pane 値を継承＝全 inject pane が
  同じ cwd を示し PR レビュー依頼を誤爆）への対処。producer が session に同期している
  cwd を terminal_title に表示する経路を追加（cwd フィールド自体はローカルに無い実
  パスなので偽装できない）。
- ✅ **connHolder.write 無期限ブロック修正（v0.5.13）**: attach.go の stdin reader は
  複数 reader のキー奪い合いを避けるため 1 goroutine 設計だが、conn.Write が relay の
  viewer 未読状態で無期限ブロックすると以後の入力が全停止（TCP ESTABLISHED のまま
  silent failure）。internal/bridge の source 側は writeTimeout 済だったが viewer 側の
  本パスに対応漏れ＝writeTimeout を追加。
- ✅ **ネットワーク切断/停滞の受動復旧一式（v0.5.14 が最終形＝origin/main の起点）**:
  Wi-Fi 切替後の「gRPC/relay dial が死んだまま接続し続ける」への対処。gRPC keepalive
  は drover-cloud 側の話で単体では直せないため、以下 4 点で受動復旧を担保:
  1. attach conn.Read 無通信 30s タイムアウト（`be4984c`）＝OS TCP タイムアウトを
     待たず backoff へ戻す。
  2. reconcile ctx 1 周 20s + WatchSessions 死活非依存の 2 分周期 backstop kick
     （`9e63259`）＝Firestore gRPC が無期限ブロックしても打ち切って復帰。
  3. reconcileWatchdog（連続 5 回 abort＝10 分停滞で launchctl kickstart で自己
     再起動）＋ dialWithTimeout（websocket.NetConn の ctx は接続全体を縛るので外側
     select で打ち切る形）（`5c8a7ef`＝v0.5.14）。
  4. pane.list との突合で terminal_title を再表明（`04624f5`）＝herdr サーバ再起動で
     token 同様に落ちるので毎周 cur を読んで期待値と食い違ったら再貼付。
- ✅ **terminal_title に Tailscale MagicDNS 名（未タグ・`f539700`）**: 既存 local_ips は
  Tailscale の CGNAT/IPv6 ULA を含むが MagicDNS 名（`host.tailnet.ts.net`）は別途
  `tailscale status --json` から取得。PATH の CLI 優先、無ければ App Store 版
  `Tailscale.app` バンドル内 CLI に fallback。パースは `parseTailscaleDNSName` に切り
  出し実プロセス呼び出し無しで単体テスト可能。
- ✅ **スリープ復帰・NIC 変化の能動検知で attach を即時再接続（未タグ・`1088c2e`）**:
  v0.5.14 の受動復旧は最大 30〜60s（DefaultIdle+backoff）の沈黙経路。イベントを能動
  検知して即再接続する常駐 watcher を attach 側に追加。
  - `watchLifecycle` goroutine（cmdAttach スコープ＝backoff 中も生存）が 3s tick で
    (i) `time.Now().Round(0)` の wall clock jump > 15s（Go monotonic clock は S3/S4
    sleep 中停止＝Round(0) で剥がして wall clock 差判定）と (ii) `nicFingerprint()`
    の diff を監視。検知で `connHolder.forceClose`（現接続を能動 close→既存
    pumpFrames→backoff 経路に乗せる）＋ `wakeCh` で backoff sleep を早期脱出＋
    backoff 状態自体をリセット。
  - `nicFingerprint`: `net.Interfaces()` の IPv4 のみを sort 結合。IPv6 SLAAC privacy
    address (RFC 4941) は誤検知源で除外／Docker/Podman/K8s/veth/Colima bridge は
    container 起動で up/down するため `virtualIfacePrefixes` で除外。
  - 共有 cooldown 30s（DefaultIdle と同長）で 2 段遷移（sleep→NIC associate、Wi-Fi の
    旧→空→新）を 1 回に集約。transient 空（net.InterfaceAddrs 一時失敗）は `lastFP`
    書き換えず＝`a→""→b` の real change を silent に吸収しない（敵対的レビュー指摘）。
  - テスト: fake now/tickCh/fpFn を DI seam から注入して実 sleep なしで検証（7 本、
    -race 緑）。

### 2026-07-25 追加: update-all（Web のワンボタン集約）

Web の「更新(self-update)／claude 更新(update-claude)／再起動(restart-agent)」が
冗長だったので 1 ボタン `update-all` に集約（3 命令は allowlist に残す＝CLI や
トラブルシュートから引き続き投げられる）。CLI は `herdr-drover update-all`。

- **順序は入れ替え不可**: (1) claude 本体更新＋セッション反映 → (2) herdr-drover
  自己更新 → (3) 自身の再起動。⚠**自分の再起動は exit でしか反映できず、exit した
  時点でハンドラが終わる＝それ以降の段は実行されない**。よって再起動は必ず最後。
  自己更新を先にしても走っているプロセスは旧 inode のままで新コードにならない＝
  先にやる利点が無い（順序を単純に保つ方が正しい）。
- **Ack は exit の前**（既存の破壊的命令と同じ規律。後 Ack だと running 滞留）。
- **claude 段が失敗したら自己更新へ進まず restart も返さない**（古い claude のまま
  daemon だけ新しくして再起動すると原因が霞む）。
- **二重起動は loud に拒否**（`updateAllRunning` の CAS）。逐次実行が正しさの前提
  なので黙って直列化しない。
- テスト: 段の順序／claude 段失敗で self へ進まないこと／同時実行拒否（実 herdr）
  ＋遠隔命令の Ack 先行→exit 順（実 Firestore エミュレータ）。

### 2026-07-25 追加: モデル切替（restart-claude --model / update-claude --model）

- ⚠**`--resume` した会話は settings.json の既定モデルを無視し、会話に紐づいたモデルで
  動く**（実測: 既定 opus・sonnet で作った会話を resume → sonnet のまま。
  `--model opus --resume` を渡すと opus-5 に切替わる）。既存会話のモデルを変える手段は
  argv の `--model` **だけ**＝この flag が要る理由。`restartOptions{Model}` で芯に通す。
- claude の model 指定に**短縮形は無い**（実測 2.1.220 の --help は `--model` のみ）＝
  `-m` は扱わない。設定は `~/.claude/settings.json` の `"model"`、値は**エイリアス
  （`opus` 等）が backend 非依存**で安全（サブスク/Bedrock で完全 ID が異なるため）。
- ⚠**herdr の SessionStart フックは `$HERDR_PANE_ID` に会話 uuid を報告する**
  （`~/.claude/hooks/herdr-agent-state.sh`・`seq=time.time_ns()`）。つまり claude pane の
  中から `claude -p` 等を走らせると**その pane の agent_session が上書きされる**
  （2026-07-25 に実際に踏んだ: 検証用の使い捨て会話が本セッション pane に紐づいた）。
  pane 内での claude 実行検証は避けるか、別 pane / 隔離 herdr で行うこと。
- ⚠**`pane.report_agent_session` は強制上書き API ではない**。herdr の
  `set_agent_session_ref_for_session_start`（0.7.4 `src/terminal/state.rs:983`）は
  seq ゲート＋`session_start_source` 許可制（startup/resume/clear/compact/new/fork）＋
  同一 owner の別会話への差し替え拒否、の多段ガードで弾く。**`ok` が返るのに値が
  変わらない**ので、成否は必ず `pane.get` で読み直して確認すること。

### 2026-07-25 追加: claude 本体の更新（update-claude・ワンコマンド）

`claude update` は symlink 差し替えのみ＝走っているセッションに効かず、
restart-claude だけではディスクが古いままなら意味がない。2 段を 1 コマンドに閉じた。

- CLI `herdr-drover update-claude [--force] [--dry-run] [sid]`／遠隔命令
  `update-claude`（Web 端末カードの「claude 更新」）。実装 `updateclaude.go`。
- 対象バイナリは restart-claude と同じ「稼働中 pane の argv[0]」が権威。**食い違う
  複数種類は loud に error**（推測しない）。pane 皆無なら PATH →
  `~/.local/bin/claude`。根拠を必ず出力。
- ⚠**`claude update` は最新でも exit 0**（実測 2.1.219: "Claude Code is up to
  date"）＝更新有無は `--version` の前後比較が権威。
- **更新が無くても再起動する**（「ディスクは最新／セッションは旧版」を直すのが目的）。
  更新失敗時は再起動へ進まない。
- ⚠**上限 15 分**（v0.5.18 で 5 分から引上げ）。claude 本体は ~250MB あり、実測
  2026-07-25 でノート PC(d24wt27c3j) の Wi-Fi が 5 分に収まらず `signal: killed`
  になった（有線の 2 台は 4 分以内）。上限超過は「上限内に終わらず中断」と理由を
  明示して返す（生の "signal: killed" では回線が遅いのか claude が壊れたのか
  判別できなかった）。**更新失敗時はセッションを触らない**ので実害は無し。
- ⚠`self-update`（herdr-drover 自身）と `update-claude`（claude 本体）は**別物**。
  Web も「更新」/「claude 更新」でラベル分離。
- ⚠テストの寿命バグで踏んだ罠: subtest 内の `t.TempDir()` は subtest 終了で消え、
  そこに置いた stub の pane が死ぬ。複数 pane を並存させる検証は stub を**外側
  スコープ**で作ること（曖昧検出テストが偽陰性になっていた）。

### 2026-07-25 追加: claude セッション再起動（restart-claude）

claude 本体を更新しても **exec 済みプロセスは旧 inode のまま**（`~/.local/bin/claude`
は `versions/<ver>` への symlink＝再 exec して初めて新版。実測: 7/18 起動の 3 本が
2.1.214 に貼り付き、当日起動だけ 2.1.219）。pane を**会話ごと作り直す**手段を追加。

- CLI `herdr-drover restart-claude [--force] [--dry-run] [sid]`／遠隔命令
  `restart-claude`（Web の端末カード「claude 再起動」＝PC 一括、セッション行と
  ターミナル画面の「⟳claude」＝1 枚）。sid 空＝その PC のローカル claude pane 全部。
- 実装は `cmd/herdr-drover/restartclaude.go`。設計根拠は DESIGN.md
  「restart-claude の設計」。**PATH 非依存**（argv は `layout.export` の launch_argv
  が権威）・会話は `agent_session` uuid の `--resume` で継続・↗窓 注入 pane は
  token で構造的に除外・working と同居 pane Tab は skip。
- テスト: 純関数テーブル＋**実 herdr 隔離 e2e 4 本**（会話 uuid 継承／位置・label・
  agent 名の保存／working skip と --force／同居 Tab skip／注入 pane 不可触）＋
  実 Firestore エミュレータの遠隔命令 dispatch。
- ⚠herdr trap（実測で判明）: `agent_session` は `(source,agent)=("herdr:claude",
  "claude")` の **exact 許可制**で、非公式 source の report は **error にならず
  黙って捨てられる**（0.7.4 `src/agent_resume.rs:is_official_agent_source`）。
  テストで source を変えると偽陰性になる。
- ⚠`TabInfo.number` は**位置ではない**（実測: w1 は tab 3 枚で number=5/21/23）。
  tab 位置は `tab.list` の並び順が権威＝`number-1` を index に使うと壊れる。
- ⚠**実インシデント（v0.5.15 で発生・v0.5.16 で修正）**: herdr の `agent_session`
  が指す uuid は復元可能な会話を保証しない。初回の実機一括再起動で claude-3
  （obsidian-vault・uuid 48378c2d…）の jsonl が `~/.claude/projects` に存在せず、
  `claude --resume` が即 exit → **単独 pane の Tab がプロセス終了で丸ごと自動
  close** され pane が消えた。v0.5.16 で「差し替え後 4s 生存確認→落ちたら resume
  無しで作り直す」二段構えを追加（回帰テスト
  `TestRestartClaudeFallsBackWhenResumeDies`＝旧コードで `pane_not_found` FAIL
  を確認済み）。**単独 pane Tab は中のプロセスが死ぬと Tab ごと消える**は
  restart 以外にも効く herdr の一般則。
  - **真因（v0.5.17 の実運用で再確認）**: claude は**最初のメッセージを送るまで
    jsonl を書かない**＝起動しただけの未使用セッションは「uuid はあるが jsonl は
    無い」状態になる。稀な破損ではなく**通常状態**なのでフォールバックは必須。
    2026-07-25 の update-claude 実行でも claude-3（作り直し直後で未使用）が
    この経路に入り、v0.5.16 のフォールバックが実際に pane を救った。

### 2026-07-25 追加: P0 完了（herdrapi 型の一本化）

DESIGN_MULTI_AGENT.md の P0（マルチエージェント化の根本ボトルネック）を実装。

- `PaneInfo.Agent` / `AgentInfo.{Agent,Title,Tokens,AgentSession}` を追加
- organize の `orgPane` **二重 decode を廃止**し `herdrapi.PaneInfo` へ一本化
  （12＋19 箇所）。`listPanesWithAgent` も削除して `api.PaneList()` 直呼びに
- `selectRestartTargets` の **pane.list join を廃止**（agent.list 単独）。
  join は冗長なだけでなく**競合の窓**だった（1 接続=1 リクエストなので
  2 往復の間に構成が変わりうる）
- 注入 token キーの literal 二重定義を `herdrapi.InjTokenPC/SID` へ統一し、
  判定を `hasInjectToken(tokens)` 1 関数に集約
- ⚠実測で判明: **`agent.list` は tokens / agent_session を返す**。
  旧コメント「agent.list に tokens は載らない」は誤り。また agent.list は
  「名前付き agent の一覧」ではなく **pane と同数返る**（未命名は Name 空）
- ⚠**新しいフィールドが要るときは herdrapi に足す**。ローカル型を再び生やすと
  同じ二重管理に戻る
- 回帰テスト `TestAgentListCarriesTokensAndSession`（実 herdr）。JSON タグを
  壊すと FAIL することを確認済み＝「型だけ足して実は空」を検出できる

### 2026-07-25 追加: 仕様資料 2 本（SPEC.md / DESIGN_MULTI_AGENT.md）

- **[SPEC.md](SPEC.md)** = 機能・インターフェース仕様の正。CLI 全 16 サブコマンド／
  遠隔命令 6 種の Ack セマンティクスと順序制約／クラウドデータモデル／
  herdr API 28 メソッドと実測トラップ／設定／**不変条件 10 項**／デプロイ手順。
- **[DESIGN_MULTI_AGENT.md](DESIGN_MULTI_AGENT.md)** = 別のコーディング
  エージェント導入のための棚卸しと一般化設計。6 サブシステムを並列調査し
  敵対的検証＋作業ツリー再検証で確定（13 エージェント／208 万トークン）。
- **最重要の事実**: herdr は**既にマルチエージェント基盤**（検出 21 種／
  状態 manifest 19 種／session 追跡・resume 14 種）。drover が追いついていない
  という構図。しかも**対応は三層**（検出できる ≠ 会話を再開できる）で、
  gemini/agy/cline/kiro/amp/grok/maki は **resume 原理的に不能**。
- **根本ボトルネックは型の欠落**: `herdrapi.PaneInfo` に `agent` が無く、
  `AgentInfo` に `agent`/`agent_session`/`tokens` が無い。これが無い限り
  identity が命名規約に依存し続ける＝**P0**。
- ⚠herdr の resume argv テーブル（`agent_resume.rs plan()`）は **API 非公開**＝
  drover にミラーが要る（二重管理・herdr 更新で乖離しうる）。

### 2026-07-25 追加: マルチエージェント対応 P1〜P7 完了（v0.5.23 / drover-cloud v0.1.11）

DESIGN_MULTI_AGENT.md の着手順序を**全段完了**。**現行仕様の正は
[SPEC.md](SPEC.md) §4.2〜4.3・§7**（同 DESIGN は設計の意図と根拠の記録として残す）。

- **P1 identity 一元化** → `internal/agentid`（**cmd ではなく internal**。producer
  からも呼べないと判定が再び分裂するため）。権威は 注入 token → agent_session →
  シム命名 → herdr 検出値（canonical 21 への exact-match 必須）。矛盾は機械確定不能
  ＝対象外＋報告。**restart/update も herdr UI 直接起動のセッションを拾うようになった**
  （従来は organize だけが拾う非対称だった）。
- **P2 producer に `agent`** — 空なら載せない（後方互換）。`window_name` の優先順に
  検出種別を挿入＝シム経由でないセッションも意味のある名前になる。
- **P3 Cloud Run 先行デプロイ**（rev 00049・環境変数 7 個の生存を確認）。新命令名
  `restart-agent-session` / `update-agent-cli` / `restart-daemon`。**旧名は alias で残置**。
- **P4 Spec テーブル** — ResumeSpec（herdr 実ソースから 14 種）/ UpdaterSpec /
  InstallSpec。`--agent` を CLI・遠隔命令に開通。`ValidateSpecs()` が静的検証。
- **P5 Web UI** — 文言を session の `agent` で動的化。不明なら総称へ degrade。
- **P6 シム一般化** — `shim <agent>` ＋ **argv[0] multi-call**。バイナリ解決を
  新規起動が要ると分かるまで**遅延**（未導入エージェントの既存セッションへも attach 可）。
- **P7 ドキュメント同期** — SPEC / README / SETUP / CLAUDE / DESIGN_MULTI_AGENT。

#### この作業で見つけて直した実バグ（すべて回帰テスト付き・旧コード FAIL 確認済み）

1. **`gemini` シムが claude セッションに接続した**（実バイナリで symlink を張って
   検出）。シム入口だけ一般化して候補選定・resume backstop・命名を claude 固定で
   残したのが原因。**種別を跨いだ attach は「別エージェントに自分の会話を覗かせる」**
   に等しい＝SPEC §7-13 に不変条件として明記。
2. **update が恒常 abort しうる状態**。identity 拡大で非 shim pane の相対名 argv[0] が
   バイナリ候補に混ざり「2 種類＝曖昧」error。単独なら相対名が採用され launchd の
   PATH で exec 失敗＝**禁じたはずの PATH 依存の再導入**。→ 絶対パスかつ直接起動の
   argv[0] のみを候補にする（除外は必ず 1 行出す）。
3. **wrapper 起動 pane の誤再起動**。`zsh -lc '… claude'` に resume 引数を足しても
   本体に届かず、**会話を失ったまま done と報告**していた。→ `IsDirectInvocation` で
   loud に skip。identity（何か）と actionability（argv を組み直せるか）を分離。
4. **未命名 pane への勝手な命名**（herdr ネイティブ pane が drover 管理名に化ける）。
5. **`decodeAgentName` が `claude-+2` を通す**（`strconv.Atoi` の符号受理）。
6. **`-r report.md` が位置引数（初期プロンプト）に化ける**。旧実装は値を uuid 形かで
   判定していたが、`claude --help` 実測で `-r [value]` は **session ID か picker の
   検索語のみ**と確定。Spec 駆動（「そのフラグが値を取るか」）で正しく落ちる。

#### 引き継ぎ上の注意

- **新しいエージェントを足すときは `internal/agentid/spec.go` に Spec を足すだけ**。
  ⚠`InstallSpec.BinNames` は herdr の `lookup_agent` alias 表の要素にすること
  （表に無い basename で起動すると herdr の検出に**一切載らない**＝resume も
  organize も silent に無効化。`ValidateSpecs()` が起動時に落とす）。
- **旧命令名を allowlist から外さない**（まだ更新していない PC から投げられなくなる）。
- **テストの stub 名は `claude`**（`claude-stub` ではない）。argv ゲートは basename を
  見るので、別名 stub だと本番と違う経路（skip）に落ちてテストが担保にならない。
- resume 表は herdr の **API 非公開**部分のミラー＝herdr 更新時は要照合
  （取りこぼしても「resume 非対応」へ loud に落ちるだけで破壊はしない）。

---

---

### 2026-07-25 追加: codex / cursor 実機検証（v0.5.24）

このPCに codex（brew cask 0.145.0）と cursor-agent（2026.07.23）を導入し、
**マルチエージェント対応を実エージェントで検証**した。`.zshrc` に alias を配線
（`codex` → `shim codex`／`cursor-agent` → `shim cursor`。claude と同じ方式）。

**仕組みの要点**: `agent_session` は herdr が自力で見つけるのではなく、
**各エージェントの hook が報告する**（`herdr integration install <agent>`／
`herdr integration status` で確認）。**未設置なら resume は原理的に不可能**。

⚠ このPCでの実施記録: install 前の `status` は claude=current(v7) /
copilot=current(v2) / codex・cursor=not installed と表示したので install した。
**ただし「未設置だったから動かなかった」は言い過ぎ**（cursor が動かなかった
真因は下記の Workspace Trust ダイアログで、integration ではない）。他機では
既に設置済みの場合がある＝**環境ごとに status で確認すること**。

| | 検出 | agent_session | resume argv | 再起動 |
|---|---|---|---|---|
| claude | ✅ | ✅ 起動時 | `--resume <id>` | ✅ |
| codex | ✅ | ✅ **初回発話時** | `codex resume <id>` | ✅ |
| cursor | ✅ | ✅ **初回発話時**（要 trust 通過） | `--resume <id>` | ✅ |

**3 種すべて restart を実機で成功**。herdr の `plan()` から写経した ResumeSpec が
3 形（`--resume <v>` / 位置引数サブコマンド / argv[0] 差異）とも実データで正しい
argv を組み立てた＝**Spec 抽象が claude 以外でも機能する**ことの実証。

- ⚠**cursor は初回 cwd で `Workspace Trust Required` ダイアログが出て入力を全部
  吸う**。ここを `[a]` で通すまで会話が始まらず agent_session も付かない。
  自動化からは「検出されるのに永久に idle」に見える。**`herdr pane read <pane>` で
  画面を読んで初めて分かった**（それまで hook の不具合を疑って空振りしていた）。
- ⚠**codex は resume 後に hook が再発火しない**（hook 呼び出しログで実測）。
  同じ pane の 2 回目の restart は素起動になる。drover 側の問題ではない。

#### ⚠ CLAUDE_CODE_CHILD_SESSION による transcript 抑止（原因特定＝v0.5.27 で修正）

herdr server が `CLAUDE_CODE_CHILD_SESSION` を持つと（**Claude Code の中から herdr
server を起動すると起きる**）、herdr が生やす全 pane が継承し、その claude は
transcript を保存しない。`--resume` が読むのはそれなので、**そのマシンの claude
セッションはどれも復元できない**。

**混入経路は drover 自身**（v0.5.27 `54bccf6` で修正済み。`sanitizedServerEnv`）。
ただし**修正が効くのはこれから起動する server だけ**＝**既に汚染された稼働 server は
再起動しないと直らない**。

##### 2026-07-26 の実測（mac-studio で再確認・SPEC §「transcript 抑止」に正）

- 本番 server = **pid 31863（7/17 23:07 起動）** が汚染当事者。`ps eww -p 31863` で
  `CLAUDE_CODE_CHILD_SESSION=1` を直接確認。**pane 側の `CLAUDE_CODE_SSE_PORT` が
  server のそれと同値**＝継承の決定的証拠（自分のポートではない）。
- ⚠ **`herdr server live-handoff` では直らない**（隔離 herdr で実測）。pane は生き残るが
  **新 server が旧 server の env を丸ごと継承**し、handoff 後に作った pane も汚染継承。
  **clean env から呼んでも無意味**＝この道は塞がっている。**再検証で時間を使わないこと**。
- ✅ **`CLAUDE_CODE_FORCE_SESSION_PERSISTENCE=1` は claude 2.1.220 で効く**
  （汚染 server 配下でも jsonl 生成を確認・対照群は 0 件）。**2.1.219 での「効かない」
  という旧結論は版依存だった**＝実測表には必ず claude の版を書くこと。
  → 汚染サーバを直ちに再起動できない場合の緩和策になる（新規起動の claude にのみ効く）。
- ⚠ `herdr workspace create --env KEY=VALUE` は **root pane に届かない**（実測）。
- ⚠ **テスト残骸の herdr server が常駐しうる**（`/tmp/hd*` の socket・7/18 起動が 2 本
  8 日間リークしていた）。本番との判別は `lsof -t <socket>`。裸の `pkill herdr` は恒久禁止。

##### 実施記録: クリーン再起動（2026-07-26）

ユーザー判断で**クリーン再起動を選択**（FORCE 緩和策の実装ではなく根治）。手順と
検証コマンドは **SPEC.md「クリーン再起動の手順」** が正。⚠実行すると**全 pane の
プロセスと会話が失われる**（レイアウトは session.json から復元。汚染下の claude は
transcript が無いので `--resume` は即終了＝会話は戻らない）。

drover 側の挙動は正しい: `--resume` 即終了 → 二段構えフォールバックが pane を
必ず残し「resume 復元不可のため新規会話で起動」と正直に報告する。

⚠ fleet の他 3 台は正常（`update-all` 履歴を全確認・「復元不可」ゼロ＝8 セッション
すべて resume で引き継げている）。この問題は mac-studio 固有。

#### ⚠ 自動化を止める「フォルダ信頼ダイアログ」（claude / cursor 両方）

新しい cwd では両方とも信頼ダイアログを出して**入力を全部吸う**。通過するまで
会話が始まらず `agent_session` も付かないので、自動化からは「検出されるのに永久に
idle」に見える。**`herdr pane read <pane> --source visible` で画面を読むこと** —
これが無いと辿り着けない（両方ともこれで初めて判明。それまで hook や integration の
不具合を疑って空振りした）。

- claude: `Quick safety check: …` → Enter
- cursor: `⚠ Workspace Trust Required` → `[a]`

#### この検証で見つけた実バグ（v0.5.24 で修正）

1. **argv ゲートの種別ハードコード**（最も危険だった）。条件が `"claude"` 固定で、
   codex/cursor の pane は argv[0] が正しくても**必ず skip** されていた。しかも
   **メッセージだけ `t.AgentKind` を出す**ので「codex の直接起動でない
   （argv[0]=".../codex"）」という矛盾文言になり一見正しく見えた。単体テストも
   dry-run も claude 経路しか通らず気づけなかった＝**種別を変えた経路を必ず 1 本通す**。
2. **`IsDirectInvocation` の判定表が claude だけ**（同上の一段手前）。
3. **argv[0] multi-call の無限自己 exec**。シム symlink を PATH 前方に置くと
   LookPath がシム自身を返す。alias 方式では起きない＝**設定方法による非対称**。

#### Windows 機（desktop-djb9pfr-herdr）は更新不能と確定

`self-update` が `herdr-drover_windows_amd64.exe` を要求して 404。現行コードは
**Windows ビルド自体が通らない**（`syscall.Flock`／`placeBinary` が unix 専用＝実測）。
TODO の out-of-scope 宣言どおりで、対応するなら移植作業が要る。

---

## 進行中 / 保留（再開ポイント）

### 0. Windows 移植（**コード本体は main 済**・2026-07-26）

作業機は **`desktop-djb9pfr-herdr`（Windows 11・herdr 0.7.4-preview）**。

**ブランチ構造（重要）**:
- **`main`**: Windows 移植のコード本体が入っている（`019d051 feat(windows)` /
  `8f831c6 fix(test)` / docs 3 本 / `41ce39e build(windows)`）。**未 push**。
- **`windows-port`**: main ＋ **`herdr-plugin.toml` の 1 commit だけ**。
  main へ merge しないこと（理由は当該 commit メッセージ＝events 削除は
  macOS/Linux の即時 re-scan を殺す・platforms への windows 追加はできない約束）。
- `windows-port-backup-2026-07-26`: 分割前の旧ブランチ（不要になったら削除可）。

なぜ共通ソースに入れてよいと判断したか:
- 移植は **build-tag 分割＋seam 化**で unix 側はバイト等価。対話系
  （attach/ssh-forward/localview）は Windows 非対応スタブ。
- 唯一の実コストは **スタブのドリフト**（unix 側シグネチャ変更に追従できず
  Windows でだけ壊れる。実例: pull で `runRemoteInject` に引数が増えた）。
  → **`make check-windows`（`GOOS=windows` の build＋vet）を常設**して検出する。
  Windows 実機不要（cgo 不使用）。`cmdSSHForward` のスタブ引数を削ると
  `too many arguments in call to cmdSSHForward` で落ちることを**実証済**。
- 前提だった「公開タグだけで Windows がビルドできる」は解決:
  **drover-cloud v0.1.13**（`selfupdate/place_windows.go`）を発行し go.mod を更新。
  `go.work` は不要になったので削除。⚠ **go.mod に replace を書かないこと**
  （`GOWORK=off make dist` の規律が壊れる）。
  ⚠ 発行時に上流で **v0.1.12 が別マシンから先に出ていた**ため本件は v0.1.13。
  タグを切る前に必ず `git fetch --tags` すること。

🔴 **実害（2026-07-25・恒久教訓）**: Windows で `go test ./...` を走らせると
**実ユーザーの `~/.herdr-drover` が test fixture で上書きされ、`enroll(slave)` の
残骸掃除（`enroll.go` の `os.Remove(sa.json/clouds.json)`）で実 SA 鍵が消えた**。
真因は `os.UserHomeDir()` が unix=`$HOME` / **Windows=`%USERPROFILE%`** を読むこと＝
`t.Setenv("HOME", tmp)` だけの隔離が Windows で **silent に無効**だった。
対処は `setTestHome(t, dir)`（HOME＋USERPROFILE を設定し `os.UserHomeDir()` が
実際に dir を返すことをその場で検証＝破れたら書き込む前に落ちる）。既存 55 箇所を置換。
**他 OS の隔離テストを新 OS で走らせる前に、隔離が実際に効くかを確かめること。**

- ✅ **この PC の `~/.herdr-drover` は復旧済み**（2026-07-25 23:25）。被害後の状態は
  `~/.herdr-drover.bak-2026-07-25` に保全。復旧内容:
  - `config.json` を daemon ログ（`agent.err.log` 冒頭）由来の実値へ書き戻し
    （`gcp_project=claude-master-4noha` / `cloud_relay_url=wss://claude-master-relay-
    nkzxa3hxma-an.a.run.app` / `learn_moves=true`。role なし＝master。
    `pc_id` は hostname 由来で `desktop-djb9pfr-herdr` に一致するので書かない）。
  - `clouds.json` は**置かない**（`LoadClouds` は不在なら config.json から単一
    クラウドを導出＝ログの `clouds=1` と一致。SAKeyPath は既定 `~/.herdr-drover/sa.json`）。
  - fixture ゴミ（`slave.json`・`sa-proj-*.json`・fixture `workspaces.json`）を削除。
    workspaces.json は**実内容が失われた**＝不在＝「ルール無し」から再学習
    （`learn_moves=true`／`organize --capture`）。
  - `sa.json` は同一 GCP プロジェクトの既存鍵 `~/.claude-master/sa.json`
    （`cm-agent@claude-master-4noha`）から復元し、ACL を本人のみへ制限（600 相当）。
  - `herdr-drover status` で project/relay/pc_id/tick が被害前ログと**一致**を確認。
  - ✅ **SA 鍵の認証も実機で検証済み**（23:56 の再起動）。この PC に ADC は無い
    （`application_default_credentials.json` 不在・env 未設定＝実測）ので、
    `state.NewWithCredentials` が成功して「クラウド開始」まで進んだ時点で
    **鍵ファイルが使われた**ことが確定し、その後 tick（5s 周期の Firestore push）が
    **エラー 0 で継続**＝RPC も認可されている。
  - 🔍 **`google_application_credentials` は元々明示設定されていた**（推定ではなく
    証拠あり）: `⚠ GOOGLE_APPLICATION_CREDENTIALS 未設定` の警告は**旧コード
    （dab5a6a の config.go）にも存在**するのに、被害前 14:25 の起動ログには
    出ていない＝当時 `cfg.Credentials` は非空だった。よって復元も明示設定に
    合わせた（`C:\\Users\\nokki\\.herdr-drover\\sa.json`）。起動ログは被害前と
    同じ形（警告なし）に戻っている。
    ⚠ JSON なのでパス区切りは `\\`（heredoc で単一 `\` を書くと `\U`/`\n` が
    不正エスケープになり config が壊れる。一度やらかして書き直した）。
- ✅ **この PC へ反映済み（2026-07-25 23:56・`v0.5.24-4-g2793307`）**。Windows の
  反映手順は macOS の rm→cp＋launchctl とは別＝以下が実測レシピ:
  1. `CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(git describe
     --tags --always --dirty)" -o bin/herdr-drover.exe ./cmd/herdr-drover`
  2. ⚠ **先に `./bin/herdr-drover.exe version` を実行して SAC を通るか試す**
     （新規ビルド exe はブロックされ得る＝稼働バイナリを差し替えた後に判明すると
     daemon が起動不能になる。ブロックされたら差し替えないこと）
  3. `~/.herdr-drover/agent.pid` の pid **だけ** を `Stop-Process`（プロセス名が
     herdr-drover であることを確認してから。裸の `pkill herdr` は恒久禁止）
  4. 稼働 exe を**上書きせず rename 退避**（`herdr-drover.exe.old-<date>`＝
     ロールバック用）→ 新 exe を copy
  5. `Start-ScheduledTask -TaskName herdr-drover-agent`（タスクは logon トリガ・
     `bin/start-agent.ps1` が herdr server 起動＋`agent` を Start-Process）
  6. `agent.err.log` で起動 6 行＋`tick エラー` 0 を確認
  - ⚠ `start-agent.ps1` は `-RedirectStandardError` で **`agent.err.log` を切り詰める**
    ＝再起動前に必要なら退避する（今回の復旧根拠はこのログだった。被害前ログは
    `~/.herdr-drover.bak-2026-07-25/agent.err.log.pre-restart` に保全）。
  - ロールバック: 4 の `.old-2026-07-25` を戻して 5 を再実行。
- 🔴 **Windows は self-update できない**（follow-up）: `make dist` の対象は
  linux/{amd64,arm64}・darwin/{amd64,arm64} のみで **windows を作っていない**のに、
  `selfupdate` は `herdr-drover_windows_amd64.exe` を探す（`selfupdate.go`）＝
  release に asset が無く必ず失敗する。**この PC 宛の pending `update-all` は
  受信しても update 部分が失敗する**（23:58 時点で未受信）。Windows PC を
  正式運用するなら `make dist` に windows/amd64 を足すのが先。
- ⏳ **残る Windows テスト赤**（実害なし・いずれも移植の未了）:
  - `internal/wsmap`: fixture が POSIX パス（`/w/proj` は Windows で非絶対）＝
    Parse/Resolve が落ちる。**実運用キーは `C:\...` で絶対＝production は通る**が、
    セパレータ/大文字小文字（Windows は case-insensitive）の正規化は未検討。
  - `internal/herdrapi`: 実 herdr harness が `/tmp` 前提（Windows は temp dir＋
    named pipe へ要移植）。`TestSocketPathResolution` は既定パスが OS 依存に
    なった（`%APPDATA%\herdr\herdr.sock`）＝期待値を OS 分割すること。
  - `internal/injectindex`: `perm=0600` 判定（Windows に POSIX perm 無し）。
  - `internal/agentfwd`: unix socket＋`/tmp`（SSH 転送＝Windows out-of-scope）。
  - `cmd/herdr-drover`（実行できた回の内訳・**製品バグは 0・全て harness**）:
    ①`startHerdrForTest` が `/tmp` 前提（macOS の `sun_path` 104B 対策）＝
    Windows に `/tmp` が無く**実 herdr テスト約 15 本が setup で落ちる**。
    Windows 用 temp dir＋named pipe パスへ要移植（ここを直すと一番効く）。
    ②`TestStdinIsTTYRealDevices` は POSIX pty 前提（Windows は ConPTY）。
    ③`TestLookupAgentBinSkipsShimItself` は `os.Symlink` に開発者モード/管理者
    権限が要る（`A required privilege is not held by the client`）＝skip 条件が要る。
  - ⚠ **Smart App Control**（`VerifiedAndReputablePolicyState=1`）が**新しく
    ビルドした未署名 exe** をブロックする（`An Application Control policy has
    blocked this file`／bash 越しは `Permission denied`）。`*.test.exe` は
    間欠的にブロックされ、`go run`/`go build` した使い捨てバイナリは実測で
    毎回ブロックされた（古い `~/.herdr-drover/bin/herdr-drover.exe` は動く＝
    レピュテーションで枯れた実行体は通る）。`GOTMPDIR` 変更では回避不可。
    **SAC は一度切ると Windows 再インストールでしか戻せない＝切らない**。
    テストが赤/緑どちらとも言えない回はこれを疑い、まず再実行する。
  - `install`/`update` は launchd/inode 前提＝`//go:build unix` にした。**Windows の
    常駐化（タスクスケジューラ）と update の Windows 経路はテスト未整備**。

### A. SSH エージェント転送 — Phase 3（実機 e2e）保留中

共用 slave 上で owner の SSH 鍵を**ディスクに置かず**一時的に git/gh 認証する
仕組み。設計 **[DESIGN_SSH_FORWARD.md](DESIGN_SSH_FORWARD.md)**。方式はユーザー
確定＝**SSH agent forwarding を relay 越しに**（署名は owner Mac が実行・秘密鍵は
slave に出ない）＋専用 deploy key＋`ssh-add -c`。用途は「repo A をローカルと
slave 両方で検証」（エージェント対エージェント）。

- **Phase 1/2 ✅（コード完成・v0.5.0 リリース済み）**:
  - `internal/agentfwd/`＝単一バイト透過パイプ上の SSH agent 接続多重化 mux
    （wire `[type:1][ch:4][len:4][payload]`・ID は LISTENER(slave) 単調割当・
    DIALER(owner) は未知 ID で dial・512KB 上限・late-DATA 再 dial 防止）＋
    Owner/Slave/SlaveSocket。**-race 緑・実 relay.Server 越し e2e 緑**。
  - owner CLI `herdr-drover ssh-forward <pc> [label]`（`sshforward.go`）＝
    `PutRelayGrant(afSid,"viewer")`＋`Wake(pc,afSid)`＋`DialViewerFrom`＋
    `agentfwd.Owner($SSH_AUTH_SOCK)`・Ctrl-C 撤去・backoff 再接続。
  - slave は `webterm.handleWake` が `isSSHForwardSid`（`afwd:` prefix）で分岐→
    `handleSSHForwardWake`（source grant＋dialSource＋SlaveSocket＋`agentfwd.Slave`・
    owner 切断で socket 自動撤去）。**wake ベース＝attach/↗窓 と同一機構を再利用し
    relay/state/web/CommandRunner を無改変**（最小差分）。
- **Phase 3 ⏳（保留・ユーザー判断「今は release までで止める」2026-07-19）**:
  実機 e2e。再開レシピ:
  1. slave（n9htqcr6g0-herdr 等）で `herdr-drover update`（→最新 v0.5.18。最終確認 v0.4.4）。
     v0.5.9 の遠隔コマンド `self-update` で owner の Web からも push できるはず（未実測）。
  2. owner で `~/.herdr-drover/bin/herdr-drover ssh-forward <slave> repoA`
     （SSH_AUTH_SOCK 稼働中）。
  3. slave で `SSH_AUTH_SOCK=~/.herdr-drover/agent-fwd/afwd-repoA.sock \
     git ls-remote git@github.com:<you>/repoA`（read-only probe→clone/pull）。
  4. owner 名義で認証されるか確認→本番は repo 限定 read-only deploy key を
     `ssh-add -c` へ差替。
  - ⚠owner は `herdr-drover` が PATH 未登録＝full path
    `~/.herdr-drover/bin/herdr-drover` で起動。

### B. Google IME + herdr で Ctrl+J/K/L/;/: がおかしい — WezTerm 設定で解決（副作用チェック確認中）

- **真因（確定・2026-07-21 更新）**: 外側端末は WezTerm、Google IME は CUSTOM keymap で
  Ctrl+J/K/L/;/: を割当。WezTerm 既定 `macos_forward_to_ime_modifier_mask` は `SHIFT|ALT`
  で **CTRL を含まない**＝Ctrl 修飾キーが IME へ転送されず、変換中でも IME keymap が発火せず
  WezTerm が制御文字（Ctrl+J=改行 等）を claude へ送る。herdr/zsh は無実（zsh 単体でも再現）。
  旧メモ「WezTerm は既定で OK のはず」は誤りだった。
- **対処（適用済み）**: 両 Mac（mac-studio・d24wt27c3j）の `~/.wezterm.lua` に
  `config.macos_forward_to_ime_modifier_mask = 'SHIFT|ALT|CTRL'` を追加。**変換中 Ctrl+J の
  ひらがな変換を d24wt27c3j で実確認**。⏳ 残: 副作用チェック（IME 日本語 ON・未入力で
  Ctrl+C/Ctrl+A 等が端末へ届くか）をユーザー目視。不都合なら `'SHIFT|CTRL'` 等へ。
  - ⚠ WezTerm config リロードは **再起動/新窓/Cmd+Shift+R**（新規作成ファイルは auto-reload
    非対応）。**`kill -HUP` は terminate 扱いで厳禁**（誤って落とした事故あり）。
  - Precomposition(未入力) の別ケースは上記に加え Google IME keymap の Precomposition
    モードにも Ctrl+J/K/L/;/: 割当追加が必要（IME 側設定・任意）。

### C. resume backstop（旧タスク・cm findLiveManagedByUUID の herdr 版・未着手）

`claude --resume <uuid>` の 2 回目が新プロセスを作る問題（同一会話 jsonl に
2 プロセス）。シムが `--resume <uuid>` で `pane.report_metadata` token に uuid を
記録→2 回目は token 一致 pane が生きていれば attach。詳細は下記「旧仕様メモ」。

### D. organize / claudeshim の空 root pane 掃除（follow-up）

reconcile は v0.5.8 で「workspace 新規作成時の空 root pane を掃除」済み
（`wsmap.ResolveWorkspaceIDWithRoot`）。**organize / claudeshim も同じ
`ResolveWorkspaceID` の create 経路で同種の空 root ゴミが出る**（着地先 workspace が
不在で新規作成される時＝稀。通常は既存 WS へ着地するので顕在化しにくい）。同じく
`ResolveWorkspaceIDWithRoot` に切替え、**tab/pane 追加後に root を close**すれば直る。
organize はループ＋`created` cache、claudeshim は線形なので、各々コンテンツ追加後に
1 回だけ close する配置に注意（先に close すると空 WS が auto-close）。

---

## デプロイ手順（cm 教訓の rm→cp 新 inode）

```sh
cd ~/works/tools/herdr-drover
gofmt -l . | grep -v state.go        # state.go は cm バイト同一コピー＝整形しない
go build ./... && go vet ./... && go test ./... -count=1
git add -A && git commit             # 日本語・Co-Authored-By: Claude
# リリースする場合（GOWORK=off＝公開 drover-cloud タグで解決）:
GOWORK=off make dist VERSION=vX.Y.Z
git tag -a vX.Y.Z -m "..." && git push origin main && git push origin vX.Y.Z
gh release create vX.Y.Z dist/herdr-drover_* dist/checksums.txt --title "..." --notes "..."
# ローカル daemon/CLI 反映（⚠上書き cp は macOS 署名キャッシュで SIGKILL）:
rm ~/.herdr-drover/bin/herdr-drover && cp dist/herdr-drover_darwin_arm64 ~/.herdr-drover/bin/herdr-drover
codesign -s - -f ~/.herdr-drover/bin/herdr-drover
launchctl kickstart -k gui/$(id -u)/com.4noha.herdr-drover
```

- ⚠**rm→cp の際に稼働 daemon が codesigning トラップで SIGKILL され KeepAlive が
  新バイナリで再起動する**（実測 2026-07-19）＝kickstart 省略でも daemon は新版に
  なるが、明示 kickstart が確実。attach（↗窓）子プロセスは daemon 再起動を跨いで
  生存＝**attach.go を変えたリリースでは明示的に作り直す**:
  `pkill -f 'herdr-drover attach'` → **その後に `launchctl kickstart -k`**。
  ⚠**pkill だけでは復旧しない**（実測 2026-07-25）: reconcile は Firestore snapshot
  駆動でローカル pane の消滅を契機にしないため、注入 pane が 11→0 のまま放置される。
  daemon の起動時 reconcile が唯一の確実な再生成契機なので kickstart が必須。
  （attach.go 無変更のリリースでは機能差ゼロ＝この手順自体が不要）。
- ✅**v0.5.29 以降はこの pkill が不要**（`attachrefresh.go`＝版数が変わった起動の 1 回
  だけ注入 pane を自動で作り直す。SPEC §6.3）。**v0.5.28 以前の PC へ配る回だけ**は
  旧コードが命令を処理するが、新バイナリの起動時判定で追いつくので手動作業は要らない。
  （2026-07-26 に「遠隔更新は attach.go の変更を配れない」を実測して足した仕組み。）
- ⚠バイナリ/設定はプロセス起動時のみ反映＝各セッションは新規起動で新版。
- ⚠**リリースビルドは GOWORK=off**（go.work のローカル drover-cloud でなく go.mod
  宣言の公開タグで解決）。⚠go.work は**このリポではなく `~/works/tools/go.work`**
  （親ディレクトリの workspace が drover-cloud をローカル解決している）。
  ⇒ `go build`/`go test` はローカル drover-cloud、`GOWORK=off make dist` は公開タグ、
  という**非対称が常時ある**ことを意識すること。usage() は backtick raw string＝
  **中に `` ` `` を入れない**（文字列が途中で閉じてビルド破壊。v0.5.0 で実際に
  やらかして amend 修正）。

---

## 残バックログ（優先順）

0. ~~**遠隔更新が attach 子プロセスを作り直さない**~~ → **v0.5.29 で解決**
   （`cmd/herdr-drover/attachrefresh.go`）。daemon 起動時に `~/.herdr-drover/attach-version`
   と自版数を比べ、**変わった起動の 1 回だけ**注入 pane を撤去する（撤去は BUG-2 の
   `emptyRemoteSource` 経路・再生成は起動時 kick ＝新機構ゼロ）。**同一版数の起動では
   何もしない**＝通常再起動で ↗窓 を瞬断させない。仕様は SPEC §6.3。
   ⚠ **v0.5.28→v0.5.29 の配信では旧コードが命令を処理する**が、新バイナリの**起動時**に
   スタンプ不在（＝作り直す側）と判定されるので、その 1 回で追いつく。
0b. **遠隔命令を投げる CLI が無い**（Web UI か `state.PushCommand` を直に叩くしかない）。
   v0.5.28 の配信は scratchpad の使い捨てツールで投入した＝`herdr-drover push-command
   <pc> <cmd>` 相当を CLI に足すのが筋（allowlist 検証は state 側が持っている）。
1. **SSH 転送 Phase 3 実機 e2e**（上記 A・保留中＝ユーザー都合の良い時）。
2. **organize/claudeshim の空 root pane 掃除**（上記 D・reconcile は v0.5.8 で修正済＝
   同経路の残り。稀だが同種ゴミ）。
3. **IME Ctrl キー**（上記 B・WezTerm 設定で解決＝副作用チェックのみ残）。
4. ~~**resume backstop**~~ → **実装済**（`parseResumeRef` / `findAgentPaneByResumeRef`。
   Spec 駆動＝全 14 種の resume 形に対応・種別を跨がない）。
5. GitHub 公開: リポジトリ push＋topic `herdr-plugin`→marketplace 掲載
   （**対外操作＝ユーザー明示確認後**）。※herdr-drover は既に GitHub 公開済み
   （release 発行運用中）。marketplace topic 付与が残。
6. 複数クラウド実 GCP e2e: 2 つ目の GCP プロジェクト/SA 鍵が要る（要 Mac canonical）。
7. **Windows 対応** → **in-flight**（上記 0・branch `windows-port`）。build/vet は
   3 OS 緑。残: この PC の設定復旧・drover-cloud v0.1.12（place_windows.go）・
   Windows テストの移植・常駐化（タスクスケジューラ）。

---

## 環境の現状（2026-07-25 時点）

- herdr 0.7.4（brew）・既定サーバ稼働・plugin link 済（`4noha.drover`）。
- 稼働 launchd `com.4noha.herdr-drover`（クラウド `claude-master-4noha`/既デプロイ relay
  `wss://claude-master-relay-nkzxa3hxma-an.a.run.app`）。
- **enroll 済み PC**: mac-studio-herdr(owner/master)・d24wt27c3j-herdr(master)・
  **n9htqcr6g0-herdr(slave)**・**lph77xyyc7-herdr(slave)**。
- **版数の実測状況**（`herdr-drover status` は稼働 CLI/daemon の両方を返す）:
  - d24wt27c3j-herdr（本作業マシン）: v0.5.18（daemon/CLI 一致・実測 2026-07-25）
  - mac-studio-herdr: 別 PC で v0.5.15〜v0.5.18 を実装（rebase 統合済）。実測は必要時。
  - slave 2 台: 最終確認 v0.4.4（remote self-update は slave 非到達＝各機で
    `herdr-drover update` 要）。v0.5.9 の遠隔コマンド（`slave/commands` long-poll）で
    slave にも push 到達するはずだが実機確認は SSH 転送 Phase 3 と併せて要検証。
- `claude` alias は drover シム（`~/.herdr-drover/bin/herdr-drover claude`）。
  別エージェントは symlink（argv[0] multi-call）か `herdr-drover shim <agent>`。
- owner の `herdr-drover` は PATH 未登録＝full path で起動。SSH_AUTH_SOCK は稼働中。
- SA 鍵 `~/.herdr-drover/sa.json`（600・非コミット）。cm 世界は本 Mac では店じまい済み。
- herdr ソースクローン（v0.7.4・AGPL＝vendor 禁止・参照のみ）: scratchpad の `herdr/`
  （消えていたら `git clone https://github.com/ogulcancelik/herdr && git checkout v0.7.4`）。

---

## 旧仕様メモ（参考保持・resume backstop 用）

⚠ **以下は v0.5.23 より前の記述**。現行の同定規則は `internal/agentid.Resolve`
（SPEC.md §4.2）＝注入 token → agent_session → シム命名 → 検出値の優先順。

- ~~claude pane 同定は 2 系統 OR・どちらも exact: (a) シム命名 `claude`/`claude-N`
  (b) herdr 検出種別 `agent=="claude"`（name=None の直接起動も対象）。~~
- `--resume <uuid>` 起動時に `pane.report_metadata` token へ uuid を記録
  （`PaneInfo.Tokens`・exact-match 可）→2 回目は token 一致 live pane に attach。
  「args 非空→常に新規」の例外は --resume のみ。⚠Tab 版が claudeshim.go を
  触っているため必ずその着地後に実装。
- herdr 0.7.4 trap: ndjson は 1 接続=1 リクエスト（events.subscribe のみ長寿命）／
  入力は `pane.send_text`（`send_input` は \r 落ち）／`terminal session control` は
  隠し CLI（attach と同じ pane resize＋lock）／`tab.move` は同一 WS reorder 専用＝
  WS 間は `pane.move` が唯一／workspace label は重複可＝id を使う。
