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
- 🪟 **リモート pane 注入（↗窓）**: 他 PC のセッションをローカル herdr に pane
  として自動出現（reconcile・自己修復）。双方向に打鍵できる
- 🤝 **共用 PC（slave）対応**: 1 アカウントを複数人で使う PC でも owner の私物
  セッションが漏れないよう、制限クレデンシャルで動く slave モード
- 🔑 **一時 SSH エージェント転送**: owner の SSH 鍵を slave のディスクに置かず、
  relay 越しに一時的に貸して GitHub 操作（署名は owner 側で実行）
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
設計詳細は [DESIGN.md](DESIGN.md)、**機能・インターフェース仕様は
[SPEC.md](SPEC.md)**、**他のコーディングエージェント対応は
[DESIGN_MULTI_AGENT.md](DESIGN_MULTI_AGENT.md)**。

## 対応エージェント

**検出（pane にどのエージェントが居るか）は herdr がネイティブに 21 種すべてを
見る**ので、Web/スマホ閲覧・↗窓 注入・organize は最初から全種で動く。差が出るのは
**「drover が argv を組み立て直す機能」**（resume / 更新 / モデル切替 / シムからの
新規起動）で、これは `internal/agentid` の Spec を持つ種別だけが対象になる。

### 実機で検証済み（5 種）

| | 会話 resume | restart<br>-agent-session | update<br>-agent-cli | `--model` | シムから<br>新規起動 |
|---|---|---|---|---|---|
| **claude** | `--resume <id>`（`-r`） | ✅ | ✅ `claude update` | ✅ | ✅ |
| **codex** | `codex resume <id>` | ✅ | ✅ `codex update` | ✅（`-m` も） | ✅ |
| **cursor** | `--resume <id>`<br>（実行名 `cursor-agent`） | ✅ | ✅ `cursor-agent update` | ✅ | ✅ |
| **copilot** | `--resume=<id>`（`-r`） | ✅ | ✅ `copilot update` | ✅ | ✅ |
| **devin** | `--resume <id>`（`-r`） | ✅ | ⚠ **版取得のみ** | ✅ | ✅ |

⚠ **devin の自己更新は意図的に載せていない。** `devin update` は存在するが
**非対話で完走しない**（stdin を閉じると rc=130）うえ Homebrew cask 管理と食い違う。
更新は `brew upgrade --cask devin-cli` を人が行い、drover は**セッション再起動だけ**
担当する。`update-agent-cli devin` は版を報告して再起動のみ行う。

#### 実測した差（会話の再開まわり）

| 事項 | claude | codex | cursor | copilot | devin |
|---|---|---|---|---|---|
| `agent_session` の発火契機 | 起動時 | 初回発話時 | 初回発話時<br>（要 trust 通過） | 初回発話時まで<br>には付与 | 初回発話時まで<br>には付与 |
| 会話 ref の形 | uuid v4 | uuid v7 系 | uuid v4 | uuid v4 | **単語スラッグ**<br>（例 `resolute-lynx`） |
| resume で会話復元 | ✅ | ✅ | ✅ | ✅ | ✅ |
| resume 後の hook 再発火 | ✅ | ❌ | ❌ | ❌ | ✅ |

⚠ **再発火しないのが多数派（5 種中 3 種）**。「resume 後の hook 再発火」が ❌ の
種別は、**同じ pane を 2 回目に restart すると素起動になる**（会話が失われる）。
1 回目の restart で `agent_session` が消えるためで、herdr / 各エージェント側の性質
＝drover では埋められない。**`--dry-run` で `--resume <ref>` が出るか確認してから
実行する**のが安全（ref が出なければ素起動になる）。

⚠ **会話 ref は UUID とは限らない**（devin は単語スラッグ）。drover は値の書式で
判定せず「非空・512B 以下・制御文字なし」しか見ない＝**書式ヒューリスティックを
入れてはいけない**。

### Spec はあるが実機未検証（9 種）

resume の argv 形だけ herdr のソースから写経してある。更新 / モデル切替 / シムからの
新規起動は**推測で書かない方針**のため未対応（実 CLI を入れて実測すれば足せる）。

| resume の形 | 該当 |
|---|---|
| `--resume <id>` | droid / hermes / qodercli |
| `--session <id>` | kimi / opencode / kilo / pi（`path` kind も） |
| `--thread <id>` | mastracode |
| `--resume=<id>` | omp（`-r`・`path` kind も） |

