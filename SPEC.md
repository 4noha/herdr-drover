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
| `restart-agent-session`<br>（旧名 `restart-claude` も可） | `--force` `--dry-run` `--model <alias>` `--agent <kind>` `[sid]` | エージェントセッションを会話ごと作り直す | [一般化済 v0.5.23] |
| `update-agent-cli`<br>（旧名 `update-claude` も可） | 同上 | エージェント本体更新＋セッション反映 | [一般化済 v0.5.23] |
| `update-all` | `--force` `--model <alias>` | **導入済み × 更新口を持つ全エージェント**を順に更新＋セッション反映 → 自己更新 | [一般化済 v0.5.27] |
| `shim <agent> [args...]`<br>（`claude` は別名） | — | エージェントシム。argv[0] multi-call でも起動可 | [一般化済 v0.5.23] |
| `ssh-forward` | `<pc> [label]` | owner の ssh-agent を slave へ relay 越しに一時転送 | [非依存] |
| `attach` | `<pc> <sid>` | ↗窓 の viewer client（reconcile が注入 pane 内で起動する内部コマンド） | [非依存] |

### 2.3b 共用 slave の認証（memvault control plane）

| サブコマンド | 引数 | 説明 | 結合度 |
|---|---|---|---|
| `memvault status` | なし | memvault `/status` を pretty-print（kind ごとの残 TTL・slot 一覧） | [非依存] |
| `memvault whoami` | なし | active operator と実効 slot（inherit 込み） | [非依存] |
| `memvault claim` | `--operator NAME` `--force` `--inherit --token T` | 自分を active operator に | [非依存] |
| `memvault release` | `--operator NAME` `--force` | active を降りる | [非依存] |
| `memvault issue-inherit-token` | `--owner NAME` `--for OP` `--ttl 8h` | 自 slot を他人に貸す consent を発行 | [非依存] |
| `memvault job register` | `--owner NAME` `--job-id ID` `--ttl 4h` | 長時間 job の寿命宣言（slot 延命） | [非依存] |
| `memvault job end` | `--owner NAME` `--job-id ID` | job 登録の解除（**冪等**） | [非依存] |

設計と AWS/GCP/GitHub の材料・消費経路は **[DESIGN_MEMVAULT.md](DESIGN_MEMVAULT.md)**
が正。ここは drover 側のインターフェース契約のみを定める。

**exit code は §2 の一般規則を 1 つ拡張する**:

| code | 意味 |
|---|---|
| 0 | 成功 |
| 1 | 実行時エラー（daemon 不在・socket 到達不能・HTTP 5xx 等） |
| 2 | 使い方エラー（未知サブコマンド・必須引数欠落） |
| **3** | **claim / release の conflict**（HTTP 409） |

3 を分けるのは、自動化から「他人が active（＝奪うか待つかの判断が要る）」と
「daemon が落ちている（＝復旧が要る）」を区別させるため。conflict 時は daemon が
返した JSON を **stderr にそのまま出す**（誰が active かを人が読める）。

**socket 解決の権威順**（用途で使う socket が違う＝split-socket 対応）:

| 用途 | 順序 |
|---|---|
| control plane（上表の全サブコマンド） | `$MEMVAULT_CTRL_SOCKET` → `$MEMVAULT_SOCKET` → `$HOME/.memvault.sock` |
| use plane（drover は未使用・client のみ保持） | `$MEMVAULT_USE_SOCKET` → `$MEMVAULT_SOCKET` → `$HOME/.memvault.sock` |

いずれも legacy `$MEMVAULT_SOCKET` に落ちる＝単一 socket 運用の daemon が無改造で
動く。全部空なら「memvault が無い」として **loud に失敗**する（推測しない）。

**operator 名の決定順**: `--operator` → `$MEMVAULT_OPERATOR` → `$USER`。
3 つ全部空なら**エラー**（鉄則③＝推測で operator を決めない）。
`job register` / `job end` の `--owner` は空を許す（＝default slot＝従来の
1-tenant ケース）が、明示されなければ claim/release と同じ順序を使う
（**同じコマンド列で対象 slot がブレないようにする**）。

**`--job-id` 省略時は `$HERDR_PANE_ID`**（drover が既に持つ pane 単位の識別子を
再利用＝新しい ID 体系を足さない）。両方空ならエラー。

**inject 系の入口は持たない（意図的）**。raw material を送る経路は各 operator の
laptop からの SSH tunnel が担当する。理由は DESIGN_MEMVAULT.md §6.1。

