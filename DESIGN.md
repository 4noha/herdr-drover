# herdr-drover 設計（v1・確定）

herdr のセッション群を PC 間・クラウドへ「駆り立てる」standalone プラグイン。
claude-master-go（cm）の実績あるクラウド同期（Firestore push・Cloud Run relay
WSS・Web/スマホ閲覧・near-$0）を **herdr の世界**で提供する。cm リポジトリ・
既デプロイクラウドは**一切無改変**（shared-cm-cloud）。

設計は 3 本のマルチエージェント調査（herdr v0.7.4 ソース完全精読＋**実機ライブ
検証**＋cm コード実読、主張検証 12/12 confirmed）で確定した。

## 決定事項（要旨）

| 決定 | 内容 | 根拠 |
|---|---|---|
| stream 源 | `herdr terminal session observe/control`（同梱 CLI サブプロセス） | pipe 完全対応のヘッドレス ndjson・CHANGELOG に bridge 用途明記・実機 e2e 実証。同一バイナリの client を使う＝PROTOCOL_VERSION(16) 完全一致問題が構造的に消滅 |
| クラウド | shared-cm-cloud（既デプロイ relay/web/Firestore を無改変共有） | relay/web/state はバイト透過・cm 固有形式検証なし（コード実読）。**pc id は必ず `<host>-herdr`**＝cm agent と分離（同一 id は DeleteSession 削除合戦になる: 検証済） |
| セッション key | pane_id（`w1:p1`） | herdr server 再起動を跨いで安定（実測）・Firestore doc id 制約適合。terminal_id は揮発ハンドル |
| 入力経路 | primary: ndjson `pane.send_input`(text)／fallback: `terminal session control` 一時接続の `terminal.input{bytes:base64}` | send_input の制御バイト透過性は \r のみ実証＝Phase 2 で実測して決定木を確定 |
| Web 閲覧 | observe（多重・PTY 非影響・viewer 毎サイズレンダ） | control は実 PTY resize＋ローカル resize lock（code-read）＝閲覧に使わない |
| 注入 pane の sid | 派生 sid `<pane_id>#inj` | relay は 1 sid=viewer 1 本＋takeover＝Web /term と同 sid だと相互蹴り出しストーム。herdr は多重 observer 設計なので bridge 並走は無料 |
| 常駐 | launchd（**ProcessType=Background 禁止**） | herdr プラグイン機構は常駐不可（hook=一発 spawn・64KB cap・supervision 無し）。Background 禁止は cm STATUS flap 事故の教訓 |
| AGPL 衛生 | herdr とは socket/CLI 越しのデータ交換のみ＝別プログラム（Go コードへ伝播なし） | **herdr ソース断片の vendor・コピペは恒久禁止**。fixtures は実行時出力のみ |

## herdr 側の一次事実（実機 v0.7.4 検証済み）