### resume が原理的に不可能（7 種）

**agy / amp / cline / gemini / grok / kiro / maki** — herdr が `agent_session` を
出さないため、会話 ref が存在しない。restart は**素起動へ落として loud に報告する**
（黙って会話を失わない）。検出・閲覧・↗窓 注入は他と同じように効く。

### 共通の前提

- ⚠ **resume には herdr の integration hook が必須**。`agent_session` は herdr が
  自力で見つけるのではなく**各エージェントの hook が報告する**。
  `herdr integration install <agent>` で設置し、`herdr integration status` で確認する。
  **未設置だと検出はされるのに resume だけ永久に効かない**。
- ⚠ **新しいエージェントを足すのは `internal/agentid/spec.go` に Spec を書くだけ**。
  ただし `InstallSpec.BinNames` は **herdr の `lookup_agent` alias 表の要素**でなければ
  ならない（表に無い名前で起動すると herdr の検出に一切載らない）。`ValidateSpecs()`
  が起動時に静的検証する。
- ⚠ **モデル名は種別ごとに互換でない**（claude=`opus` / codex=`gpt-5` /
  cursor=`sonnet-4-thinking` / devin=`claude-sonnet-4`）。`--model` は `--agent` と
  併用する。

## 使い方

### エージェントシム（cwd 自動 attach / 新規起動）

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

別のエージェント（codex 等）も同じ仕組みで使えます。**alias は exec に効かない**
ので、シムを起動する側から見て名前が一致している必要があります:

```sh
# 方法 1: canonical label の名前で symlink（argv[0] multi-call）
ln -s ~/.herdr-drover/bin/herdr-drover ~/bin/codex   # PATH の前方に置く

# 方法 2: 明示形（symlink を張れない場合）
herdr-drover shim codex
```

種別を跨いだ attach はしません（`codex` シムが claude セッションに繋がることは
ない）。本体バイナリの解決は**新規起動が要ると分かってから**行うので、ローカルに
未導入のエージェントでも既存セッションへ attach できます。

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

### update-agent-cli（エージェント本体の更新 → セッション反映をワンコマンドで）

```sh
# エージェントを最新にして、そのままこの PC のセッションへ反映
herdr-drover update-agent-cli          # 旧名 update-claude も使えます

# 何が起きるか確認（実行しない）／1 枚だけ／作業中も強制
herdr-drover update-agent-cli --dry-run
herdr-drover update-agent-cli w1:pD
herdr-drover update-agent-cli --force

# 種別を指定（省略時は更新口を持つ既定＝claude）
herdr-drover update-agent-cli --agent claude
```

Web からは端末カードの「更新」ボタン。他 PC・slave にも届く。

- `claude update` は symlink を差し替えるだけで**走っているセッションには効かない**、
  逆に再起動だけでは**ディスクが古いままなら何も新しくならない**。この 2 段を 1
  コマンドに閉じたもの。中身は `claude update` → 下記 restart-agent-session。
- **更新が無くても再起動する**。「ディスクは最新だがセッションは旧版」がまさに
  直したい状態なので、そこで止まらない。
- 更新対象のバイナリは稼働中セッションの起動パスから決める（PATH は使わない）。
  根拠を毎回出力し、種類が食い違う場合は推測せずエラーにする。
- 更新に失敗した場合はセッションを触らない（古いまま作り直しても無意味なため）。

⚠ `herdr-drover update` は **herdr-drover 自身**の更新です（エージェント本体は
`update-agent-cli`）。

### restart-agent-session（エージェントのバイナリ更新をセッションへ反映）

エージェント本体を更新しても、**すでに起動しているセッションは古いバイナリのまま**
動く（`~/.local/bin/claude` は `versions/<ver>` への symlink＝プロセスは exec した
時点の実体に貼り付く）。`restart-agent-session` は pane を**会話を引き継いだまま**
作り直し、新しいバイナリを掴ませる。

