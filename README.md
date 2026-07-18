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
- attach は `herdr terminal attach` へのプロセス置換（detach は herdr 標準の
  Ctrl+B q）。実 claude バイナリは `exec.LookPath("claude")` で絶対パス解決
  （shell alias に非依存）

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

## Status

Phase 1（一覧同期）・Phase 2（Web ターミナル）・Phase 4（プラグイン化・
遠隔命令・install/launchd・配布）実装済み。**Phase 3（リモート pane 注入＝
↗窓相当）は未実装**（DESIGN.md 参照・次期）。各フェーズは実 herdr 隔離
サーバ＋実 Firestore エミュレータ＋実 relay（cm 無改変 build）の常設 e2e
gate（`test/`）で検証している。実 launchd へのロード（カットオーバー）は
`herdr-drover install` を手動実行（テストは `--no-launchctl`＋隔離 HOME
のみ＝実環境不可侵）。

## Requirements

- macOS / Linux, herdr >= 0.7.4, Go 1.25+（ソースビルド時）
- GCP プロジェクト（Cloud Run relay + Firestore）— claude-master-go と同一の
  クラウド構成を共有可能
