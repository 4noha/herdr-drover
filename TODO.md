# 実装手順メモ（2026-07-18 セッション引き継ぎ）

前セッションのリミット到達に備えた引き継ぎ。**開発の鉄則は DESIGN.md 末尾**
（実テスト担保・旧コード FAIL 確認・exact-match・AGPL 衛生・silent 変更禁止）。

## 0. 現在の in-flight 状態（最優先で確認）

**Tab 単位着地ルールのマルチエージェント実装が実行中だった**
（run wf_ebb19788-ca7。セッション終了と共に停止するが、**エージェントは
working tree に直接書くため成果物は残る**）:

- journal（各エージェントの完了報告・実測 findings 20 件）:
  `~/.claude/projects/-Users-4noha-works-tools-claude-master-go/e55a448f-d437-4ef1-957d-a163641ee435/subagents/workflows/wf_ebb19788-ca7/journal.jsonl`
- 完了済み: ①Probe（tab 生成/移動 API・tab.move 前後の id/terminal_id・
  イベント種別・**herdr 直接起動 claude の同定手段**を実測確定・blocker 0）
  ②Build-A: `internal/wsmap/`（ルール解決）＋ claudeshim の**新 Tab 着地**化
- 実行中だった: Build-B: `cmd/herdr-drover/organize.go`（organize/--capture/
  learn_moves）
- 未着手: 統合（wsmap⇔organize 契約解消・`test/wsrules_e2e_test.go` の
  往復ループ e2e）→ 2 レンズレビュー → 修正

**再開手順**: `git status` で着地物を確認 → `go build ./... && go vet ./... &&
go test ./... -count=1`（実 herdr 必要・隔離レシピは DESIGN.md）→ 壊れて
いれば journal の完了報告と突き合わせて統合を手動完遂。organize が未着地
なら仕様は下記「1.」の通り新規実装。

## 1. Tab 単位着地ルール（仕様の正・ユーザー確定 UX）

- **claude 新規起動は常に新しい Tab**（claude pane 1 枚）。既存 Tab を
  split しない（ルール無しでも常時有効）
- 着地先 Workspace は `~/.herdr-drover/workspaces.json` で解決:
  `{"exact": {"/abs/cwd": "label"}, "rules": [{"prefix","workspace"}],
  "default": "..."}`＝ **exact > 最長 prefix > default > 無し（現 WS）** の決定的解決
- claude pane の同定は 2 系統 OR・どちらも exact:
  (a) シム命名 `claude`/`claude-N` (b) herdr 検出種別=claude（Probe findings
  参照・name=None の herdr 直接起動セッションも対象にする）
- `herdr-drover organize`: ズレ Tab を移動。**単独 Tab→tab.move／同居 Tab→
  pane move --new-tab で切り出し**（同居 pane を巻き込まない・旧シム間借りの
  正規化を兼ねる）／曖昧（1 Tab に別 cwd claude 複数等）→skip＋報告。--dry-run 必須
- `organize --capture`: 現配置→exact ルール上書き（同 cwd が複数 WS に散る
  場合は skip＋報告）。--dry-run 対応・書込前差分表示
- `learn_moves=true`（config.json・既定 false）: daemon が tab 移動イベントを
  購読し exact ルール自動反映（backlog 再送の dedup 必須・1 行ログ必須）
- e2e: ルール→新 Tab 着地（既存 tab の pane 数不変）→手動 tab.move→capture→
  同 cwd 2 本目が学習先に着地→organize --dry-run「移動不要」、の完全往復

## 2. デプロイ手順（毎回同じ・cm 教訓の rm→cp 新 inode）

```sh
cd ~/works/tools/herdr-drover
gofmt -l . | grep -v state.go   # state.go は cm バイト同一コピー＝整形しない
go build ./... && go vet ./... && go test ./... -count=1
git add -A && git commit  # 日本語コミットメッセージ・Co-Authored-By: Claude
sh scripts/build.sh       # plugin bin/
rm ~/.herdr-drover/bin/herdr-drover && cp bin/herdr-drover ~/.herdr-drover/bin/
launchctl kickstart -k gui/$(id -u)/com.4noha.herdr-drover
```

デプロイ後: `herdr-drover organize --dry-run` → 計画確認 → `organize` で
既存の間借り pane（旧シム産）を正規化するようユーザーに案内。

## 3. 次タスク: resume backstop（ユーザー要望・cm findLiveManagedByUUID の herdr 版）

`claude --resume <uuid>` の 2 回目が新プロセスを作る問題（同一会話 jsonl に
2 プロセス＝cm で実害のあった dup と同型）:

- シムが `--resume <uuid>` 起動時に `pane.report_metadata` の **token** へ
  uuid を記録（`PaneInfo.Tokens`・exact-match 可能を実測済み）
- 2 回目の `--resume <uuid>`: token 一致 pane が生きていれば **attach**・
  無ければ新規。「args 非空→常に新規」の例外は --resume のみ（他フラグは従来）
- 上位互換の可能性: `herdr integration install claude` の session identity
  報告が API から読めるなら権威にする（新規会話の uuid も追える・claude 内
  会話切替の stale token も解消）。Probe findings（journal）に手掛かりあり
- 注意: Tab 版が claudeshim.go を触っているため**必ず Tab 版着地後に実装**

## 4. 残バックログ（優先順）

1. Phase 3: リモート pane 注入（↗窓相当）。設計は DESIGN.md「リモート pane
   注入」節（派生 sid `<pane_id>#inj`・reconcile・M8f2 教訓）
2. GitHub 公開: リポジトリ push＋topic `herdr-plugin` 付与→marketplace 自動
   掲載（**対外操作＝ユーザー明示確認後**）
3. 実 Chrome 目視: Web /term の clamp 既定 120×320 の最終判定
   （DESIGN.md リスク節・ゲート未通過のまま）
4. cm 世界の店じまい: `com.4noha.claude-master`（monitor）と `-cloud` の
   launchd 2 本が並走中。herdr 常用が安定したら停止・掃除（pc id 分離済み
   なので急がない）。`.zshrc` の旧 cm alias はコメントで残置済（ロールバック用）
5. Windows 対応（out-of-scope 宣言済・将来）

## 5. 環境の現状（2026-07-18 時点）

- herdr 0.7.4（brew）・既定サーバ稼働・plugin link 済（`4noha.drover`）
- launchd `com.4noha.herdr-drover` 常駐（pc id `mac-studio-herdr`・
  クラウドは cm と共有＝`claude-master-4noha`/既デプロイ relay）
- `claude` alias は drover シム（`~/.herdr-drover/bin/herdr-drover claude`）
- コミット: c52df00→0d2f3a7(P1)→3c5d97b(P2)→3f99c76(P4)→9cb6f4b(シム)
- herdr ソースクローン（v0.7.4・AGPL＝vendor 禁止・参照のみ）:
  scratchpad の `herdr/`（消えていたら `git clone https://github.com/ogulcancelik/herdr && git checkout v0.7.4`）
