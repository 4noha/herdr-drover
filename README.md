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

## Status

Phase 1（セッション一覧のクラウド同期）実装中。

## Requirements

- macOS / Linux, herdr >= 0.7.4, Go 1.25+（ソースビルド時）
- GCP プロジェクト（Cloud Run relay + Firestore）— claude-master-go と同一の
  クラウド構成を共有可能