```sh
# 何が対象になるか確認（実行しない）
herdr-drover restart-agent-session --dry-run   # 旧名 restart-claude も使えます

# この PC のローカルセッションを全部（作業中は自動 skip）
herdr-drover restart-agent-session

# 1 枚だけ／作業中でも強制
herdr-drover restart-agent-session w1:pD
herdr-drover restart-agent-session --force w1:pD

# 種別で絞る（省略時は全種別）
herdr-drover restart-agent-session --agent claude

# モデルを切り替える（例: 既存の会話も Opus へ）
herdr-drover restart-agent-session --model opus
```

会話の引き継ぎ方はエージェントごとに違います（`--resume` / `--session` /
`--thread` / `--resume=` / codex は位置引数）。herdr が会話 ref を出さない 7 種
（agy・amp・cline・gemini・grok・kiro・maki）は**原理的に再開できない**ため、
会話なしで起動し直したうえでその旨を明示します（黙って会話を失いません）。

また、`zsh -lc '… claude'` のように**ラッパー経由で起動された pane は触りません**
（resume 引数を足してもエージェント本体に届かないため）。

⚠ **`--resume` した会話は settings.json の既定モデルを無視して、その会話に紐づいた
モデルのまま動きます**（実測）。既存の会話のモデルを変えるには `--model` が要ります。
`--model` は起動時の指定なので、セッション内で `/model` を使えば従来どおり変更できます。

Web からはセッション行とターミナル画面の「⟳」ボタン（1 枚）で同じことができる
（遠隔命令 `restart-agent-session`）。ボタンの文言はそのセッションのエージェント名に
変わります。他 PC・slave にも届く。

- **会話は失われない**: herdr が持つ会話 uuid を `--resume <uuid>` として渡し直す。
  Tab の位置・ラベル・agent 名もそのまま保つ。
- **会話が復元できないときも pane は残る**: 会話ファイルが既に消えている等で
  `--resume` 起動が即終了した場合は、`--resume` を外して新規会話として立て直す
  （その旨を出力・履歴に残す）。pane や Tab が消えたまま終わることはない。
- **作業中（agent_status=working）は既定でスキップ**。実行中タスクを巻き込まない
  （`--force` で強制。skip した理由は必ず出力／遠隔命令なら履歴の detail に残る）。
- **↗窓 の注入 pane は対象外**（リモートの鏡なので pane token で構造的に除外）。
  同居 pane のある Tab も巻き添えを避けて skip する。
- 起動コマンドは herdr が保持する実 argv をそのまま再利用する（PATH を引き直さない）
  ＝daemon 経由でも手元の CLI と同じバイナリが起動する。

### ssh-forward（slave へ owner の SSH エージェントを一時転送）

共用 slave 上で **owner の SSH 秘密鍵をディスクに置かず**、一時的に git/gh の
SSH 認証を通す。owner の `ssh-agent` を drover-cloud relay 越しに slave へ転送し、
**署名は owner Mac が実行**する（classic SSH agent forwarding を NAT 越しの relay に
載せたもの）。用途は「同じリポジトリをローカルと slave 両方で検証」等
（エージェント対エージェント）。設計は [DESIGN_SSH_FORWARD.md](DESIGN_SSH_FORWARD.md)。

```sh
# owner: 専用 deploy key を confirm 付きで登録（毎署名 owner Mac で承認）
ssh-add -c ~/.ssh/repoA_deploy

# owner: 転送ウィンドウを開く（Ctrl-C で撤去）
herdr-drover ssh-forward n9htqcr6g0-herdr repoA
#   → slave sock: ~/.herdr-drover/agent-fwd/afwd-repoA.sock

# slave: 表示された sock を SSH_AUTH_SOCK に指定して git
SSH_AUTH_SOCK=~/.herdr-drover/agent-fwd/afwd-repoA.sock git clone git@github.com:you/repoA
#   → 署名は owner Mac が実行・秘密鍵は slave に出ない
```

- **脅威モデル**: slave は同一 UID を複数人で共有＝socket も 0600 も同 UID には
  無力。本命の安全弁は **`ssh-add -c`（毎署名 owner 承認）** と **転送ウィンドウを
  owner が開いている間だけ有効**（Ctrl-C/切断で socket 自動撤去）＋**専用 deploy
  key（対象リポ限定・read-only 推奨）** で被害範囲を絞ること。
- **仕組み**: `afwd:<label>` の wake で slave を起こし、既存の grant/relay 機構
  （attach/↗窓 と同一）を再利用。relay/Firestore/Web は無改変。転送は
  `internal/agentfwd` の多重化 mux（1 relay セッション上で複数 SSH agent 接続）。