**`status` の出力は `/status` の応答をそのまま提示する**（欠落なし）。struct に
宣言したフィールドだけを出す実装は、daemon が `/status` を拡張するたびに黙って
情報を落とす（実際に `git_loaded` / `git_hosts` / `github_app_loaded` /
`kind_ttl_remain_sec` / `routes` の 5 キーが消えていた＝鉄則⑤違反。2026-07-31
修正）。したがって:

- **分岐**に使う値は `memvaultclient.Status` の typed field
- **表示**は `Status.Raw`（未宣言キーを含む生の map）

回帰は `TestMemvaultStatusShowsEveryDaemonField`（実 daemon 相手に「daemon が
返したキーが 1 つでも出力に無ければ FAIL」）で固定。

**`status` / `whoami` は slot ズレを検出したら stderr に警告する**。
「active operator の slot は空だが default slot に材料がある」状態は、参照側
（`/aws/creds` `/gcp/*` `/git/credential`）が `--owner` 省略時に active slot を
見るため **材料があるのに 404/503 になる**（しかも 404 本文は host 違いにしか
読めない）。判定は exact（`/status` の `slots` オブジェクト自体が無い
＝multi-owner 前の daemon のときだけ黙る。**エントリ不在は「空」で確定**＝
memvault は slot を lazy 生成し、`claim` は slot を作らないため）。
stdout は素の JSON のまま保つ（機械可読性を壊さない）。詳細は
[DESIGN_MEMVAULT.md](DESIGN_MEMVAULT.md) §5.4(a)。

### 2.4 `restart-agent-session` の詳細仕様

**目的**: claude バイナリを入れ替えても exec 済みプロセスは旧 inode に貼り付く
（`~/.local/bin/claude` は `versions/<ver>` への symlink）。pane を作り直して
新版を掴ませる。

**対象の選定**（すべて exact-match。ヒューリスティック分類は禁止）。
identity 判定は `resolveAgentKind`（`cmd/herdr-drover/agentid.go`）に一元化され、
shim / restart・update / organize が**同じ規則**を共有する（v0.5.23）:

1. `resolveAgentKind` が `claude` を返すこと。権威は強い順に
   ① `agent_session` の `(source,agent)`＝`("herdr:claude","claude")`
   ② シム命名 `claude` / `claude-N`
   ③ herdr の検出値 `agent`（canonical 21 label への exact-match のみ）
   — ②③ は**どちらか一方でも成立すれば対象**。以前は restart/update だけが
   ①③ を見ておらず、herdr UI から直接起動した claude を取りこぼしていた
2. `tokens` に `drover_inj_pc` / `drover_inj_sid` が**無い**（↗窓 注入 pane を
   構造的に除外）。これは**最優先で**評価する — reconcile の mirror_agents が
   注入 pane の検出値に `claude` を書くため、後ろに置くとリモートの鏡を
   ローカルセッションと誤認する。判定は `agent.list` 単独で完結する（AgentInfo が
   tokens / agent_session / tab_id を持つ＝実測。**pane.list との join はしない** —
   1 接続=1 リクエストなので 2 往復は競合の窓になる）
3. 権威同士が矛盾する pane（例: 名 `claude` / 検出 `codex`）は**機械確定不能**＝
   対象外＋必ず報告（推測で動かさない）
4. **argv が claude の直接起動であること**（`isDirectAgentInvocation`＝
   `launch_argv[0]` の basename が `claude`）。identity が claude でも
   `zsh -lc '… claude'` のような wrapper 起動は**触らない** — 末尾に `--resume` を
   足しても claude に届かず、会話を失ったまま「done」と報告してしまうため
5. `sid` 指定時はその pane のみ。対象外 sid は **loud に error**（黙って 0 件に
   しない）

**未命名 pane の扱い**: herdr UI 起動の pane（agent 名なし）も対象だが、
再起動後に**agent 名を付けない**（drover 管理名に変えるのはユーザーが頼んで
いない identity 変更）。次回も検出値 ③ で拾えるので取りこぼさない。

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

### 2.5 `update-agent-cli` / `update-all` の詳細仕様

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

**`update-all` の対象と失敗時の扱い**（v0.5.27 で確定）:

- 対象は **UpdaterSpec を持つ**（更新方法を実 CLI で確認済み）かつ
  **このマシンに導入されている**（InstallSpec でバイナリが解決できる）エージェント。
  実行順は canonical 名の昇順で**決定的**。未導入は 1 行出して skip（silent skip 禁止）。
- ⚠**1 つのエージェントの失敗で他や自己更新を止めない**。失敗は集約して Ack に残す。
  理由: **自己更新は不具合修正の唯一の配布経路**で、例えば cursor の更新失敗で
  herdr-drover 自身が更新できなくなるのが実運用で最も困る。
  （v0.5.26 以前は claude 段の失敗で全体を止めていた＝**意図的に反転させた**）
