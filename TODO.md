# 実装手順メモ（2026-07-18 更新）

**開発の鉄則は DESIGN.md 末尾**（実テスト担保・旧コード FAIL 確認・
exact-match・AGPL 衛生・silent 変更禁止）。

## ✅ 完了: 自動 min ローカルビューア（2026-07-18・デプロイ済・未 commit）

「起動元端末とメインアプリで縦幅が違うと片方の下部（claude 入力）が見えない」
問題の根治。単一 pane を2ビューアで同時に下部まで見せるには pane grid を
両者の min にするしかない（単一 PTY の構造制約・大きい側は余白）。

**根因（コード＋実測二重確認）**: ①旧 `herdr terminal attach` は pane を
`direct_attach_resize_locks` に固定＝メイン < 起動元だとメイン下部が切れた。
②その修正で常時 `observe` にしたら、観測 < grid のとき herdr は grid 上端から
観測行ぶんを描く**上寄せクリップ**（`render` の `while y<area.height &&
rows.next()`・実 pane rows=20 observe で入力ボックスが範囲外を実測）で、今度は
起動元 < メインのとき起動元下部が切れた。scrollback は履歴=上方向のみで到達不能。

**修正（`cmd/herdr-drover/localview.go` 自動 min）**: 起動時に `pane.get` の
`scroll.viewport_rows`（=grid 行）を threshold に取り、`pickStreamMode`:
- `localRows >= gridRows` → `terminal session observe`（ロック非取得＝メイン優先・
  起動元は余白）。※現行挙動を保持。
- `localRows <  gridRows` → 隠し CLI `terminal session control --cols C --rows R`
  （ControlTerminal＝attach と同じロック経路）で pane を起動元実寸へ縮小＋ロック
  ＝起動元で下部まで見える／メインは pane を余白付き表示。**ロックが有益な
  「メインが大きい側」だけロックを張る**。control は observe と同一 terminal.frame
  ＝表示コード共通。stdin EOF で自動 Detach するので stdin パイプを開いたまま保持。
  入力は両モードとも `pane.send_text`（ロック非依存）。SIGWINCH で mode 再評価
  ＋observe 中は grid 行を再取得（メインを途中リサイズした場合の threshold 追随）。

全10 pkg 緑・実 herdr 隔離サーバで **control が pane grid を local へ実縮小＋
ロック**（`TestLocalViewControlShrinksGridWhenLocalSmaller`）／observe が
viewport_rows 不変（`TestLocalViewObserveFramesAndLockFree`）／`pickStreamMode`
純関数を機械確認。**残**: ①実 raw-mode TTY の full e2e はユーザー実端末で確認
（pty ハーネス未自動化）②ユーザー確認後に git commit（デプロイ済 010b636-dirty）
③既知の残課題（ユーザー承認済み「稀な動的リサイズ」）: control ロック中は
pane.get が自分のロックサイズを返しメイン真サイズを読めない＝ロック後にメインを
起動元より小さく縮めるとメイン下部が切れ得る（detach で解消）／幅方向 min は
grid 桁が API 非公開のため未対応（control は起動元実桁を渡す＝混在次元のみ余白/
クリップ）。詳細は memory `herdr-drover-project.md`。

## ✅ 完了: Tab 単位着地ルール（d63aca0・デプロイ済み 2026-07-18 10:21）

下記 0-1 節の実装は完遂・全10 pkg 緑・レビュー serious 4 件修正済み・
launchd 再起動済み。**次のタスクは「3. resume backstop」**（設計そのまま有効。
Probe の一次事実: pane.move で pane_id は変わるが terminal_id 安定・旧 pane_id
は alias 解決・tab.move は同一 WS reorder 専用＝WS 間は pane.move が唯一）。
ユーザー向け案内済み事項: organize --dry-run → organize で旧・間借り pane の
正規化を推奨。

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

## 3.5 次々タスク: 同居 Tab の丸ごと引っ越し（ユーザー承認済みの合成操作）

organize の同居 Tab 処理を「claude だけ切り出し」から「**Tab 丸ごと引っ越し**」へ
格上げする（ユーザー提案・実測事実で成立確認済み）:

- 合成手順: ①`pane.layout`（ndjson 実在確認済み）で元 Tab の分割トポロジーを
  読む ②先頭 pane を `pane.move {type:new_tab, workspace_id, label:元タブ名}` で
  移す ③残り pane を tree 順に `pane.move {type:tab, tab_id, target_pane_id,
  split, ratio}` で再構築 ④空になった元 Tab は自動 close（実測済み）
- 根拠（実測済み）: pane.move の tab 宛先形態・ソース tab の空自動 close・
  terminal_id/agent 名/セッションの移動跨ぎ維持
- 実装時の確認: pane.layout 応答の ratio 有無（無ければ均等割で妥協し明記）・
  途中失敗時の半端状態の報告（非トランザクション＝1 pane ごとに検証・
  失敗は loud に報告して停止）
- 曖昧ケース（1 Tab に別 cwd の claude 複数）は従来どおり skip＋報告

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