- ⚠ owner・slave 双方が本機能を持つビルド（>= v0.5.0）である必要がある。

### memvault（共用 slave の AWS / GCP / GitHub 認証）

共用 slave で AWS / GCP / GitHub の認証を **秘密材をディスクに置かず**行う。実体は
外部プロダクト [4noha/memvault](https://github.com/4noha/memvault) の daemon で、
raw material（SA 鍵 JSON・SSO refresh token・PAT・GitHub App 秘密鍵）は operator の
laptop から SSH 越しに daemon の stdin へ流し込み、**slave のディスクには一度も
落ちない**。ワークロードが受け取るのは外向きに使える短命トークンだけ。

drover が提供するのは **control plane の thin wrapper**（複数人が 1 daemon を
共用する multi-owner モードの operator 切替・job 寿命宣言）:

```sh
herdr-drover memvault status                 # kind ごとの残 TTL・slot 一覧
herdr-drover memvault whoami                 # 今 active な operator は誰か
herdr-drover memvault claim                  # 自分を active に（既定 $USER）
herdr-drover memvault release                # 降りる
herdr-drover memvault issue-inherit-token --owner me --for you --ttl 8h

# 長時間 job（docker build / terraform apply）の寿命を宣言して slot を延命
herdr-drover memvault job register --ttl 4h  # job-id 既定は $HERDR_PANE_ID
herdr-drover memvault job end                # 冪等
```

- **消費側は memvault が直接受ける**（drover は通らない）: AWS は
  `~/.aws/config` の `credential_process`＝SDK 全部（boto3 / terraform / cdk / CLI）、
  GCP は `GCE_METADATA_HOST` に立てた GCE metadata impersonator＝ADC 準拠ツール
  全部（gcloud / gsutil / bq / terraform-google / google-cloud-*）、GitHub は
  git-credential helper（`gh` は memvault repo の `tools/gh-mv` wrapper）。
- **inject 経路は drover に持たせない（意図的）**。raw material は operator の
  laptop から出さない設計契約を drover が破らないため。
- exit code は **3 = claim/release の conflict**（他 operator が active）を
  1（daemon 不在等）と区別する＝自動化から「奪うか待つか」を判断できる。
- 設計・脅威モデル・SSH 転送との使い分けは
  **[DESIGN_MEMVAULT.md](DESIGN_MEMVAULT.md)**。CLI 契約は [SPEC.md](SPEC.md) §2.3b。
- ⚠ memvault は **「協力的なチームの事故を防ぐ」道具**で、敵対的な同席者からは
  守れない（socket mode 0600 が唯一の認可境界＝同 UID は全 endpoint を呼べる）。

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

一覧同期・Web ターミナル・**リモート pane 注入（↗窓・実クラウド複数 PC 越しの
実機 e2e 済み）**・プラグイン化・遠隔命令・install/launchd・配布、**共用 PC
（slave）対応**、**一時 SSH エージェント転送（v0.5.0）** を実装済み。各機能は実
herdr 隔離サーバ＋実 Firestore エミュレータ＋実 relay（drover-cloud build）の常設
e2e gate（`test/`）で検証している。稼働版 v0.5.0（`mac-studio-herdr` ほか）。

進行中の残課題（in-flight・保留の再開ポイント）は **[TODO.md](TODO.md)** が正
（SSH 転送の実機 git e2e／IME Ctrl キー／resume backstop 等）。実 launchd への
ロード（カットオーバー）は `herdr-drover install` を手動実行（テストは
`--no-launchctl`＋隔離 HOME のみ＝実環境不可侵）。

## Requirements

- macOS / Linux, herdr >= 0.7.4, Go 1.25+（ソースビルド時）
- クラウド同期を使う場合: GCP プロジェクト（Cloud Run relay + Firestore）。
  クラウドサーバは独立リポジトリ **[drover-cloud](https://github.com/4noha/drover-cloud)**
  を 1 回デプロイして全 PC で共有する（一から立てる手順は
  [drover-cloud/SETUP.md](https://github.com/4noha/drover-cloud/blob/main/SETUP.md)）。
  既存クラウドに参加するだけなら GCP 操作は不要（Web「＋ 端末を追加」→ enroll）
