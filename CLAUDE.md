# herdr-drover プロジェクト

herdr（AI エージェント用ターミナルマルチプレクサ）の **standalone プラグイン**。
herdr のセッション群にクラウド同期（Web/スマホ閲覧・リモート pane 注入・
near-$0）を足し、`claude` シムで cwd 自動 attach／新 Tab 起動を提供する。
Python 版 claude-master(cm) のクラウド同期を **herdr の世界**へ移した後継。

- 導入手順: [SETUP.md](SETUP.md)／使い方: [README.md](README.md)／
  設計: [DESIGN.md](DESIGN.md)／**進行中作業の正: [TODO.md](TODO.md)**
- **再開時はまず TODO.md を読む**（in-flight タスク・残課題・デプロイ手順の正）

## アーキテクチャ（要点）

- herdr との接点は 2 本だけ: **ndjson API socket**（pane 列挙・入力・イベント）と
  同梱 CLI サブプロセス **`herdr terminal session observe/control`**
  （ヘッドレス frame ストリーム）。同一バイナリの client を使う＝wire
  PROTOCOL_VERSION 問題が構造的に不在。
- **クラウド層は外部 module `github.com/4noha/drover-cloud` へ切り出し済**
  （`state`/`relayclient`/`selfupdate` を import）。以前は cm からのバイト同一
  コピーを `internal/cloud/` に二重管理していた＝解消。開発中は go.mod の
  `replace github.com/4noha/drover-cloud => ../drover-cloud` でローカル解決。
  relay/Web サーバ本体も drover-cloud にある（Cloud Run へ 1 回デプロイ＝全 PC 共有）。
- `claude` シム: cwd 一致 attach／複数は picker／無しは新しい Tab。表示は
  **自動 min ローカルビューア**（`cmd/herdr-drover/localview.go`）＝起動元端末が
  grid より小さければ `terminal session control` で pane を縮小＋ロック（下部
  入力まで見える）、大きければ `observe`（ロック非取得＝メイン優先）。入力は
  両モードとも `pane.send_text`。

## 開発の鉄則（cm から継承・DESIGN.md 末尾）

1. **推測修正をしない** — 「動かない」はまず実再現してから直す
2. **実テストで担保** — 実 herdr（隔離 `HERDR_SOCKET_PATH`・**短い /tmp パス**＝
   `sun_path` 104B 制約）・実 Firestore エミュレータ・実録画 fixture。合成
   ストリームだけで緑にしない。**修正前に旧コードでテストが落ちることを確認**
3. **ヒューリスティック分類をしない** — exact-match の identity（metadata/argv 符号化）
4. **herdr ソースの vendor 禁止**（AGPL 衛生＝プロセス境界のデータ交換のみ）
5. **silent なコード/設定変更をしない**（丸め・skip は必ずログ）

## 絶対の禁則（過去の事故で学習）

- **cm リポジトリ（`~/works/tools/claude-master-go`）を改変しない**（読むだけ）。
  稼働中の Cloud Run relay も無停止で保つ。
- **裸の `pkill herdr` をしない**（ユーザーの実サーバを殺した事故あり）。自分が
  spawn した PID だけを対象にする。
- テストで**実 `~/Library/LaunchAgents` を触らない**（隔離 HOME＋`--no-launchctl`）。
- **pc id は必ず `<host>-herdr`**（cm agent と同一 id は Firestore DeleteSession
  の削除合戦になる）。SA 鍵 `~/.herdr-drover/sa.json` は 600・非コミット。
- 対外操作（GitHub 公開・実クラウドデプロイ等）は**ユーザー明示確認後**。

## herdr 0.7.4 の実測 trap

- ndjson API は **1 接続=1 リクエスト**（毎回再接続。events.subscribe のみ長寿命）。
- 入力は `pane.send_input`(text) だと \r が落ちる＝**`pane.send_text`**（\r 込み）
  か `pane.send_keys` を使う。
- `terminal session control` は**隠し CLI**（`--help` 非掲載。`--cols/--rows/
  --takeover`）。ControlTerminal＝attach と同じ **pane resize＋lock**。
  `observe` はロック非取得・観測側サイズへ仮想描画（観測 < grid は上寄せクリップ）。
- pane grid 桁は API 非公開（`pane.get` の `scroll.viewport_rows`=行のみ）。
- `tab.move` は同一 WS の reorder 専用＝WS 間移動は `pane.move` が唯一。
- workspace label は重複可＝識別子にしない（workspace_id を使う）。

## ビルド / テスト / デプロイ

```sh
export PATH="/opt/homebrew/bin:$PATH"          # Homebrew Go
gofmt -l . && go build ./... && go vet ./... && go test ./... -count=1
# デプロイ（cm 教訓の rm→cp 新 inode。上書き cp は macOS 署名キャッシュで SIGKILL）
sh scripts/build.sh
rm ~/.herdr-drover/bin/herdr-drover && cp bin/herdr-drover ~/.herdr-drover/bin/
launchctl kickstart -k gui/$(id -u)/com.4noha.herdr-drover
```

⚠ バイナリ/設定はプロセス起動時のみ反映＝proxy/claude セッションは新規起動で新版。
