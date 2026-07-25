# 実装手順メモ / 引き継ぎ（2026-07-25 更新）

**開発の鉄則は [CLAUDE.md](CLAUDE.md) / DESIGN.md 末尾**（実テスト担保・旧コード
FAIL 確認・exact-match・AGPL 衛生・silent 変更禁止・対外操作はユーザー確認後）。
**再開時はまずこの TODO.md を通読**（in-flight・残課題・デプロイ手順の正）。

---

## 最新状態（2026-07-18〜25・最新タグ v0.5.18／HEAD は未タグ 2 commit ahead）

直近で以下を実装・リリース済み（詳細は各 DESIGN doc）。**全て darwin/linux
build 緑・テスト緑**。origin/main = v0.5.18（`5bb956d`）、HEAD は `f539700`
（MagicDNS 名）＋`1088c2e`（watchLifecycle）の未タグ 2 commit（別 PC の v0.5.15〜
v0.5.18 と rebase 統合済＝2026-07-25）。稼働 launchd `com.4noha.herdr-drover`
（pc=`mac-studio-herdr`）は **v0.5.18**。

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

---

## 進行中 / 保留（再開ポイント）

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
- ⚠バイナリ/設定はプロセス起動時のみ反映＝各セッションは新規起動で新版。
- ⚠**リリースビルドは GOWORK=off**（go.work のローカル drover-cloud でなく go.mod
  宣言の公開タグで解決）。usage() は backtick raw string＝**中に `` ` `` を入れない**
  （文字列が途中で閉じてビルド破壊。v0.5.0 で実際にやらかして amend 修正）。

---

## 残バックログ（優先順）

1. **SSH 転送 Phase 3 実機 e2e**（上記 A・保留中＝ユーザー都合の良い時）。
2. **organize/claudeshim の空 root pane 掃除**（上記 D・reconcile は v0.5.8 で修正済＝
   同経路の残り。稀だが同種ゴミ）。
3. **IME Ctrl キー**（上記 B・WezTerm 設定で解決＝副作用チェックのみ残）。
4. **resume backstop**（上記 C・未着手）。⚠claudeshim.go を触るので他タスクと衝突注意
   （organize/claudeshim root 掃除と同ファイル＝まとめて着手が吉）。
5. GitHub 公開: リポジトリ push＋topic `herdr-plugin`→marketplace 掲載
   （**対外操作＝ユーザー明示確認後**）。※herdr-drover は既に GitHub 公開済み
   （release 発行運用中）。marketplace topic 付与が残。
6. 複数クラウド実 GCP e2e: 2 つ目の GCP プロジェクト/SA 鍵が要る（要 Mac canonical）。
7. Windows 対応（out-of-scope 宣言済・将来。現状 wsmap/selfupdate が unix-only）。

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
- owner の `herdr-drover` は PATH 未登録＝full path で起動。SSH_AUTH_SOCK は稼働中。
- SA 鍵 `~/.herdr-drover/sa.json`（600・非コミット）。cm 世界は本 Mac では店じまい済み。
- herdr ソースクローン（v0.7.4・AGPL＝vendor 禁止・参照のみ）: scratchpad の `herdr/`
  （消えていたら `git clone https://github.com/ogulcancelik/herdr && git checkout v0.7.4`）。

---

## 旧仕様メモ（参考保持・resume backstop 用）

- claude pane 同定は 2 系統 OR・どちらも exact: (a) シム命名 `claude`/`claude-N`
  (b) herdr 検出種別 `agent=="claude"`（name=None の直接起動も対象）。
- `--resume <uuid>` 起動時に `pane.report_metadata` token へ uuid を記録
  （`PaneInfo.Tokens`・exact-match 可）→2 回目は token 一致 live pane に attach。
  「args 非空→常に新規」の例外は --resume のみ。⚠Tab 版が claudeshim.go を
  触っているため必ずその着地後に実装。
- herdr 0.7.4 trap: ndjson は 1 接続=1 リクエスト（events.subscribe のみ長寿命）／
  入力は `pane.send_text`（`send_input` は \r 落ち）／`terminal session control` は
  隠し CLI（attach と同じ pane resize＋lock）／`tab.move` は同一 WS reorder 専用＝
  WS 間は `pane.move` が唯一／workspace label は重複可＝id を使う。
