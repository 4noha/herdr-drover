# herdr-drover SETUP — PC を丸ごと構築する

このリポジトリの内容だけで、herdr の claude シム＋クラウド同期（Web/スマホ
閲覧・near-$0）を構築できる。クラウドサーバ自体を一から立てる手順は
共有リポジトリ [drover-cloud/SETUP.md](../drover-cloud/SETUP.md) にある
（クラウドは 1 つを全 PC で共有＝2 台目以降はサーバ操作不要）。

## 前提

- macOS / Linux（Windows は out-of-scope）
- **herdr >= 0.7.4**（`brew install herdr` 等）＋既定サーバが起動できること
- **claude**（Claude Code CLI）が PATH にあること
- ソースビルドする場合は **Go 1.25+**（Homebrew Go）

## 1. プラグインを入れる

### かんたん（ワンライナー）

```sh
curl -fsSL https://raw.githubusercontent.com/4noha/herdr-drover/main/install.sh | sh
```

Release 未発行の間は自動で **Go ソースビルドに fallback** して
`~/.local/bin/herdr-drover` を置く（PATH に無ければ案内が出る）。再実行で更新（冪等）。

### ソースから（開発）

```sh
git clone https://github.com/4noha/herdr-drover && cd herdr-drover
export PATH="/opt/homebrew/bin:$PATH"
go build ./... && go test ./...        # 検証（実 herdr 隔離サーバを使う）
sh scripts/build.sh                    # → bin/herdr-drover
```

> ⚠ 本リポジトリはクラウド層を共有リポジトリ **drover-cloud** に依存する。
> 開発中はローカルに両方を並べて（`../drover-cloud`）`go.mod` の
> `replace github.com/4noha/drover-cloud => ../drover-cloud` で解決する。

## 2. claude シムを有効化

```sh
alias claude='~/.herdr-drover/bin/herdr-drover claude'   # .zshrc/.bashrc へ
```

以後 `claude` は「cwd 一致の既存セッションへ自動 attach／無ければ新しい Tab で
起動」する（詳細は [README](README.md)）。表示は**自動 min ローカルビューア**
（起動元端末とメイン herdr のサイズ差で下部入力が切れない）。

### （推奨）herdr の event で即時同期

```sh
herdr plugin link      # herdr の pane イベントで agent を nudge（周期 poll の補完）
```

## 3. クラウドにつなぐ

### 既にクラウドがある場合（推奨・かんたん）

1. オーナーが Web UI（`https://<relay>/`）にログイン →「**＋ 端末を追加**」で
   一回限りコードを発行
2. この PC で:

   ```sh
   herdr-drover enroll <code> --relay wss://<relay-host>
   ```

   → `~/.herdr-drover/sa.json`（SA 鍵・600）と `~/.herdr-drover/config.json`
   （`gcp_project` / `cloud_relay_url` / `google_application_credentials`）が自動配置される
3. 常駐させる:

   ```sh
   herdr-drover install       # launchd 常駐（--dry-run / --no-launchctl 可）
   herdr-drover status        # daemon 生存・herdr 接続・設定の充足を確認
   ```

これで herdr のセッションが Web 端末一覧に出る。以後 `claude` を使うだけで同期される。

### クラウドをまだ持っていない場合

先に [drover-cloud/SETUP.md](../drover-cloud/SETUP.md) で GCP に relay/Firestore/
Web を立て、その `wss://…` URL とオーナーの Google ログインを用意してから、
上の enroll に進む。

## 設定の解決順

**env > `~/.herdr-drover/config`（KEY=VALUE・手動）> `~/.herdr-drover/config.json`
（enroll が書く）> 既定**。

| キー | 用途 |
|---|---|
| `GCP_PROJECT` / `gcp_project` | Firestore プロジェクト（agent 必須） |
| `CLOUD_RELAY_URL` / `cloud_relay_url` | Cloud Run relay の **wss://** URL（Web ターミナルに必須） |
| `GOOGLE_APPLICATION_CREDENTIALS` / `google_application_credentials` | SA 鍵パス（`~/.herdr-drover/sa.json`） |
| `PC_ID` / `pc_id` | 端末 id（既定 `<hostname 短縮小文字>-herdr`）。⚠ **cm agent と同一 id 禁止**（`-herdr` を必ず付ける＝DeleteSession 削除合戦の回避） |
| `HERDR_SOCKET_PATH` | herdr ndjson socket（既定 `~/.config/herdr/herdr.sock`） |
| `DROVER_TICK` / `DROVER_IDLE` | producer 周期（既定 5s）／Web quiescence 自切断（既定 30s） |

## 4. Workspace 整理（任意）

claude セッションを Tab 単位で Workspace へ振り分けるルール
（`~/.herdr-drover/workspaces.json`）と `organize`/`--capture`/live 学習がある。
使い方は [README](README.md) の「organize / capture / live 学習」節を参照。

## 更新・撤去

```sh
herdr-drover update        # GitHub Releases 自己更新（sha256 検証・原子置換）
herdr-drover uninstall     # launchd 常駐解除（plist・稼働バイナリ除去。設定/ログは残す）
```

> ⚠ バイナリ/設定はプロセス起動時のみ反映。更新後は claude セッションの再起動
> （新しい `claude` 起動）で新版になる。daemon は `herdr-drover install` 再実行 or
> launchd kickstart で反映。
