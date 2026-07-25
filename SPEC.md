# herdr-drover 機能・仕様詳細

本書は herdr-drover の**外部から観測できる契約**（CLI・遠隔命令・クラウドデータ
モデル・herdr API 依存・設定・不変条件）を実装から確定して記述したもの。

- 設計判断の背景は [DESIGN.md](DESIGN.md)、進行中作業は [TODO.md](TODO.md)。
- **他のコーディングエージェント対応**については
  [DESIGN_MULTI_AGENT.md](DESIGN_MULTI_AGENT.md)。本書内では
  <span id="agent-tag">**[claude 専用]** / **[要一般化]** / **[非依存]**</span>
  のタグで各項目の結合度を示す。
- 記載はすべて実コードおよび実機実測に基づく。推測は「推測」と明記する。

対象バージョン: herdr-drover v0.5.20 / drover-cloud v0.1.10 / herdr 0.7.4。

---

## 1. 製品境界

herdr（AI コーディングエージェント用ターミナルマルチプレクサ）の standalone
プラグイン。herdr のセッション群にクラウド同期を足す。

**herdr との接点は 2 本だけ**（この 2 本以外に herdr へ触らない）:

1. **ndjson API socket** — pane/tab/workspace の列挙・操作・入力注入・イベント購読
2. **同梱 CLI サブプロセス** `herdr terminal session observe|control` —
   ヘッドレスな frame ストリーム

**herdr のソースは vendor しない**（AGPL 衛生＝プロセス境界のデータ交換のみ）。

