# herdr-drover

**Drive your herdr sessions across machines.** — herdr のセッション群をクラウド
経由で複数 PC・ブラウザ・スマホへ「駆り立てる」standalone プラグイン。

[herdr](https://herdr.dev) は AI コーディングエージェント用のターミナル
マルチプレクサ。herdr-drover はそこに **クラウド同期**を足す:

- 🌍 **全 PC のセッション一覧を Web で**: 各 PC の herdr pane/agent 状態を
  Firestore で同期し、Google ログインの Web UI から一覧
- 📱 **ブラウザ/スマホからフル忠実ターミナル**: 任意のセッションへ Cloud Run
  relay (WSS) 越しに接続。herdr のサーバサイドレンダ差分フレーム
  （DECSET 2026 括り）をそのままストリーム
- 🪟 **リモート pane 注入**: 他 PC のセッションをローカル herdr に pane として
  自動出現（reconcile・自己修復）
- 💤 **near-$0**: 無通信 30s で自動切断、Firestore push で自動復帰。
  アイドル時のクラウド課金ほぼゼロ

## 仕組み（要点）

- herdr との接点は 2 本だけ: **ndjson API socket**（pane 列挙・入力・イベント）と
  **同梱 CLI サブプロセス** `herdr terminal session observe/control`
  （ヘッドレスなフレームストリーム）。バイナリ同梱 client を使うため
  herdr の wire プロトコルバージョン問題が構造的に発生しない
- herdr 本体のコード・設定は無改変。AGPL 衛生: herdr とはプロセス境界の
  データ交換のみ（ソース断片の vendor 禁止）

- クラウド層（relay/Web/Firestore サーバ ＋ `state`/`relayclient`/`selfupdate`
  の共有 Go ライブラリ）は独立リポジトリ **[drover-cloud](https://github.com/4noha/drover-cloud)** に
  切り出してある。herdr-drover はその共有ライブラリを import し、クラウドは
  drover-cloud を Cloud Run に 1 回デプロイしたものを全 PC で共有する

**構築手順は [SETUP.md](SETUP.md)**（PC 側の導入＋クラウド参加）。クラウドを
一から立てる手順は [drover-cloud/SETUP.md](https://github.com/4noha/drover-cloud/blob/main/SETUP.md)。
設計詳細は [DESIGN.md](DESIGN.md)。

## 使い方

### claude シム（cwd 自動 attach / 新規起動）

`herdr-drover claude [args...]` は claude-master `start` の C 案（自動 attach/
復帰）を herdr 世界で再現するシム:

- herdr server が居なければ detached 自動起動（ping 応答まで最大 10s 待ち）。
  これは**ユーザーの herdr server の代理起動**であって drover の管理下には
  置かない（drover は止めない・監督しない。停止は `herdr server stop`）。
  同時 2 シムの二重起動は herdr 自身の単一インスタンス制御に委ねる
  （実測 0.7.4: 2 本目は "already running" exit 1・socket 強奪なし）
- 引数なし: **cwd 完全一致**の既存 claude セッション（agent 名 `claude` /
  `claude-<数字>` の構造 exact-match のみ）へ attach。複数あれば番号 picker
  （Enter=先頭 / n か 0=新規 / 数字=指定）。無ければ新規起動。
  cwd は物理パスへ正規化（symlink 経路 `/tmp` 等でも dup を作らない）
- 新規は**常に新しい Tab（claude pane 1 枚）**として生まれる（既存 Tab を
  split して表示を邪魔しない）。tab label は cwd 末尾。focus は奪わない
- 着地先 workspace は `~/.herdr-drover/workspaces.json` のルールで解決
  （**exact cwd > 最長 prefix > default** の決定的解決・`~` 展開対応。
  ルール無しは現在 focused の workspace）。label の workspace が無ければ
  focus 非奪取で自動作成、label 重複時は number 最小を採用（決定的）。
  ファイルが壊れている場合は黙って無視せず**エラーで停止**する:

  ```json
  {
    "exact":   {"/abs/cwd": "label", "~/works/x": "label2"},
    "rules":   [{"prefix": "~/works", "workspace": "label3"}],
    "default": "label4"
  }
  ```
- agent 名は herdr の一意制約（実測 `agent_name_taken`）に合わせ
  `claude` → `claude-2` → … と自動採番
- 引数あり（TTY）: 常に新規 Tab（明示指定の尊重＝既存 attach で横取りしない）
- 引数あり（非 TTY）: herdr を経由せず**素の claude へプロセス置換**
  （`echo prompt | claude -p …` の pipe stdin/stdout 契約を透過）
- 引数なし×非 TTY（CI/パイプ）: attach せず pane_id/terminal_id を表示して
  exit 0（自動化スクリプトから呼ばれても dup セッションを作らない）
- 接続は**自動 min ローカルビューア**（`internal` 非依存の
  `cmd/herdr-drover/localview.go`）。単一 pane をメインアプリ（herdr TUI）と
  起動元端末の両方で下部まで見せるには、pane grid を**両者の小さい方**に
  合わせるしかない（単一 PTY は片方にしか厳密一致できない＝大きい側は余白）。
  herdr 0.7.4 の実挙動（ソース確定）に基づき自動で切り替える:
  - **起動元 ≥ grid**: `herdr terminal session observe`（`TerminalObserve`＝
    **ロック非取得**・観測側サイズへ仮想描画・pane 実サイズを変えない）。
    リサイズ権限をメインアプリに残す（メイン優先）。起動元が大きければ余白。
  - **起動元 < grid**: `herdr terminal session control`（`ControlTerminal`＝
    pane を起動元実寸へ resize＋`direct_attach_resize_locks` へ登録）で pane を
    縮小＋ロック＝**起動元で下部入力まで見える**。メインはその pane を余白付きで
    表示（ロックが有益な「メインが大きい側」だけロックを張る）。
  - 旧実装の常時 `herdr terminal attach` は起動元サイズに pane を固定し、逆に
    メインが小さいと下部が切れた（herdr 0.7.4 `src/ui/panes.rs`／
    `server/headless.rs` で確定・ユーザー実測で裏取り）。常時 observe だと起動元が
    小さいとき下部がクリップされた（実測）。自動 min はこの両方を解消する。

  grid 行は `pane.get` の `scroll.viewport_rows`（非ロック時に真のメインサイズ）。
  桁は API 非公開のため control には起動元端末の実桁を渡す（外部が両次元で
  小さい一般ケースは完全 fit）。キー入力は両モードとも ndjson API の
  `pane.send_text`（byte-perfect）で注入。detach は Ctrl+B q（末尾 Ctrl-B は次
  入力へ保留・Ctrl-B Ctrl-B でリテラル送出）。SIGWINCH で mode を再評価し
  respawn（observe 中は grid 行も再取得＝途中でメインをリサイズした場合の追随）。
  実 claude バイナリは `exec.LookPath("claude")` で絶対パス解決（shell alias 非依存）。
  ⚠残課題（稀）: control ロック中はメインの真サイズを読めず、ロック後にメインを
  起動元より小さく縮めるとメイン側が下部クリップし得る（detach で解消）。
  ⚠非 UTF-8 バイト（キーボードからは実質発生しない）は control fallback を使わず破棄

```sh
alias claude='~/.herdr-drover/bin/herdr-drover claude'
```

### organize / capture / live 学習（Tab 単位の Workspace 整理）

pane は「1 つの Tab の描画領域の分割」＝claude セッションの整理・学習の単位は
**Tab**。着地ルールは `~/.herdr-drover/workspaces.json`（`internal/wsmap`）に
持ち、**exact-cwd > 最長 prefix > default** で決定的に解決する（ヒューリス
ティック分類はしない）:

```sh
herdr-drover organize --dry-run    # 計画表示のみ（herdr/wsmap 無変更）
herdr-drover organize              # ルール解決先の Workspace へ Tab を整理
herdr-drover organize --capture --dry-run  # 現配置→exact ルールの差分表示
herdr-drover organize --capture    # 現配置を exact ルールとして保存
```

- **claude pane の同定は 2 系統 OR・どちらも exact-match**: (a) シム命名
  （agent 名 `claude` / `claude-N`） (b) herdr の検出種別 `agent == "claude"`
  （herdr UI から直接開いたセッションも取りこぼさない）。両者が矛盾する
  pane は機械確定不能＝対象外＋報告
- **移動は Tab の構成で決定的に分岐**（herdr 0.7.4 に別 workspace への
  Tab 移動 API は無く `pane.move` が唯一のプリミティブ＝実測）:
  claude **単独 Tab** は `pane.move new_tab` で Tab ごと移動（custom label
  引継ぎ・ソース Tab は自動 close）／**非 claude pane と同居**する Tab は
  claude pane だけを新 Tab へ**切り出し**（同居 pane を巻き込まない）／
  1 Tab に claude 複数などの曖昧は skip＋理由報告。実行結果（id 変化含む）は
  1 行ずつ報告（silent 禁止）
- **capture** は「claude cwd → その Tab の workspace label」を exact ルール
  として保存（書込前に差分表示・既存 exact のみ上書きで prefix/default は
  不変・同一 cwd が複数 workspace に散る場合は曖昧＝skip＋報告）
- **live 学習（opt-in・既定 off）**: `~/.herdr-drover/config.json` に
  `"learn_moves": true` を書くと、agent daemon が `pane.moved` を購読し、
  手動の Tab 移動（cross-workspace の claude pane 移動）を exact ルールへ
  自動反映する。herdr の event バックログ再送（実測仕様）は「購読前 pane
  配置 snapshot」と「ライブ状態」の 2 重 exact 照合で捨てる（誤学習しない・
  daemon 再起動でも削除済みルールを復活させない）。移動先 workspace の
  label が重複している場合はルール化不能として skip（capture と同一判定）。
  ルール書込・skip は必ず 1 行ログに残る。
  次に同じ場所で claude を開くと **Tab ごと**学習先 Workspace に生まれる

### mv-tab（Tab を別 Workspace へ丸ごと引っ越し）

herdr 0.7.4 の `tab.move` は同一 workspace 内 reorder 専用で、**別 workspace への
Tab 移動 API は無い**。`pane.move` を唯一のプリミティブとして丸ごと引っ越す
（単独 pane Tab は `pane.move new_tab` 一発／複数 pane Tab は `pane.layout` を採取して
`pane.move new_tab` → `pane.move tab` で残り pane を再構築＝連鎖近似）。terminal_id
はプラットフォーム API 経由で維持されるため走行中プロセスは無停止。

```sh
# CLI（対話ピッカ・TTY 必須）
herdr-drover mv-tab

# CLI（非対話）
herdr-drover mv-tab --src-tab w1:tD --dst-ws w3
herdr-drover mv-tab --self --dst-ws-label slave   # 起動プロセスの Tab を label 一致 WS へ

# plugin action（drawer から起動）
#   → launcher が新 Tab を layout.apply で開き、そこの TTY 内で対話ピッカを走らせる
```

- `--self` は herdr の `pane.current` API で自 pane を **exact 特定**（推測なし）。
  agent（Claude 等）が「このセッションを X に」と自然言語 1 発で指示するための口。
- `--dst-ws-label` は `workspace.list` の label exact 一致で解決。**label は重複可**
  （実測仕様）なので複数一致は明示エラー＝ `--dst-ws <workspace_id>` で再指定。
- 成功後は `workspace.focus` + `tab.focus` で受入先 WS/新 Tab へ自動フォーカス。

**Claude Code から使う場合の Skill**: リポ同梱の `skills/mv-tab/SKILL.md` を
Claude Code の skills ディレクトリに配置すると、「このセッションを slave に移動して」
のような自然言語で `--self --dst-ws-label` を自動起動できる:

```sh
mkdir -p ~/.claude/skills
ln -s "$PWD/skills/mv-tab" ~/.claude/skills/mv-tab
```

### 複数クラウド（端末ごとにマルチ Google アカウント）

1 台の PC が **複数の独立したクラウド**（別 Google アカウント＝別 GCP
プロジェクト/別 relay/別 SA 鍵）へ**同時接続**し、同じ herdr セッションを各
クラウドへ push・各々の relay でトンネル/コマンドを受けられる。クラウド側は
一切改変不要（PC 側 agent のみの機能）。

- **設定**: `~/.herdr-drover/clouds.json`（`[{project, relay_url, sa_key_path,
  pc_name?}]`）。**無ければ従来どおり env/config.json の単一クラウド**＝既存
  構成は挙動完全不変（後方互換）。
- **追加は enroll**: 2 つ目以降の Google アカウントを Web「＋ 端末を追加」→
  `herdr-drover enroll <code> --relay wss://…` すると、SA を `sa-<project>.json`
  （既存 sa.json を上書きしない）に置き、`clouds.json` へ追記する（既存クラウドは
  seed で保持・同 project は更新）。初回/同一クラウド再 enroll は従来どおり
  `sa.json`＋`config.json`＝byte 同一の後方互換。
- **認証の肝**: `GOOGLE_APPLICATION_CREDENTIALS` はプロセス global で 1 つしか
  持てないため、共有 lib の `state.NewWithCredentials`（`option.WithCredentialsFile`）で
  **クラウドごとに SA 鍵を個別注入**＝1 プロセスで複数 GCP プロジェクトへ同時接続。
- **fan-out**: agent がクラウドごとに goroutine（RegisterPC＋producer push＋
  遠隔命令＋Web ターミナル制御線）を回す。セッション源は共有＝同一セッションを
  全クラウドへ。次回 agent 再起動（`herdr-drover install` / kickstart）で反映。

## Status

Phase 1（一覧同期）・Phase 2（Web ターミナル）・**Phase 3（リモート pane 注入＝
↗窓相当）**・Phase 4（プラグイン化・遠隔命令・install/launchd・配布）実装済み。
各フェーズは実 herdr 隔離サーバ＋実 Firestore エミュレータ＋実 relay
（drover-cloud build）の常設 e2e gate（`test/`）で検証している。⚠ リモート pane
注入の実クラウド 2 PC 越しの完全 e2e（他 PC の実セッションが↗注入されて打鍵往復）は
2 つ目のクラウド/PC が要るため未実施（reconcile の pane 生成/冪等/自己修復/fail-safe は
実 herdr で機械検証済み・attach viewer の cm-wire は Phase 2 viewer と同形式）。実
launchd へのロード（カットオーバー）は `herdr-drover install` を手動実行（テストは
`--no-launchctl`＋隔離 HOME のみ＝実環境不可侵）。

## Requirements

- macOS / Linux, herdr >= 0.7.4, Go 1.25+（ソースビルド時）
- クラウド同期を使う場合: GCP プロジェクト（Cloud Run relay + Firestore）。
  クラウドサーバは独立リポジトリ **[drover-cloud](https://github.com/4noha/drover-cloud)**
  を 1 回デプロイして全 PC で共有する（一から立てる手順は
  [drover-cloud/SETUP.md](https://github.com/4noha/drover-cloud/blob/main/SETUP.md)）。
  既存クラウドに参加するだけなら GCP 操作は不要（Web「＋ 端末を追加」→ enroll）