- ただし**エージェント単位**では従来どおり「更新に失敗したらそのセッションは
  触らない」を維持する（古いまま作り直しても目的を達さず pane を無駄に作り直すだけ）。

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
| `restart-daemon`<br>（旧名 `restart-agent`） | — | `launchctl kickstart -k`（**herdr-drover デーモン**） | **先行**（自己 kill するため） | [非依存] |
| `self-update` | — | selfupdate.Update → `os.Exit(0)` | **先行** | [非依存] |
| `restart-proxy` | 必須 | 当該 sid の bridge respawn（**claude プロセスには触らない**） | 後 | [非依存] |
| `restart-agent-session` | 空=全部 | エージェントセッションを会話ごと作り直す | 後 | [一般化済] |
| `update-agent-cli` | 空=全部 | エージェント本体更新＋セッション反映 | 後 | [一般化済] |
| `update-all` | — | 上記＋自己更新＋再起動 | **先行**（この後 exit する） | [agent 非依存] |

**旧命令名は alias として allowlist に残す**（`restart-claude` / `update-claude` /
`restart-agent`）。まだ更新していない PC の Web/CLI から投げられなくなるため、
外してはいけない。`state.NormalizeCommand` が新名へ写像し、`agent` を `"claude"` に
固定する（旧名は claude 専用だった＝推測ではなく事実）。`restart-agent` だけは
元々 agent 概念を持たないので空のまま `restart-daemon` になる。**どう解釈したかは
必ず Ack detail に残す**。

**`agent` パラメータ**: 空 = その PC の全エージェント種別（`sid` 空と同型）。
Cloud 側 `ValidAgents`（canonical 21 種）で投入時に検証する — 未知の値を通すと
受け手が「知らないので全 agent」に degrade して**意図より広い破壊**をしかねない。

⚠**デプロイ順序**: Cloud Run を**先に**デプロイして新命令名を allowlist へ追加
→ その後 agent 側。逆順だと `未知のコマンド` で投入できない。

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
agent        string  // エージェント種別（canonical label。空=全種別）  ← v0.5.23
requested_by string  // ログイン email（監査）
ts           string  // RFC3339Nano
status       string  // pending → running → done|error
detail       string  // 実行結果の要約（履歴に表示）
finished_at  string  // RFC3339Nano
```

⚠ `agent` は **struct タグだけでは Firestore に書かれない**（`Set` は map を
そのまま書く）。`PushCommand` の**map リテラルにも `"agent"` を足すこと**。

⚠ **`--model` を遠隔から渡す口はまだ無い**（`cmd` / `sid` / `agent` の 3 つ）。

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
window_name  string  // シム命名 → herdr 検出種別 → pane label → pane_id の優先順
is_active    bool    // agent_status == "working" の exact 写像
agent_status string  // herdr の生値（idle/working/blocked/done/unknown）
agent        string  // エージェント種別（canonical label）。**空なら載せない** ← v0.5.23
```

**除外**は「pane_id が空」と「↗窓 注入 pane」の 2 条件のみ（agent 種別で絞って
いない＝[非依存]）。

`agent` は `agentid.Resolve` の結果。**空／canonical 外／権威が矛盾なら
キー自体を載せない**（後方互換＝旧 Web・旧 agent は追加キーを知らない）。
session doc は端から端まで pass-through なので、1 キー足すだけで Firestore→Web
まで中間層の改修ゼロで届く。影響は初回 1 回だけ `content_hash` が変わって
write が 1 回増えることのみ。

**`agent_kind: "herdr-drover"`** は「↗窓 に応答できる drover が居る PC か」の
製品マーカーであり、**コーディングエージェント種別ではない**。流用禁止
（`DroverPCs` が注入対象の絞り込みに使うため、壊すと ↗窓 が全滅する）。

---

## 4.2 エージェント種別（canonical label）

herdr が検出する **21 種**（`src/detect/mod.rs agent_label()` から実ソース抽出）:

```
agy amp claude cline codex copilot cursor devin droid gemini grok
hermes kilo kimi kiro maki mastracode omp opencode pi qodercli
```

**identity 判定は `internal/agentid.Resolve` に一元化**（shim / restart・update /
organize / producer が共有）。権威は強い順に:

| # | 権威 | 供給元 | 備考 |
|---|---|---|---|
| 0 | 注入 token | `drover_inj_pc` / `drover_inj_sid` | **最優先で対象外**。reconcile の `mirror_agents` が注入 pane の検出値に `claude` を書くため、後段に置くとリモートの鏡を誤認する |
| 1 | `agent_session.(source,agent)` | herdr の 14 組 allowlist | 値域が canonical に閉じる。ただし `report_agent_session` は無認証の公開 API＝**偽装不能ではない** |
| 2 | シム命名 `<agent>` / `<agent>-N` | drover が慣習的に書く | ユーザーも `herdr agent rename` で書ける同一フィールド |
| 3 | herdr の検出値 `agent` | プロセス名検出 ∪ hook 申告 | **canonical への exact-match 必須**（未知文字列は捨てる） |