- `herdr terminal session observe <target> [--cols N] [--rows N]`（既定 120x40）:
  stdout に ndjson `{type:"terminal.frame",seq,encoding:"ansi",width,height,full,bytes:base64}` /
  `{type:"terminal.closed",reason}`。**全フレーム DECSET 2026 括り＋?25l** の
  server-rendered ANSI（BlitEncoder セル差分、full=全再描画 \x1b[2J\x1b[H、
  diff=100-500B、無変化時はフレーム自体出ない）。多重 observer 可・PTY 非影響。
- `herdr terminal session control <target> [--takeover]`: 上記＋stdin から ndjson
  `terminal.input{text|bytes}` / `terminal.resize{cols,rows}` / `terminal.scroll` /
  `terminal.release`。**接続時に実 PTY を resize しローカル resize lock**。
  writable owner は端末毎 1 つ（takeover で追放）。stdin EOF=Detach。
- target 解決: terminal_id 完全一致 → pane id（`w1:p1` 等）→ agent 名（曖昧は
  エラー）。terminal_id は `pane.list`/`agent.list` で取得。
- ndjson API socket（`HERDR_SOCKET_PATH` → 既定 `~/.config/herdr/herdr.sock`）:
  **1 接続=1 リクエスト**（実測 BrokenPipe）＝毎回再接続。events.subscribe のみ
  長寿命（接続維持は Phase 1 で実測・周期 poll backstop 常設）。
- scrollback はフレームに含まれない（viewport レンダのみ）。`terminal.scroll` は
  **共有 runtime 状態＝ローカル表示にも影響**（実測）→ v1 はリモート
  scrollback 非対応を明示制約とする。
- 認証なし（socket 0600 のみ）＝socket を直接公開しない。必ず agent dial-out
  ＋relay grant 経由（cm と同型）。

## データフロー

### 一覧同期
```
launchd 常駐 `herdr-drover agent`（pc id = <host>-herdr）
 → producer: pane.list/agent.list（events nudge＋周期 poll backstop）
 → session map（key=pane_id, short_dir=cwd 末尾, window_name=agent/label,
    is_active=AgentStatus）
 → state.PushStatus（content_hash ゲート＝near-$0）／消滅キー DeleteSession
 → Firestore pcs/{pc}/sessions/{pane_id} → 既存 Web 端末一覧
```
scan エラー tick は skip＝前回状態維持（cm の空 STATUS flap 教訓）。

### Web ターミナル
```
ブラウザ /term → relay(viewer) → wake/{pc} → agent WatchWake
 → PutRelayGrant(source,60s) → relay.Dial(source)
 → bridge: 初回 RESIZE magic 待ち → `herdr terminal session observe <pane_id>
    --cols C --rows R` spawn → frame envelope decode → ANSI bytes を WSS へ
逆方向: cmwire parser が RESIZE→observe respawn（新 full frame）／SCROLL→v1 無視
 ／IMAGE(0xff 0xfd)→u32 長ぶん **parse-and-drop 必須**（漏れると画像バイトが
 打鍵として pane に流れる）／その他→pane.send_input
無通信 30s → BridgeSourceIdle 自切断（quiescence）→ M9 push で自動復帰
```

### リモート pane 注入（↗窓相当）
```
reconcile: WatchSessions（他 PC）→ ローカル herdr へ
  agent.start(argv=["herdr-drover","attach","<pc>","<pane_id>"], name=表示名)
  ＋pane.report_metadata{source:"drover",pc,sid}
identity = metadata＋launch_argv の二重符号化（exact-match・非ヒューリスティック）
attach client: Wake→Grant→relay viewer（派生 sid <pane_id>#inj）
  frame→自 stdout(pane PTY)、stdin→WSS、SIGWINCH→RESIZE
  conn close は exit せず backoff 再接続（cm socket-client の「切断=窓死亡」欠陥を修正）
派生 sid は sessions コレクションに出さない（wake/grants ペアリング専用）
```
M8f2 教訓: desired/cur トレース・**定常 CREATE=0 を機械確認**・list 失敗を
「pane ゼロ」と誤認しない。

### 遠隔命令（既存 3 種に写像・ValidCommands は焼込みで追加不可）
- restart-agent → `launchctl kickstart -k`（自身）
- self-update → selfupdate.Update → 再起動
- restart-proxy → 当該 sid の bridge respawn（不能なら status=error で Ack＝滞留させない）
破壊的命令は Ack 先行（cm 規律）。

## リポジトリ構成

```
go.mod                        module github.com/4noha/herdr-drover（firestore/websocket/google-api のみ）
herdr-plugin.toml             プラグイン manifest（actions: install/status/nudge、events→nudge）
scripts/build.sh              [[build]] 実体
cmd/herdr-drover/             main/agent/attach/enroll/install（サブコマンド）
internal/herdrapi/            ndjson API client（1接続=1リクエスト）＋events 購読＋型
internal/bridge/              observe spawn＋cm-wire アダプタ（cmwire.go は cm framing 互換移植）
internal/session/             producer（pane→session map→Push/Delete）
internal/reconcile/           リモート pane 注入
internal/attach/              注入 pane 内 viewer client
internal/cloud/state/         cm からバイト同一コピー（自己完結・実エミュレータテスト付き）
internal/cloud/relayclient/   cm relay.Dial＋BridgeSourceIdle 相当のコピー
internal/selfupdate/          cm からコピー（stdlib のみ）
test/fixtures/                実 herdr の ndjson/フレーム録画（実行時出力のみ＝AGPL 衛生）
```

## 実装フェーズ（各ゲートは実環境・合成不可）

1. **一覧同期**: 実 herdr＋実 Firestore＋実 Chrome で herdr PC カード表示・
   pane 作成/終了の反映・cm agent と共存（削除合戦なし）を実証
2. **Web ターミナル**: 実 Chrome /term の display-oracle（echo 往復・ESC 系
   キー・quiescence→push 復帰・ローカル表示無影響）。入力決定木を実測確定
3. **リモート pane 注入**: 実 2 サーバ相当で出現/打鍵/消滅/再起動自己修復・
   定常 CREATE=0・Web と注入 pane の同時接続（派生 sid 実証）
4. **プラグイン化・enroll・遠隔命令・配布**: 実 plugin install→launchd 常駐・
   実 Web ボタン→Ack 監査・marketplace 掲載（topic herdr-plugin）

## 主要リスクと緩和

- **hidden CLI**（terminal session observe/control は --help 非掲載）: agent 起動時
  ping で version 実測・未検証 version は警告・version 毎 fixture・fallback 2 系
  （PTY 付き terminal attach／bincode 直叩き=タグ表解明済）
- pane.send_input の制御バイト透過性未確定 → Phase 2 実測で決定木確定
- 160×500 既定 viewport の padding/初回 full frame → 実ブラウザゲートで判定
  （bridge 行 clamp を knob として用意・事前最適化しない）
- herdr server 再起動時の plugin pane 再 spawn 挙動は調査間で矛盾 → reconcile
  は両挙動許容で設計し実再起動ゲートで確定
- wake/{pc} 単一 doc の近接同時 wake 上書き → viewer backoff 再試行（cm 同一・実害小）
- Windows out-of-scope（direct attach 非対応・named pipe 分岐要）

## 開発の鉄則（cm から継承）

1. 推測修正をしない — 実再現してから直す
2. 実テストで担保 — 実 herdr（隔離 HERDR_SOCKET_PATH・**短い /tmp パス**＝
   sun_path 104B 制約）・実エミュレータ・実録画 fixture。合成ストリームだけで
   緑にしない。修正前に旧コードでテストが落ちることを確認
3. ヒューリスティック分類をしない — exact-match の identity（metadata/argv 符号化）
4. herdr ソースの vendor 禁止（AGPL 衛生）
