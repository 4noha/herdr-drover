# 実装手順メモ / 引き継ぎ（2026-07-20 更新）

**開発の鉄則は [CLAUDE.md](CLAUDE.md) / DESIGN.md 末尾**（実テスト担保・旧コード
FAIL 確認・exact-match・AGPL 衛生・silent 変更禁止・対外操作はユーザー確認後）。
**再開時はまずこの TODO.md を通読**（in-flight・残課題・デプロイ手順の正）。

---

## 最新状態（2026-07-18〜20・稼働版 v0.5.0）

直近で以下を実装・リリース済み（詳細は各 DESIGN doc）。**全て darwin/linux
build 緑・テスト緑**。稼働 launchd `com.4noha.herdr-drover`（pc=`mac-studio-herdr`）
は **v0.5.0**。

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
  1. slave（n9htqcr6g0-herdr 等）で `herdr-drover update`（→v0.5.0。現在 v0.4.4）。
  2. owner で `~/.herdr-drover/bin/herdr-drover ssh-forward <slave> repoA`
     （SSH_AUTH_SOCK 稼働中）。
  3. slave で `SSH_AUTH_SOCK=~/.herdr-drover/agent-fwd/afwd-repoA.sock \
     git ls-remote git@github.com:<you>/repoA`（read-only probe→clone/pull）。
  4. owner 名義で認証されるか確認→本番は repo 限定 read-only deploy key を
     `ssh-add -c` へ差替。
  - ⚠owner は `herdr-drover` が PATH 未登録＝full path
    `~/.herdr-drover/bin/herdr-drover` で起動。

### B. Google IME + herdr で Ctrl+J/K/L/;/: がおかしい — 調査完了・修正保留

- **真因（確定）**: 外側端末は WezTerm、Google IME は CUSTOM keymap で
  Ctrl+J/K/L/;/: を **変換中(Composition/Conversion)だけ**に割当。未入力
  (Precomposition)/英数(DirectInput) には無い＝preedit が無い状態で押すと IME が
  握らず WezTerm が制御文字（Ctrl+J=改行 等）を claude へ送る。herdr は無実
  （prefix=Ctrl+A・ペア移動は prefix+j/k/l）。
- **修正案（保留）**: Google IME keymap の Precomposition に Ctrl+J/K/L/;/: の
  InputMode* を追加（IME が未入力でも握るように）。WezTerm 側は
  `macos_forward_to_ime_modifier_mask` に CTRL 含む（既定で OK のはず）。
  ⇒ ユーザーが「一旦おいて」保留にした課題。

### C. resume backstop（旧タスク・cm findLiveManagedByUUID の herdr 版・未着手）

`claude --resume <uuid>` の 2 回目が新プロセスを作る問題（同一会話 jsonl に
2 プロセス）。シムが `--resume <uuid>` で `pane.report_metadata` token に uuid を
記録→2 回目は token 一致 pane が生きていれば attach。詳細は下記「旧仕様メモ」。

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
  生存＝新版にしたければ `pkill -f 'herdr-drover attach'`→reconcile 再生成
  （ただし attach.go 無変更のリリースでは機能差ゼロ＝不要）。
- ⚠バイナリ/設定はプロセス起動時のみ反映＝各セッションは新規起動で新版。
- ⚠**リリースビルドは GOWORK=off**（go.work のローカル drover-cloud でなく go.mod
  宣言の公開タグで解決）。usage() は backtick raw string＝**中に `` ` `` を入れない**
  （文字列が途中で閉じてビルド破壊。v0.5.0 で実際にやらかして amend 修正）。

---

## 残バックログ（優先順）

1. **SSH 転送 Phase 3 実機 e2e**（上記 A・保留中＝ユーザー都合の良い時）。
2. **IME Ctrl キー**（上記 B・保留中）。
3. **resume backstop**（上記 C・未着手）。⚠claudeshim.go を触るので他タスクと衝突注意。
4. GitHub 公開: リポジトリ push＋topic `herdr-plugin`→marketplace 掲載
   （**対外操作＝ユーザー明示確認後**）。※herdr-drover は既に GitHub 公開済み
   （release 発行運用中）。marketplace topic 付与が残。
5. 複数クラウド実 GCP e2e: 2 つ目の GCP プロジェクト/SA 鍵が要る（要 Mac canonical）。
6. Windows 対応（out-of-scope 宣言済・将来。現状 wsmap/selfupdate が unix-only）。

---

## 環境の現状（2026-07-20 時点）

- herdr 0.7.4（brew）・既定サーバ稼働・plugin link 済（`4noha.drover`）。
- 稼働 launchd `com.4noha.herdr-drover`（pc=`mac-studio-herdr`・v0.5.0・
  クラウド `claude-master-4noha`/既デプロイ relay
  `wss://claude-master-relay-nkzxa3hxma-an.a.run.app`）。
- **enroll 済み PC**: mac-studio-herdr(owner/master)・d24wt27c3j-herdr(master)・
  **n9htqcr6g0-herdr(slave)**・**lph77xyyc7-herdr(slave)**。slave 2 台は現在 v0.4.4
  （remote self-update は slave 非到達＝各機で `herdr-drover update` 要）。
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