権威同士が矛盾したら **機械確定不能**＝対象外＋必ず報告（推測で動かさない）。

### 4.3 エージェント差分の Spec テーブル

`internal/agentid/spec.go`。**分岐をコードに散らさずデータで持つ**。

- **ResumeSpec**（herdr `agent_resume.rs plan()` の写し・14 種）— resume 引数の形は
  4 通り: `--resume <v>`（claude/devin/droid/hermes/qodercli/cursor）／
  `--session <v>`（kimi/opencode/kilo/pi）／`--thread <v>`（mastracode）／
  `--resume=<v>`（copilot/omp）／`codex resume <v>`（**位置引数サブコマンド**）。
  `cursor` だけ argv[0] が `cursor-agent`。`pi`/`omp` は `path` kind も取る。
  **残り 7 種（agy/amp/cline/gemini/grok/kiro/maki）は resume 原理的に不可**
  （herdr が `agent_session` を出さない）＝素起動へ落として loud に報告。
- **UpdaterSpec** — `VersionArgv`（nil=版比較 skip）／`UpdateArgv`（nil=更新口なし＝
  再起動のみ）／`Timeout`（**per-agent 予算**。全体に 1 本掛けない）。
  実 CLI で確認した 5 種（claude / codex / cursor / copilot は `<bin> update`＋
  `--version`。**devin は `--version` のみで `UpdateArgv=nil`**）。**推測で書かない**。
  ⚠ **`UpdateArgv=nil` は「更新口が無い」ではなく「自動化から呼べない」も含む**:
  `devin update` は存在するが**非対話で完走しない**（stdin を閉じて rc=130）うえ
  Homebrew cask 管理と食い違うため、あえて載せていない（人が
  `brew upgrade --cask devin-cli`、drover は再起動だけ担当）。
  対象種別は `agentid.UpdaterAgents()` が表から導出する（**文言に焼かない**）。
- **ModelSpec** — 起動時のモデル指定。`Flag` と `Aliases`（strip 対象の短縮形）。
  ⚠**フラグ名が同じでもモデル名は互換でない**（claude=`opus` / codex=`gpt-5` /
  cursor=`sonnet-4-thinking` / devin=`claude-sonnet-4`）。よって `--model` は
  **`--agent` と併用が必須**（旧名 `restart-claude` 経由のみ claude 固定として許す
  ＝既存手順書の互換）。codex だけ短縮形 `-m` を持つので、**剥がし損ねると二重指定に
  なる**（copilot / devin に短縮形は無い＝実測）。
- **InstallSpec** — `BinNames` / `WellKnownPaths`。⚠**`BinNames` は herdr の
  `lookup_agent` alias 表の要素でなければならない**。表に無い basename で起動すると
  herdr の検出（前景プロセス名基準）に**一切載らず**、`pane.agent` も
  `agent_session` も付かない＝resume backstop も organize の検出系統も silent に
  無効化される。`ValidateSpecs()` が静的検証する。

### エージェント固有の実測差（2026-07-25・実機検証）

| 事項 | claude | codex | cursor |
|---|---|---|---|
| herdr の検出 | ✅ | ✅（0.6 秒） | ✅ |
| `agent_session` の発火契機 | **起動時** | **初回の発話時** | **初回の発話時**（要 trust 通過） |
| 会話 ref の形 | uuid v4 | uuid v7 系 | uuid v4 |
| resume argv | `--resume <id>` | `codex resume <id>` | `--resume <id>`（argv[0]=`cursor-agent`） |
| restart 実機 | ✅ | ✅ | ✅ |
| resume 後の hook 再発火 | ✅ | ❌ **しない** | ❌ **しない**（2026-07-26 計測） |

#### copilot / devin（2026-07-26 追加・**会話 e2e まで実機検証済み**）