**クラウド層**は外部モジュール
[drover-cloud](https://github.com/4noha/drover-cloud) にあり、relay/Web/
Firestore サーバは Cloud Run に 1 回デプロイして全 PC で共有する。

---

## 2. CLI 仕様

`herdr-drover <subcommand> [args]`。exit code は **0=成功 / 1=実行時エラー /
2=使い方エラー**（`main.go` の `run()` が単一の dispatch 点）。

### 2.1 常駐・診断

| サブコマンド | 引数 | 説明 | 結合度 |
|---|---|---|---|
| `agent` | なし | 常駐 daemon（launchd から起動）。周期 tick＋SIGUSR1 で即時 re-scan | [非依存] |
| `status` | なし | daemon 生存・herdr 接続・設定充足を表示 | [非依存] |
| `nudge` | なし | 稼働 daemon へ SIGUSR1（herdr plugin events からの即時 re-scan） | [非依存] |
| `version` / `-v` / `--version` | なし | バージョン表示 | [非依存] |
| `help` / `-h` / `--help` | なし | ヘルプ | [要一般化]（本文が claude 前提） |

`agent`/`status`/`nudge`/`update` は**引数を取らない**。余分な引数は exit 2
（typo が成功に見えるのを防ぐ）。

### 2.2 導入・更新

| サブコマンド | 引数 | 説明 | 結合度 |
|---|---|---|---|
| `install` | `--dry-run` `--no-launchctl` | launchd 常駐を登録。ProcessType は焼かない | [非依存] |
| `uninstall` | `--dry-run` `--no-launchctl` | 常駐解除（設定とログは残す） | [非依存] |
| `enroll` | `<code> --relay wss://<host>` | Web の「＋端末を追加」コードで SA 鍵と設定を自動配置 | [非依存] |
| `update` | なし | **herdr-drover 自身**の selfupdate（GitHub Releases・sha256 検証・原子置換） | [非依存] |

### 2.3 セッション操作

| サブコマンド | 引数 | 説明 | 結合度 |
|---|---|---|---|
| `claude` | `[args...]` | claude シム。cwd 一致 attach／picker／新 Tab／非 TTY 透過 | **[claude 専用入口・要一般化]** |
| `organize` | `--dry-run` `--capture` | claude セッションを含む Tab を wsmap ルール解決先 Workspace へ整理 | [要一般化]（同定のみ） |
| `mv-tab` | `--self` `--src-tab` `--dst-ws` `--dst-ws-label` | Tab を別 Workspace へ丸ごと引っ越し | [非依存] |
| `mv-tab-launch` | なし | plugin action の実体（新 Tab を開いて対話モードを走らせる） | [非依存] |
| `restart-claude` | `--force` `--dry-run` `--model <alias>` `[sid]` | claude セッションを会話ごと作り直す | [要一般化] |
| `update-claude` | `--force` `--dry-run` `--model <alias>` `[sid]` | claude 本体更新＋セッション反映 | [要一般化] |
| `update-all` | `--force` `--model <alias>` | 上記＋自己更新（Web のワンボタン相当） | [要一般化] |
| `ssh-forward` | `<pc> [label]` | owner の ssh-agent を slave へ relay 越しに一時転送 | [非依存] |
| `attach` | `<pc> <sid>` | ↗窓 の viewer client（reconcile が注入 pane 内で起動する内部コマンド） | [非依存] |

### 2.4 `restart-claude` の詳細仕様

**目的**: claude バイナリを入れ替えても exec 済みプロセスは旧 inode に貼り付く
（`~/.local/bin/claude` は `versions/<ver>` への symlink）。pane を作り直して
新版を掴ませる。

**対象の選定**（すべて exact-match。ヒューリスティック分類は禁止）:

1. `agent.list` の `name` がシム encode 形（`claude` / `claude-N`）**かつ**
2. `pane.list` の `tokens` に `drover_inj_pc` / `drover_inj_sid` が**無い**
   （↗窓 注入 pane を構造的に除外）
3. `sid` 指定時はその pane のみ。対象外 sid は **loud に error**（黙って 0 件に
   しない）

**argv の構築**:

- **PATH からバイナリを解決しない**。daemon（launchd）の PATH には
  `~/.local/bin` も `~/.herdr-drover/bin` も入らないため、遠隔命令経路と CLI
  経路で別物を起動してしまう。権威は `layout.export` が返す**その pane の実
  launch_argv**
- 会話は `agent_session`(kind=id) の uuid を `--resume <uuid>` として張り直す。
  uuid 未検出なら argv を一切いじらない（既存 `--resume` を落として会話を失わない）
- `--model <alias>` 指定時は既存の `--model` を落として張り替える。未指定なら
  既存指定に触らない

**差し替え手順**:

```
layout.apply{tab_id, root:{pane, cwd, command:新argv}}   # 同 Tab を差し替え
  → tab.move（元の並び位置へ戻す）
  → agent.rename（元の agent 名を復元）
  → 4 秒の生存確認
```

**安全弁**（いずれも silent skip 禁止＝理由を必ず出力）:

| 条件 | 挙動 |
|---|---|
| `agent_status == "working"` | 既定 skip（`--force` で強制） |
| Tab に同居 pane がある | skip（`layout.apply{tab_id}` は Tab の全 pane を作り直すため） |
| pane に launch argv が無い | skip（shell pane） |
| `--resume` 起動が 4 秒以内に落ちた | **resume 無しで作り直す**（下記） |

**二段構え（不変条件: pane を消したまま終わらせない）**:
herdr の `agent_session` が指す uuid は「復元可能な会話」を保証しない。claude は
**最初のメッセージを送るまで jsonl を書かない**ので、起動しただけの未使用
セッションは「uuid はあるが jsonl は無い」状態になる（稀な破損ではなく通常状態）。
`claude --resume <無い uuid>` は即 exit し、**単独 pane の Tab はプロセス終了で
Tab ごと自動 close される**。よって差し替え後に生存を確認し、落ちていたら
`--resume` を外した元 argv で新 Tab を作り直し、位置・label・agent 名を復元する。
生存判定は `pane.get` の `pane_not_found` **exact** のみを「死」と扱う（socket
一時障害で作り直しを誘発して二重に壊さない）。

### 2.5 `update-claude` / `update-all` の詳細仕様

**バイナリ解決の権威順**（どの経路で決めたかを必ず出力）:

1. 稼働中のローカル claude pane の argv[0]（**食い違う複数種類は曖昧＝error**）
2. `PATH` の `claude`
3. `~/.local/bin/claude`（native installer の既定配置）

**更新有無の判定**: `claude update` は最新でも exit 0 を返す（実測 2.1.219:
`Claude Code is up to date`）ため、**exit code では判定できない**。
`--version` の前後比較が権威。

**更新が無くても再起動する**（「ディスクは最新だがセッションは旧版」を直すのが
目的）。**更新に失敗したらセッションを触らない**。

上限 15 分。claude 本体は ~250MB あり、実測でノート PC の Wi-Fi が 5 分に
収まらなかった。上限超過は「上限内に終わらず中断」と理由を明示する（生の
`signal: killed` では回線問題か破損か判別できない）。

**`update-all` の順序は入れ替えられない**:

```
(1) claude 本体更新＋セッション反映 → (2) herdr-drover 自己更新 → (3) 自身の再起動
```

自分自身の更新を反映する手段は「プロセスを終了して launchd に再起動させる」
だけで、**exit した時点でハンドラが終わる＝それ以降の段は実行されない**。よって
再起動は必ず最後。自己更新を先に済ませても走っているプロセスは旧 inode のまま
新コードにならないので、先に置く利点も無い。

二重起動は CAS で **loud に拒否**する（逐次実行が正しさの前提なので黙って
直列化しない）。

---

## 3. 遠隔命令仕様

Firestore `commands/{pc}/q/{id}` 経由。owner 認証済み Web が投入し、
各 PC の daemon が claim（`pending→running` を transaction で 1 度だけ）して実行、
結果を Ack（`done|error`）で書き戻す。

**allowlist**（`drover-cloud/state.ValidCommands`。web/agent 双方が同じ map を見る）:

| 命令 | sid | 実体 | Ack の位置 | 結合度 |
|---|---|---|---|---|
| `restart-agent` | — | `launchctl kickstart -k`（**herdr-drover デーモン**） | **先行**（自己 kill するため） | [非依存] |
| `self-update` | — | selfupdate.Update → `os.Exit(0)` | **先行** | [非依存] |
| `restart-proxy` | 必須 | 当該 sid の bridge respawn（**claude プロセスには触らない**） | 後 | [非依存] |
| `restart-claude` | 空=全部 | claude セッションを会話ごと作り直す | 後 | [要一般化] |
| `update-claude` | 空=全部 | claude 本体更新＋セッション反映 | 後 | [要一般化] |
| `update-all` | — | 上記＋自己更新＋再起動 | **先行**（この後 exit する） | [要一般化] |

**Ack 先行の規律**: 自プロセスが死ぬ命令は Ack を**実行前**に打つ。後 Ack だと
プロセスが死んで監査が `running` のまま永久に滞留する。

**未配線・未知命令**は `status=error` で Ack（pending を滞留させない）。

**多層防御**: owner 限定判定は web 側だが、agent 側も dispatch 前に revocation を
再検査する。

### 3.1 Command ドキュメント

```
id           string  // 12 バイト乱数の hex
cmd          string  // allowlist の語
sid          string  // 対象セッション（空=PC 全体）
requested_by string  // ログイン email（監査）
ts           string  // RFC3339Nano
status       string  // pending → running → done|error
detail       string  // 実行結果の要約（履歴に表示）
finished_at  string  // RFC3339Nano
```

⚠ **パラメータは `cmd` と `sid` の 2 つだけ**。`--model` のような追加引数を
遠隔から渡す口が無い（[DESIGN_MULTI_AGENT.md](DESIGN_MULTI_AGENT.md) §2.5 で
`agent` フィールド追加を提案）。

---

## 4. クラウドデータモデル

Firestore コレクション:

| パス | 内容 |
|---|---|
| `pcs/{pc}` | PC ごとのメタ（`cm_version` / `agent_kind` / `role` / セッション数） |
| `pcs/{pc}/sessions/{key}` | セッション一覧（下記） |
| `commands/{pc}/q/{id}` | 遠隔命令キュー（§3.1） |
| `wake/{pc}` | Web ターミナル起動の合図 |
| `slaves/{pc}` | slave の制限クレデンシャル |

### 4.1 session ドキュメント

`internal/session/producer.go` が `pane.list` / `agent.list` の実データから構築:

```
key          string  // pane_id（server 再起動を跨いで安定）
session_id   string  // = key
cwd          string
short_dir    string  // cwd の末尾（表示用）
window_name  string  // agent 名 → pane label → pane_id の優先順
is_active    bool    // agent_status == "working" の exact 写像
agent_status string  // herdr の生値（idle/working/blocked/done/unknown）
```

**除外**は「pane_id が空」と「↗窓 注入 pane」の 2 条件のみ（agent 種別で絞って
いない＝[非依存]）。

⚠ **`agent` フィールドが無い**。Web はどのセッションがどのエージェントか判別
できない（[DESIGN_MULTI_AGENT.md](DESIGN_MULTI_AGENT.md) §2.7）。

**`agent_kind: "herdr-drover"`** は「↗窓 に応答できる drover が居る PC か」の
製品マーカーであり、**コーディングエージェント種別ではない**。流用禁止
（`DroverPCs` が注入対象の絞り込みに使うため、壊すと ↗窓 が全滅する）。

---

## 5. herdr API 依存

使用している ndjson メソッド（28 種）:

```
agent.list  agent.rename  agent.start
layout.apply  layout.export
pane.close  pane.current  pane.get  pane.layout  pane.list  pane.move
pane.read  pane.release_agent  pane.report_agent  pane.report_agent_session
pane.report_metadata  pane.send_input  pane.send_keys  pane.send_text  pane.split
tab.focus  tab.list  tab.move
workspace.close  workspace.create  workspace.focus  workspace.list  workspace.rename
```

### 5.1 実測で確定した herdr 0.7.4 の契約（重要な trap）

| 事項 | 実測結果 |
|---|---|
| 接続モデル | **1 接続 = 1 リクエスト**（毎回再接続）。`events.subscribe` のみ長寿命 |
| 入力注入 | `pane.send_input`(text) は `\r` が落ちる。**`pane.send_text`**（\r 込み）か `pane.send_keys` を使う |
| `terminal session control` | 隠し CLI（`--help` 非掲載）。attach と同じ **pane resize＋lock** |
| `terminal session observe` | ロック非取得・観測側サイズへ仮想描画（観測 < grid は上寄せクリップ） |
| pane grid 桁 | API 非公開（`pane.get` の `scroll.viewport_rows` は行のみ） |
| `tab.move` | **同一 WS の reorder 専用**。WS 間移動は `pane.move` が唯一 |
| `tab.list` の `number` | **位置ではない**（w1 が tab 3 枚で number=5/21/23 の実測）。位置は並び順が権威 |
| workspace label | **重複可**＝識別子にしない（`workspace_id` を使う） |
| `layout.apply{tab_id}` | 新 tab を**末尾に作ってから**旧 tab を close。label(custom_name) は継承、**位置は末尾へ移る** |
| `layout.apply` の排他 | `tab_id` と `workspace_id` の同時指定は `invalid_target` |
| `layout.export` の `command` | pane の**実 launch_argv** を返す（バイナリ解決の権威） |
| 単独 pane の Tab | 中のプロセスが終了すると **Tab ごと自動 close** |
| `agent_session` の受理 | `(source, agent)` の **exact 許可制**（`herdr:claude`/`claude` 等 14 組）。非公式 source の report は **error にならず黙って捨てられる** |
| `agent_session` の上書き | **強制上書き API ではない**。seq 単調増加＋`session_start_source` 許可制＋同一 owner の別会話への差し替え拒否の多段ガード。**`ok` が返るのに値が変わらない**ので `pane.get` で読み直して確認すること |
| `pane.report_agent` の seq | seq は **送らない**（per-source の単調増加ゲートで、seq 有りだと後続 report が state を更新できない罠） |
| pane の消滅と reconcile | reconcile は **Firestore snapshot 駆動**。ローカル pane の消滅を契機にしない（後述 §7） |
| 注入 pane の `cwd` | herdr が同一 workspace の既存 pane 値を**便宜継承する**＝偽値。ルール書込経路で拾うと wsmap が汚染される |

---

## 6. 設定

### 6.1 環境変数（`~/.herdr-drover/config.json` でも可。env が優先）

| 変数 | 既定 | 説明 |
|---|---|---|
| `GCP_PROJECT` | — | Firestore の GCP プロジェクト（agent 必須） |
| `CLOUD_RELAY_URL` | — | Cloud Run relay の WSS URL |
| `GOOGLE_APPLICATION_CREDENTIALS` | — | SA 鍵パス |
| `PC_ID` | `<hostname 短縮小文字>-herdr` | 端末 id。**cm agent と同一 id 禁止** |
| `HERDR_SOCKET_PATH` | `~/.config/herdr/herdr.sock` | herdr ndjson API socket |
| `HERDR_ROLE` | `master` | `slave` で制限クレデンシャル動作 |
| `DROVER_TICK` | `5s` | producer 周期 |
| `DROVER_IDLE` | `30s` | Web ターミナル quiescence 自切断（負値＝無効化は不可） |
| `DROVER_MIRROR_AGENTS` | `false` | ↗窓 にリモートの agent_status を転記 |
| `DROVER_SHARE_LOCAL_IPS` | — | terminal_title へローカル IP を出す |

### 6.2 ファイル

| パス | 内容 |
|---|---|
| `~/.herdr-drover/config.json` | 設定（600） |
| `~/.herdr-drover/sa.json` | SA 鍵（600・非コミット） |
| `~/.herdr-drover/clouds.json` | マルチ Google アカウント fan-out |
| `~/.herdr-drover/workspaces.json` | Tab 着地ルール（`exact` > 最長 `prefix` > `default`）＋ `inject_placement` |
| `~/.herdr-drover/inject-index.json` | 注入 pane の identity index |
| `~/.herdr-drover/agent.log` | daemon ログ |

---

## 7. 不変条件（壊してはいけない性質）

1. **exact-match identity のみ**。ヒューリスティック分類はしない。曖昧なら
   skip して**必ず報告**する（silent skip 禁止）。
2. **↗窓 注入 pane は常に対象外**。identity token（`drover_inj_pc` /
   `drover_inj_sid`）で構造的に除外する。現在 3 系統に散在（producer /
   restartclaude / organize）＝**新しい pane 列挙経路を足すときは必ず除外を入れる**。
3. **注入 pane の cwd は偽値**（§5.1）。ルール書込経路（capture / learn）で
   拾わないこと。
4. **自己再起動は必ず最後**（§2.5）。exit がハンドラを終わらせるため。
5. **Ack はプロセスが死ぬ前**（§3）。
6. **pane を消したまま終わらせない**（§2.4 の二段構え）。
7. **`agent_kind: "herdr-drover"` を流用しない**（§4.1）。
8. **pc id は必ず `<host>-herdr`**（cm agent と同一 id は Firestore
   `DeleteSession` の削除合戦になる）。
9. **裸の `pkill herdr` をしない**（ユーザーの実サーバを殺した事故あり）。自分が
   spawn した PID だけを対象にする。
10. **attach 子プロセスの作り直しには kickstart が必須**。
    `pkill -f 'herdr-drover attach'` だけでは復旧しない — reconcile は Firestore
    snapshot 駆動でローカル pane の消滅を契機にしないため、注入 pane が消えたまま
    放置される（実測 2026-07-25: 11→0 のまま停止）。daemon の起動時 reconcile が
    唯一の確実な再生成契機。

---

## 8. デプロイ手順

```sh
cd ~/works/tools/herdr-drover
gofmt -l . && go build ./... && go vet ./... && go test ./... -count=1
git add -A && git commit                     # 日本語・Co-Authored-By

# リリース（GOWORK=off＝公開 drover-cloud タグで解決）
GOWORK=off make dist VERSION=vX.Y.Z
git tag -a vX.Y.Z -m "..." && git push origin main && git push origin vX.Y.Z
gh release create vX.Y.Z dist/herdr-drover_* dist/checksums.txt --title "..." --notes "..."

# ローカル反映（⚠上書き cp は macOS 署名キャッシュで SIGKILL＝rm→cp で新 inode）
rm ~/.herdr-drover/bin/herdr-drover && cp dist/herdr-drover_darwin_arm64 ~/.herdr-drover/bin/herdr-drover
codesign -s - -f ~/.herdr-drover/bin/herdr-drover
launchctl kickstart -k gui/$(id -u)/com.4noha.herdr-drover

# attach.go を変えたリリースでは ↗窓 も作り直す（順序厳守）
pkill -f 'herdr-drover attach' && launchctl kickstart -k gui/$(id -u)/com.4noha.herdr-drover
```

**Cloud Run（drover-cloud）**:

```sh
cd ~/works/tools/drover-cloud
gcloud builds submit --project=<PROJECT> --config deploy/cloudbuild.yaml \
  --substitutions=_IMAGE=<REGION>-docker.pkg.dev/<PROJECT>/cm/<SERVICE> .
gcloud run deploy <SERVICE> --project=<PROJECT> --region=<REGION> --image=<IMAGE>
```

⚠ **`deploy/deploy.sh` は初回構築専用**。`--set-env-vars` で env を全置換する
ため、稼働中サービスに実行すると `GOOGLE_OAUTH_CLIENT_ID` /
`ALLOWED_EMAILS` / `ENROLL_SA_JSON_B64` / FCM 系が消える。既存サービスの更新は
上記の image 差し替えのみを使い、デプロイ後に env の個数を確認すること。

⚠ **命令名を増やすときは Cloud Run を先にデプロイする**。逆順だと web の
allowlist に無く `未知のコマンド` で投入できない。

---

## 9. 開発の鉄則

1. **推測修正をしない** — 「動かない」はまず実再現してから直す
2. **実テストで担保** — 実 herdr（隔離 `HERDR_SOCKET_PATH`・短い /tmp パス＝
   `sun_path` 104B 制約）・実 Firestore エミュレータ・実録画 fixture。
   **修正前に旧コードでテストが落ちることを確認**
3. **ヒューリスティック分類をしない** — exact-match の identity
4. **herdr ソースの vendor 禁止**（AGPL 衛生）
5. **silent なコード/設定変更をしない**（丸め・skip は必ずログ）
