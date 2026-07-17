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
  （Enter=先頭 / n か 0=新規 / 数字=指定）。無ければ新規 pane で起動。
  cwd は物理パスへ正規化（symlink 経路 `/tmp` 等でも dup を作らない）
- agent 名は herdr の一意制約（実測 `agent_name_taken`）に合わせ
  `claude` → `claude-2` → … と自動採番
- 引数あり（TTY）: 常に新規 pane（明示指定の尊重＝既存 attach で横取りしない）
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