| 事項 | copilot 1.0.75 | devin 3000.2.17 |
|---|---|---|
| 導入 | `npm i -g @github/copilot`（要 Node 22+） | `brew install --cask devin-cli` |
| 実行ファイル | `copilot` | `devin` |
| 版の出力 | `GitHub Copilot CLI 1.0.75.` ＋ 更新案内の 2 行目 | `devin 3000.2.17 (2c489dfc)` |
| 認証 | `copilot login`（OAuth device flow・Keychain 保存。<br>`COPILOT_GITHUB_TOKEN`>`GH_TOKEN`>`GITHUB_TOKEN` も可） | `devin auth login`<br>（`~/.local/share/devin/credentials.toml`） |
| herdr の検出 | ✅ | ✅ |
| `agent_session` の発火契機 | 初回発話時までに付与（起動時かは未計測） | 同左 |
| **会話 ref の形** | uuid v4 | ⚠**単語スラッグ**（実測 `resolute-lynx`） |
| resume argv | `--resume=<id>`（`-r` 短縮形あり） | `--resume <id>`（`-r` 短縮形あり） |
| **restart 実機（会話復元）** | ✅ | ✅ |
| **resume 後の hook 再発火** | ❌ **しない**（codex と同型） | ✅ **する** |
| 自己更新 | `copilot update` ✅ 非対話で完走 | ❌ **非対話で完走しない**（rc=130）＝Spec に載せない |
| `--model` | ✅（短縮形なし） | ✅（短縮形なし・env `DEVIN_MODEL`） |

#### 「resume 後の hook 再発火」の一覧（2026-07-26 に cursor を計測して確定）

| claude | codex | cursor | copilot | devin |
|---|---|---|---|---|
| ✅ する | ❌ しない | ❌ しない | ❌ しない | ✅ する |

⚠ **再発火しないのが多数派（5 種中 3 種）**。❌ の種別は 1 回目の restart で
`agent_session` が消えるため、**同じ pane を 2 回目に restart すると素起動になる**
（会話が失われる）。herdr / 各エージェント側の性質で drover では埋められない。
運用は **`--dry-run` で `--resume <ref>` が組み立てられるか確認してから実行**する。
cursor の計測は「restart → 画面に元の発話が復元されている（＝resume は成功）／
`agent_session` は `None` に落ちる」を実 pane で確認した。

⚠ **devin の会話 ref は UUID ではない**（`resolute-lynx`）。`ValidSessionRef` が
「非空・512B 以下・制御文字なし」しか見ない設計だからそのまま通った。
**値の書式で判定するヒューリスティックを入れると devin が resume 不能になる**
（`-r report.md` の一件で書式判定を排した判断が、別種別で実際に効いた事例）。

⚠ **copilot の実バイナリは VS Code 拡張の同梱版に解決されうる**（実測:
`~/Library/Application Support/Code/User/globalStorage/github.copilot-chat/copilotCli/copilot`）。
「稼働 pane の argv[0] が権威・PATH 非依存」という設計どおりの動作だが、
`update-agent-cli copilot` はその同梱版を更新対象にするので VS Code 側の管理と
食い違いうる。npm 版と同版（1.0.75）であることは確認済み。

⚠ **devin の resume は「値が任意」形**（clap の `-r, --resume [<SESSION_ID>]`）。
optional value はスペース区切りで値を拾わない実装がありうるので実測した:
`devin --resume <id>` はパースを通り、対照の裸位置引数 `devin <id>` は
`error: unexpected argument` で弾かれる＝**スペース形で値が付く**（`FormSpace` が正）。

⚠ **`agent_session` は herdr が自力で見つけるのではなく、各エージェントの hook が
報告する**（`herdr integration install <agent>` で設置。`herdr integration status`
で確認）。**integration 未設置のエージェントは resume が原理的に不可能**
（drover 側は正しく素起動へ落として loud に報告する）。設置状況は**環境ごとに
違う**ので、`agent_session` が付かないときは status を確認する — ただし
**未設置は原因の候補の 1 つにすぎない**（下の trust ダイアログの例を参照）。

**自動導入**（v0.5.26〜）: シムが**新規セッションを始める直前**に未導入なら
`herdr integration install <agent>` を実行する（`DROVER_AUTO_INTEGRATION=off` で無効）。
規律:

- **未導入のときだけ**入れる。導入済み（版が古くても）には触らない
  — ユーザーが手で直した hook を勝手に戻さない。
- 導入したら**必ず報告**する（silent な設定変更をしない）。
- 失敗しても**起動を止めない**（hook 無しでもエージェント自体は動く）。
- **attach 経路では実行しない**。hook は session 開始時に発火するので、既存
  セッションに後から入れても ref は**遡らない**＝効果が無い。
- ⚠**「観測した agent に対して入れる」方式にしてはいけない**。↗窓 の注入 pane は
  リモートのエージェントを鏡写しするので `agent` が付く（reconcile の
  `mirror_agents`）が、実体は `herdr-drover attach` でローカルにその CLI は無い
  （実測 2026-07-25: 注入 11 枚すべて attach プロセス・`agent_session` は None）。
  観測駆動にすると**そのPCに無いエージェントの設定を書いてしまう**。
  ⇒ **注入 pane に integration は不要**（そのエージェントが実際に走る PC 側で要る）。

⚠ **初回起動時の同意ダイアログに注意**（claude / cursor の**両方**で確認）。
新しい cwd では「このフォルダを信頼しますか」ダイアログが出て**入力を全て吸う**。
通過するまで会話が始まらず、`agent_session` も付かない（＝resume 不可）。
自動化から見ると「検出はされるのに永久に idle」という分かりにくい状態になる。

- claude: `Quick safety check: Is this a project you created or one you trust?`
  → Enter（既定は「Yes, I trust this folder」）
- cursor: `⚠ Workspace Trust Required` → `[a]`

**画面を読む手段（`herdr pane read <pane> --source visible`）を持っておくこと** —
これが無いと原因に辿り着けない（実際、両方ともこれで初めて判明した。それまでは
hook や integration の不具合を疑って空振りしていた）。

### ⚠ `CLAUDE_CODE_CHILD_SESSION` による transcript 抑止（原因特定済み）

herdr server が `CLAUDE_CODE_CHILD_SESSION` を持っていると（**Claude Code の中から
herdr server を起動すると起きる**）、herdr が生やす全 pane がそれを継承する。
その claude は「サブセッション」とみなされ **transcript を保存しない**。
`--resume` が読むのはその transcript なので、**そのマシンの claude セッションは
どれも復元できない**。

実測（A/B で確定。⚠**claude のバージョンで結論が変わる**ので必ず版を併記すること）:

| 条件 | claude 版 | transcript |
|---|---|---|
| 素の herdr 経路 | 2.1.219 / 2.1.220 | **保存されない** |
| pane に `CLAUDE_CODE_FORCE_SESSION_PERSISTENCE=1` を注入 | 2.1.219 | 保存されない |
| **同上** | **2.1.220** | ✅ **保存される**（2026-07-26 実測） |
| pane に `CLAUDE_CODE_CHILD_SESSION=""`（空値）を注入 | 2.1.219 | 保存されない |
| `claude -p`（print mode・マーカーあり） | 2.1.219 | 保存される |

⇒ **claude 2.1.220 で `CLAUDE_CODE_FORCE_SESSION_PERSISTENCE=1` が効くようになった**
（2.1.220 の実装は「FORCE があれば抑止しない」を先頭分岐で判定する）。
**「env 注入では直せない」という 2.1.219 時点の結論は 2.1.220 では成り立たない**。
汚染サーバを直ちに再起動できない場合の緩和策として使える（既に走っている
プロセスの env は変えられないので、**新しく起動する claude にのみ効く**）。

⚠ **`herdr server live-handoff` では直らない**（2026-07-26・隔離 herdr で実測）。
pane は生き残るが **新 server が旧 server の env を丸ごと継承する**（仕込んだ
`CLAUDE_CODE_SESSION_ID` がそのまま新 server に現れた）。handoff 後に新規作成した
pane も汚染を継承した。**clean な env から handoff を呼んでも無意味**＝この道は無い。

⚠ `herdr workspace create --env KEY=VALUE` は **root pane に届かない**（同実測。
`printenv` で未設定）。pane へ env を渡す経路として当てにしないこと。

### 原因と恒久対処（v0.5.27 で特定・修正）

**混入経路は drover 自身だった**。シムの `ensureHerdrServer` が herdr server を
自動起動するとき、**親の環境をそのまま渡していた**（`cmd.Env=nil` で継承）。
Claude Code のセッション内からシムが呼ばれ herdr が未起動だと、
`CLAUDE_CODE_CHILD_SESSION` を抱えたサーバが常駐する。**herdr は pane を
server の env で起こす**ので、そこから生える全 pane が継承する。

実障害: mac-studio の herdr server は 2026-07-18 10:37（herdr 移行の日）に
この経路で起動され、8 日間マーカーを持ち続けていた。他 PC で起きなかったのは、
そちらのサーバが Claude Code の外から起動されていたため。

**対処**: `sanitizedServerEnv` が呼び出し元固有の変数を **exact-match** で落とす
（`CLAUDE_CODE_CHILD_SESSION` / `_SESSION_ID` / `_ENTRYPOINT` / `_SSE_PORT`）。
落としたものは必ず報告する。`HERDR_SOCKET_PATH` / `XDG_CONFIG_HOME` は透過
（隔離テストの前提）。

⚠**既に汚染されたサーバは自動では直らない**（env はプロセス起動時に決まる）。
そのマシンでは **herdr server をクリーンな環境で起動し直す**必要がある
（⚠全 pane が失われるので慎重に。設定や integration の再導入は不要）。

**クリーン再起動の手順**（herdr の**外**の clean なシェルから。実施 2026-07-26）:

```sh
env | grep CLAUDE_CODE                          # 何も出なければ clean
cp ~/.config/herdr/session.json ~/.config/herdr/session.json.bak-$(date +%F)
herdr server stop                               # ⚠全 pane のプロセスが死ぬ
env -u CLAUDE_CODE_CHILD_SESSION -u CLAUDE_CODE_SESSION_ID \
    -u CLAUDE_CODE_ENTRYPOINT -u CLAUDE_CODE_SSE_PORT herdr server &
# 検証（何も出なければ成功）:
ps eww -p $(lsof -t ~/.config/herdr/herdr.sock | head -1) | tr ' ' '\n' | grep CLAUDE_CODE
```

- **残る**: `~/.config/herdr/session.json` のレイアウト（pane ごとの cwd / label /
  `launch_argv`）。**失う**: 全 pane のプロセスと画面。汚染下の claude は transcript が
  無いので `--resume` は即終了する＝**会話は戻らない**（単独 pane の Tab は消えうる）。
- ⚠ **汚染の当事者判定は `ps eww -p <server pid>`**（同 uid なら macOS でも env が読める）。
  pane 側の `CLAUDE_CODE_SSE_PORT` が server のそれと**同値**なら継承の決定的証拠。
- ⚠ socket を握っている server を `lsof -t <socket>` で特定してから触ること。
  同時に**テスト残骸の herdr server が複数常駐しうる**（`/tmp/hd*` の socket を持つ）。
  裸の `pkill herdr` は恒久禁止＝socket で本番と残骸を見分ける。

**drover 側の挙動は正しい**: `--resume` で即終了 → 二段構えフォールバックが
resume 無しで作り直して **pane は必ず残す**＋`resume 復元不可のため新規会話で起動`
と正直に報告する。会話は失われるが、黙って失うことはない。

⚠ **codex は resume 後に hook が再発火しない**（hook 呼び出しログで実測）。
「1 回目の restart は会話 ref を使えるが、herdr は ref を再学習しない」＝
**同じ pane の 2 回目の restart は素起動になる**。drover 側の不具合ではない。

⚠ resume 引数の抽出は **Spec 駆動**（「そのフラグが値を取るか」で決める）。
**値の書式で判定しない** — claude の会話 ref はたまたま uuid だが pi/omp は path も
取るので、uuid 判定だと二重指定や誤削除、他エージェントでの backstop 不発が起きる。

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
| `DROVER_MIRROR_AGENTS` | `false` | ↗窓 にリモートの agent_status を転記（**metadata 転記の gate であって注入の gate ではない**） |
| `DROVER_INJECT_REMOTE` | `true` | 他 PC のセッションを ↗窓 pane として注入するか（file 名 `inject_remote_panes`）。`false` で**既存の注入 pane も撤去**し新規を作らない。producer（Web/スマホ閲覧）は止めない |
| `DROVER_SHARE_LOCAL_IPS` | — | terminal_title へローカル IP を出す |

### 6.2 ファイル

| パス | 内容 |
|---|---|
| `~/.herdr-drover/config.json` | 設定（600） |
| `~/.herdr-drover/sa.json` | SA 鍵（600・非コミット） |
| `~/.herdr-drover/clouds.json` | マルチ Google アカウント fan-out |
| `~/.herdr-drover/workspaces.json` | Tab 着地ルール（`exact` > 最長 `prefix` > `default`）＋ `inject_placement` |
| `~/.herdr-drover/inject-index.json` | 注入 pane の identity index |
| `~/.herdr-drover/attach-version` | 注入 pane を最後に作り直した drover 版数（600）。§6.3 |
| `~/.herdr-drover/attach.log` | ↗窓 viewer の接続ログ（600・全 viewer 共有）。§6.4 |
| `~/.herdr-drover/agent.log` | daemon ログ |

### 6.4 ↗窓 viewer の接続ログ（`attach.log`）

**⚠ これが無いと ↗窓 の接続障害は事後診断が構造的に不可能だった。** attach の診断は
すべて pane 画面向けで、しかも各エラーは `\x1b[2J`（画面クリア）してから書くため
**次のフレームが 1 枚来た瞬間に消える**。再注入すれば復旧するので、原因を追う手がかりが
毎回失われていた（「viewer が張り付いて注入し直すまで戻らない」障害が長く残っていた理由）。

- **全 viewer が 1 本のファイルを共有する**。この障害でまず知りたいのは「全 viewer が
  同時に落ちたのか（ネットワーク/relay 側）／個別に落ちたのか（プロセス個別）」で、
  ファイルを分けると突合が手作業になる。行頭は `<pc>/<sid>[pid]`。
- **1 サイクル粒度**（フレームごとには書かない）。BUG-3 の thrash で agent.log が
  16.8MB に膨れた前例があるため粒度は粗く保つ。8MB で 1 世代だけローテートし、
  **ローテートした事実は新ファイルの先頭に必ず書く**（silent に捨てない）。
- 主要な行:

  | 行 | 何が読めるか |
  |---|---|
  | `dial 成功（所要 …）` / `dial 失敗（所要 …）: …` | relay へ届いているか |
  | `pump 終了 接続=… received=…B idleClosed=…` | **received=0B は「dial は通るが source が bridge を張っていない」**（Wake/grant 側）。`>0` なら「流れていた接続が落ちた」で原因が別 |
  | `cycle 終了 継続=… idleClosed=… backoff a→b` | 再接続が回っているか止まっているか |
  | `PutRelayGrant 失敗` / `Wake 失敗` | 元は戻り値を捨てていた best-effort 経路（画面には出さずログにだけ残す） |
  | `watchLifecycle: <理由> → forceClose` | スリープ復帰 / NIC 変化の能動検知が効いたか |

- ⚠ ログを開けなくても attach は動き続ける（↗窓 の表示と入力がログ都合で壊れる方が
  害が大きい）。ただし黙って諦めず画面に 1 行出す。

### 6.3 注入 pane の版数追随（`attach-version`）

**注入 pane（↗窓）の中身は `<selfExe> attach <pc> <sid>` という別プロセスで、親は
herdr＝drover daemon が exit/再起動しても入れ替わらない。** よって `attach.go` の
変更は **pane を作り直さない限り反映されない**。ローカル配信手順が
`pkill -f 'herdr-drover attach'` → `launchctl kickstart -k` を要求してきたのはこれが
理由で、**遠隔 `self-update` / `update-all` はこの pkill をしないため attach.go の
変更が他 PC に永久に届かなかった**（実測 2026-07-26・v0.5.28 の BUG-3 修正が owner 機
でしか効いていなかった）。

そこで daemon は起動時に「前回この処理を回した版数」を `attach-version` と比較し、
**変わっていた起動の 1 回だけ既存の注入 pane を撤去する**（撤去は
`DROVER_INJECT_REMOTE=off` と同じ desired=∅ 経路。再生成は起動時 reconcile）。

- **版数が同じ起動では何もしない**＝通常の daemon 再起動で ↗窓 を瞬断させない。
- スタンプ不在（この仕組み以前のバイナリが作った pane が残る起動）は**作り直す側**に倒す。
- 版数が空（ldflags を焼かない素の `go build`）は**判定しない**＝毎起動の瞬断を避ける。
- 撤去が完走しなかった周はスタンプを更新しない＝次回起動で再試行する。
- ⇒ **`attach.go` を変えたリリースでも遠隔 `self-update` だけで配れる**（v0.5.29〜）。

---

## 7. 不変条件（壊してはいけない性質）

1. **exact-match identity のみ**。ヒューリスティック分類はしない。曖昧なら
   skip して**必ず報告**する（silent skip 禁止）。
2. **↗窓 注入 pane は常に対象外**。identity token（`drover_inj_pc` /
   `drover_inj_sid`）で構造的に除外する。除外は `resolveAgentKind` の**最優先段**に
   集約済み（v0.5.23。shim / restart・update / organize が共有）。producer だけは
   別経路なので独自に除外する＝**新しい pane 列挙経路を足すときは
   `resolveAgentKind` を通すこと**。
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
11. **identity ≠ 破壊してよい対象**。pane を作り直す操作（restart / update）は
    `agentid.Resolve` に加えて `agentid.IsDirectInvocation`（`launch_argv[0]` の
    basename）を必ず確認する。wrapper 起動 pane に resume 引数を足しても
    エージェントに届かず、会話を失ったまま成功と報告してしまう。
12. **未命名 pane に勝手に命名しない**。herdr UI 起動の pane を再起動しても
    agent 名を付けない（頼まれていない identity 変更になる）。
13. **エージェント種別を跨いで attach しない**。シムの候補選定・resume backstop・
    命名はすべて種別を一致させる。跨ぐと「別のエージェントに自分の会話を覗かせる」
    ことになり、静かに起きる（実バグ: `gemini` シムが cwd 一致の claude
    セッションへ接続した。v0.5.23 で修正・回帰テスト済み）。
14. **バイナリ解決は新規起動が要ると分かってから**（遅延化）。無条件に解決すると
    「ローカル未導入のエージェントの既存セッションへ attach だけしたい」が失敗する。
15. **旧命令名を allowlist から外さない**。まだ更新していない PC の Web/CLI から
    投げられなくなる（先行デプロイの意味が消える）。

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
