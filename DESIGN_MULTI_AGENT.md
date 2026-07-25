# マルチエージェント対応 設計（Claude 専用箇所の棚卸しと一般化仕様）

herdr-drover は現状 **Claude Code 専用**に作られている部分がある。別のコーディング
エージェント（codex / cursor / gemini 等）を導入するにあたり、

- **(a) claude 専用でよい箇所**（一般化しない）
- **(b) 一般化が必要な箇所**
- **(c) 既にエージェント非依存な箇所**（触らない）

を実コードから確定し、一般化の設計方針をまとめる。

機能・インターフェース仕様は [SPEC.md](SPEC.md)、設計背景は [DESIGN.md](DESIGN.md)。

**調査方法**: 6 サブシステムを並列調査し、各主張を別エージェントが実コードで
敵対的に検証、さらに統合時に作業ツリーで再検証した（herdr-drover v0.5.20 /
drover-cloud v0.1.10 / herdr 0.7.4）。「推測」と明記した箇所以外はコードの事実。

---

## 1. 前提: herdr は既にマルチエージェント基盤である

**これが設計の出発点**。herdr 側は複数エージェントを前提に作られており、
drover が追いついていない、という構図。

### 1.1 herdr が提供しているもの

| 機能 | 実装 | 網羅 |
|---|---|---|
| **agent 種別の自動検出** | `src/detect/mod.rs` の 21 variant enum。`lookup_agent` がプロセス名 alias 表で判定。node/bun/python/sh 等の generic runtime からも抽出 | **21 種** |
| **状態判定ルール** | `~/.local/state/herdr/agent-detection/remote/*.toml`（remote 更新される manifest。画面スクレイプの regex 群） | **19 種** |
| **共通状態語彙** | `AgentStatus` = idle / working / blocked / done / unknown | 全 agent |
| **会話 session の追跡** | `agent_session{source, agent, kind(id\|path), value}` | **14 種** |
| **resume argv の知識** | `src/agent_resume.rs` の `plan()` | 14 種（**API 非公開**） |
| **hook 資産の同梱・install** | `src/integration/assets/` の 14 ディレクトリ | 14 種 |
| **任意 agent ラベルの受理** | `normalize_reported_agent_label`（既知は canonical 化、未知は trim して素通し） | 無制限 |

canonical label 21 種（`-` を含むものは無い）:

```
pi claude codex gemini cursor devin agy cline omp mastracode opencode
copilot kimi kiro droid amp grok hermes kilo qodercli maki
```

⚠ variant 名と label が違うものがある: `Antigravity → "agy"`,
`GithubCopilot → "copilot"`。

### 1.2 対応レベルの三層構造（重要）

**検出できる ≠ 会話を再開できる**。herdr 内部でも 3 層に分かれている:

| 層 | 対象 | 意味 |
|---|---|---|
| 検出のみ（21 種） | + `gemini` `agy` `cline` `kiro` `amp` `grok` `maki` | pane.agent には出るが **agent_session が付かない＝resume 不能** |
| 状態判定あり（19 種） | manifest がある | working/idle/blocked を画面から判定できる（`omp` `mastracode` は manifest 無し＝hook 専用） |
| session 追跡あり（14 種） | allowlist 通過 | `--resume` 相当が可能 |

**drover の設計はこの三層を明示的に扱う必要がある**。「対応エージェント」と一言で
括ると、resume 不能な agent に resume を試みて壊す。

### 1.3 herdr が提供しないもの（drover が実装する）

| 必要なもの | 状況 |
|---|---|
| **resume argv テーブル** | herdr は内部に持つが **ndjson API 非公開**＝drover にミラーが必要（二重管理。herdr の更新で乖離しうる） |
| **バイナリ名・既定配置の解決** | herdr は `integration/registry.rs` に表を持つが API 非公開 |
| **本体の更新手段** | herdr は関知しない |
| **agent 名の採番・cwd 一致 attach・picker** | drover 固有の UX |
| **クラウド同期スキーマ・遠隔命令** | drover 固有 |
| **注入 pane の identity と除外** | drover 固有（token） |

### 1.4 境界上の注意: drover は herdr の状態を書き換えている

`mirrorInjectedAgent`（`reconcile.go`）が `pane.report_agent(source="drover-inj",
agent=<リモートの window_name>, state)` を打つと、herdr 側で
`hook_authority.agent_label` になり、`effective_agent_label()` が hook を最優先
するため **`pane.list` / `agent.list` の `agent` が転記値そのものになる**。
つまり drover は herdr の agent 種別フィールドの**書き手でもある**。

- `source="drover-inj"` は `is_official_agent_source` に当たらない。おかげで
  **注入 pane には agent_session が絶対に付かない**＝resume 対象に混ざる余地が
  構造的に無い。**この source 名は変えてはいけない**
- この経路は `DROVER_MIRROR_AGENTS`（既定 false の opt-in）で on/off される

---

## 2. 分類済みインベントリ

### 2.1 (a) claude 専用でよい

| 場所 | 事実 | 扱い |
|---|---|---|
| `~/.claude/hooks/herdr-agent-state.sh` | `HERDR_INTEGRATION_ID=claude` / `source="herdr:claude"`。Claude Code の hook JSON スキーマに完全依存。**herdr の `integration install` が生成する管理ファイル**で、drover の Go コードは `~/.claude` を一切参照しない | **drover は何もしない**。他エージェント分は herdr が 14 種同梱 |
| `skills/mv-tab/SKILL.md` | SKILL.md は Claude Code 固有の拡張機構 | 一般化ではなく**並列成果物の追加**。呼び先 `mv-tab --self --dst-ws-label` は完全に非依存 |
| TODO.md の hook 実測記録 | claude pane の中から `claude -p` を走らせると agent_session が壊れる（seq=time.time_ns 上書き） | 他エージェントでは**再実測が必要** |

### 2.2 (b) 一般化が必要（サブシステム別）

#### B1. シム入口・命名・attach（`claudeshim.go` / `main.go`）

| 項目 | 事実 |
|---|---|
| バイナリ解決 | `exec.LookPath("claude")` の literal 固定。エラー文も固定 |
| **無条件実行** | `lookupClaude` が**全経路の先頭で走る**。既存 pane へ attach するだけでも PATH に無ければ shim 全体が exit 1 |
| 命名 encode | `claude` / `claude-N`（`agent_name_taken` の exact-match で採番・上限 64） |
| 命名 decode | `isClaudeAgentName`。**3 サブシステムの共有 decode**（shim の attach/picker・restart/update・organize/capture/learn） |
| 候補選定 | `claudeCandidates` が `isClaudeAgentName(name) && cwd 一致`。**herdr UI から直接起動された agent は `Name=""` で候補外** |
| dispatch | `case "claude"` とエラー前置が固定 |
| usage / 文言 | `alias claude='herdr-drover claude'` を含め claude 固定 |

#### B2. 会話再開（resume）

| 項目 | 事実 |
|---|---|
| 引数抽出 | `--resume` / `-r` / `--resume=` の 3 形のみ |
| 値の妥当性 | `isUUID`（36 文字・区切り位置固定・hex）。**非 UUID 形の session id では backstop が silent に無効化**。herdr 側は「非空・512B 以下・制御文字なし」しか要求しない |
| pane 突合せ | `AgentSession.Kind == "id" && Value == uuid` のみで **`Agent` / `Source` を見ていない**。`Kind=="path"`（pi/omp）は構造的に不一致 |
| argv 生成 | `--resume <uuid>` / `--model <alias>` を literal 出力 |
| 非対称 | resume は `-r` 短縮形を扱うのに model は扱わない（claude CLI 表面への張り付きの証拠） |

#### B3. 対象選定 identity（`restartclaude.go`）

| 項目 | 事実 |
|---|---|
| identity 条件 | `isClaudeAgentName(a.Name)` の **1 系統のみ**（organize の 2 系統 OR と非対称） |
| **既存の穴** | herdr UI から直接起動した claude セッションは restart/update の全経路から**今すでに**除外されている（organize は検出系統で拾うのに lifecycle だけ拾っていない） |
| 改名 fallback | preferred 名が taken なら別番号へ改名＝他エージェント pane が `claude-N` に化ける経路（現状は到達不能だが一般化で顕在化） |
| 対象 0 件 | `nil, nil` で成功扱い。`update-all` は段 2 へ進んで `done` で Ack される＝「更新成功なのに 1 つも再起動されていない」が履歴上 done で残る |

#### B4. 本体更新（`updateclaude.go`）

| 項目 | 事実 |
|---|---|
| 更新手段 | `<bin> update` サブコマンド固定 |
| 版取得 | `<bin> --version` 固定 |
| well-known パス | `~/.local/bin/claude` 固定 |
| タイムアウト | 15 分を `runUpdateAll` **全体**に 1 本掛け＝N エージェントで予算を食い合う |
| 曖昧 error | argv[0] が 2 種類以上で error。**identity を広げた瞬間に恒常発火する順序依存の地雷**（`claudeBinsFromPanes` が agent ラベルを落とした `[]string` を返すのが根因） |

#### B5. organize / capture / learn

| 項目 | 事実 |
|---|---|
| 同定 | 2 系統 OR（シム命名 / `p.Agent == "claude"`）＋矛盾判定。**注入 pane 除外は実装済み**（v0.5.20 で修正） |
| wsmap キー | cwd のみ。agent 次元が無い |

#### B6. 型の欠落（**一般化の根本ボトルネック**）→ **v0.5.22 で解消済み**

~~`internal/herdrapi` の `PaneInfo` に `agent` が無い。`AgentInfo` に
`agent`/`agent_session`/`tokens` が無い~~ → **P0 として実装済み**:

- `PaneInfo.Agent` / `AgentInfo.{Agent,Title,Tokens,AgentSession}` を追加
- organize の `orgPane` 二重 decode を廃止し `herdrapi.PaneInfo` に一本化
- `selectRestartTargets` の pane.list join を廃止（agent.list 単独へ）。
  **join は冗長なだけでなく競合の窓だった** — herdr の ndjson は
  1 接続=1 リクエストなので 2 往復の間に構成が変わりうる
- 注入 token キーの literal 二重定義を `herdrapi.InjTokenPC/SID` 参照へ統一

実測の裏取り: 全 15 pane で agent.list と pane.list の照合フィールド
（agent / agent_status / tokens / agent_session / tab_id / workspace_id / cwd /
terminal_id）が**完全一致**、pane_id 集合も一致、`name`(agent.list) ==
`label`(pane.list)。回帰テスト `TestAgentListCarriesTokensAndSession` が
実 herdr で tokens/agent_session の実在を担保する（JSON タグを壊すと FAIL する
ことを確認済み＝「型だけ足して実は空」を検出できる）。

⚠ 以後 **新しいフィールドが要るときは herdrapi に足す**こと。ローカル型を
再び生やすと同じ二重管理に戻る。

#### B7. クラウド同期スキーマ

session doc に **`agent` フィールドが無い**（[SPEC.md](SPEC.md) §4.1）。Web は
どのセッションがどのエージェントか判別できない。

#### B8. 遠隔命令の wire と Web UI

- 命令名に agent 名が焼き込まれている（`restart-claude` / `update-claude`）
- **Command に `agent` パラメータが無い**（`cmd` と `sid` のみ）
- Web の「⟳claude」ボタン・確認文言が claude 固定

#### B9. テスト基盤

`ReportAgentSession` の `(source, agent)` が herdr の 14 組 allowlist 外だと、
herdr は session_ref を **silent に None** にする。既存テストを機械的に別 agent
へ置換すると、**テストは緑のまま何も検証しなくなる**。鉄則②「修正前に旧コードで
落ちることを確認」が空振りする典型。

### 2.3 (c) 既に非依存（触らない）

要点のみ（詳細は調査記録）:

- **ローカルビューア** `localview.go` — ファイル全体で `claude` の出現はコメント
  1 箇所のみ
- **注入（↗窓）一式** — 選定条件に agent 種別が一切無い。identity token は
  `(pc, pane_id)` の 2 値のみ
- **bridge / 入力** — 分岐は `utf8.Valid` のみ
- **restart の骨格** — layout 操作・tab 移動・生存確認・二重起動拒否は argv の
  中身を解釈しない
- **wsmap の解決ロジック** — package doc のみ claude を謳う
- **mv-tab** — `claude` の grep は全てコメントと変数名。実体は「Tab の最初の
  pane」を採るだけ
- **producer / 通知 / クラウド state** — 除外は pane_id 空と注入 pane のみ。
  通知は herdr の共通 agent_status 遷移で駆動
- **自己更新** `update.go` — エージェントの知識ゼロ

### 2.4 grep 偽陽性（変更禁止）

| 文字列 | 実体 |
|---|---|
| `claude-master` | 前身プロダクト（cm）の実行ファイル名。cm 互換 enroll 経路 |
| `agent_kind: "herdr-drover"` | 「↗窓 に応答できる drover が居る PC か」の製品マーカー。**コーディングエージェント種別ではない** |
| `restart-agent` | **herdr-drover デーモン**の kickstart |
| `input.go` の claude/DECSET 2004 | `pane.send_input` を棄却した理由の記録 |

---

## 3. 一般化の設計方針

### 3.1 エージェント種別をどこで持つか

権威は 3 系統あり、**どれか 1 つでは足りない**。

| 系統 | 供給元 | 網羅 | 信頼性 |
|---|---|---|---|
| (i) herdr の検出値 `pane.agent` | canonical label 21 種 | 広い | **中** — hook_authority が最優先されるので外部（drover 自身の `mirror_agents` を含む）が上書きできる |
| (ii) `agent_session.{source,agent}` | 14 組 allowlist を通ったものだけ | 狭い | **高** — source が `herdr:<agent>` の official に限られる |
| (iii) drover のシム命名 | drover が起動した pane のみ | 最狭 | **高** — 自分で書いた値 |

**設計**: 同定を 1 箇所へ集約し、優先順位付きで解決する。

```go
// 提案: cmd/herdr-drover/agentid.go
func ResolveAgentKind(p herdrapi.PaneInfo, shimName string) (kind, conflict string)
//  0. Tokens[InjTokenPC]/[InjTokenSID] があれば ("", "")   // 注入 pane は常に対象外
//  1. AgentSession.Source == "herdr:"+AgentSession.Agent なら その Agent  // 最強
//  2. shimName の decode 成功 → その kind
//     - 1 と矛盾したら conflict（機械確定不能＝skip＋報告）
//  3. p.Agent が既知 21 label に exact-match したときのみ採用  // 未知文字列は捨てる
//  4. どれも無ければ ("", "")
```

**ステップ 3 の「既知 21 label への exact-match 必須」が要点**。`p.Agent` には
drover 自身が `mirror_agents` で書いた `claude-2` のような任意文字列が入りうる
（herdr は未知ラベルを trim して素通しする）。

### 3.2 「会話の再開」の抽象

herdr の内部テーブル（`agent_resume.rs` の `plan()`・全文再検証済み）:

| agent | argv 形 | Kind |
|---|---|---|
| claude, devin, droid, hermes, qodercli | `<bin> --resume <v>` | id |
| codex | `codex resume <v>`（**位置引数サブコマンド**） | id |
| copilot, omp | `<bin> --resume=<v>` | id（omp は path も） |
| pi | `pi --session <v>` | id / path |
| kimi, opencode, kilo | `<bin> --session <v>` | id |
| mastracode | `mastracode --thread <v>` | id |
| cursor | **`cursor-agent` --resume `<v>`**（argv[0] が agent 名と違う） | id |
| gemini, agy, cline, kiro, amp, grok, maki | **resume 不能**（allowlist 外） | — |

**この表は API 非公開**なので drover にミラーを持つしかない。

```go
type ResumeSpec struct {
    Flag       string   // "--resume" / "--session" / "--thread" / ""
    Aliases    []string // claude の "-r" など strip 対象
    Form       Form     // FormSpace | FormEquals | FormSubcommand
    Subcommand string   // codex の "resume"
    Argv0      string   // 空なら agent 名。cursor は "cursor-agent"
    Kinds      []string // {"id"} / {"id","path"}
    Supported  bool     // false = resume 非対応（loud に skip）
}
```

**置き換える 3 点**（骨格はそのまま活かせる）:

1. resume 引数の抽出 → Spec 駆動。**値を落とすか否かは「そのフラグが値を取るか」
   で決め、値の書式では決めない**（`isUUID` 判定を廃止）
2. session ref の妥当性 → herdr 相当（非空・512B 以下・制御文字なし／絶対パス）へ緩和
3. pane 側の照合 → `AgentSession.Agent == kind` を AND に追加し、`Kind=="path"` も扱う。
   `ResumeUUID string` を `Session herdrapi.AgentSession` へ置換

⚠ `FormSubcommand`（codex）は「フラグ 1 個を落とす」現行構造では表現できない。
「argv[1] が resume サブコマンドならそこから 2 語落とす」が必要。

⚠ **resume 非対応 7 種は「未実装」ではなく原理的に不可能**（herdr が
agent_session を出さない）。restart 時は resume 無しの素起動へ落とし、その旨を
loud に出力・Ack detail に残す。

### 3.3 「本体の更新」の抽象

```go
type UpdaterSpec struct {
    VersionArgv []string      // 既定 ["--version"]。持たない CLI では nil
    UpdateArgv  []string      // claude: ["update"]。nil = 自己更新口なし
    Timeout     time.Duration // 既定 15 分（claude 実測 ~250MB 由来）
}
```

- **維持すべき良い設計**: 更新有無の権威は「前後の `--version` 文字列比較」で
  exit code に依存しない。出力書式もパースしていないので移植可能
- `VersionArgv` が nil / probe 失敗 → 「版比較 skip・更新有無不明」と明示ログして
  再起動段へ進む分岐が要る（現行は更新前に return）
- `UpdateArgv` が nil → 更新段を skip し「更新口なし（再起動のみ）」と loud にログ
- **Timeout は per-agent 予算にする**（現行は全体に 1 本掛け）
- 1 エージェントの失敗で他と自己更新を止めるかは**新たな設計判断**（現行は全停止。
  複数では「失敗を集約して自己更新は続行」が実運用に合う可能性が高い＝推測）
- **「自己更新は必ず最後」の順序不変条件は agent 非依存＝そのまま維持**

### 3.4 「本体バイナリの解決」の抽象

```go
type InstallSpec struct {
    BinNames       []string // agent 名 ≠ 実行名。cursor→["cursor-agent"]
    WellKnownPaths []string // claude→["~/.local/bin/claude"]
    HerdrDetectsAs string   // ★ herdr の lookup_agent 表に載る basename
}
```

権威順は現行維持: (1) 稼働 pane の argv[0]（最も正確・PATH 非依存）→
(2) `LookPath(BinNames...)` → (3) WellKnownPaths。決まらなければ**推測せず error**。

**`HerdrDetectsAs` が最重要**。herdr の検出は**前景プロセス名基準**で、alias 表に
載らない basename で起動すると **`pane.agent` も `agent_session` も一切付かない**
＝resume backstop も organize の検出系統も silent に無効化される。drover 自身が
これを実測しており、テストで「exec するとプロセス名が変わって herdr の検出に
載らない」と記録して stub をわざと exec しない実装にしている。

alias 表の例: `claude|claude-code`, `cursor|cursor-agent`,
`copilot|github-copilot|ghcs`, `kilo|kilo-code`, `agy|antigravity`。
**`BinNames` は必ずこの表の要素にする**（Spec ロード時に静的検証するのが安全）。

**`lookupClaude` の遅延化**: 現在は無条件に走るため「ローカル未導入の
エージェントのセッションへ attach だけしたい」が失敗する。バイナリ解決を
新規起動経路まで遅延させる。

### 3.5 遠隔命令の命名とパラメータ

**命名**（旧名は allowlist に残して alias 扱い＝`agent="claude"` 固定へ写像）:

| 現行 | 一般化後 |
|---|---|
| `restart-claude` | `restart-agent-session` |
| `update-claude` | `update-agent-cli` |
| `update-all` | **改名不要**（意味を「導入済み全エージェント → drover 自己更新 → 再起動」へ拡張） |
| `restart-agent`（= drover デーモン） | `restart-daemon` へ改名推奨（**語彙衝突**。一般化後は "agent" がコーディングエージェントを指す語になる） |

**パラメータ**: agent 種別を運ぶ経路が全区間で塞がっているので 4 か所を同時に開ける:

1. `state/commands.go` の `Command` に `Agent string`（json は snake_case＝
   devices.js との契約）
2. **map リテラルにも `"agent": agent` を追加**（struct タグだけでは書かれない）
3. `web/web.go` に `r.FormValue("agent")` と `PushCommand(..., agent, ...)`
4. `restartOptions` に `Agent string`（""=全 agent）

`Agent` が空の解釈は「その PC の全エージェント」（sid 空と同型）。ただし
**どう解釈したかを必ず 1 行出力**する。旧 doc に agent が無い場合は空で degrade し、
Ack detail に「agent 未指定＝旧互換解釈」と残す。

⚠ **デプロイ順序**: Cloud Run を**先に**デプロイして新命令名を allowlist へ追加
→ その後 agent 側。逆順だと `未知のコマンド` で投入できない。

### 3.6 agent 名の encode/decode

一般化: `<agent>` / `<agent>-N`。

- **区切り文字の衝突は起きない**: canonical label 21 種は**どれも `-` を含まない**
  （`claude-code` / `cursor-agent` は lookup_agent の**入力 alias** であって
  `pane.agent` に出る値ではない）。既知集合への exact prefix で一意に decode できる
- 採番上限 64 は **agent ごとの名前空間**にする
- **herdr の agent 名は全 agent 共有のグローバル一意制約**。改名 fallback が
  別 agent を `claude-N` に化けさせないよう agent を引数で受ける
- ⚠ herdr の target 解決は `agent_name == target || 検出 label == target` の **OR**。
  name=`codex` の pane と検出 label=`codex` の別 pane が同時に存在すると
  `herdr agent focus codex` が Ambiguous になる（**推測**: 実運用で発生しうる。
  実測が必要）
- **より堅牢な方向（推奨）**: identity の第一権威を命名から
  `AgentSession.{Source,Agent}` へ移し、命名は「drover が起動した pane の目印」へ格下げ

### 3.7 スキーマ拡張の波及

**朗報**: session doc は端から端まで完全 pass-through。`ListSessions` は
`d.Data()` をそのまま返し、web はそのまま JSON encode、`contentHash` は
予約キー以外の全キーを自動的に含む。

したがって producer の `sess` map に **`"agent"` を 1 キー足すだけで
Firestore→Web まで中間層の改修ゼロ**で届く。影響は「初回 1 回だけ content_hash が
変わって write が 1 回増える」だけ。値が空なら載せない（後方互換）。

同時に `window_name` の優先順を
`agent.list の Name → pane.agent → display_agent → pane.Label → pane_id` にすれば、
シム経由でないエージェントも意味のある名前を得る。

`wsmap` に agent 次元を足すかは**明示的な設計判断が必要**:

- 案 A: `exact: {"<agent>|<cwd>": label}`。`wsmap.Parse` は
  `DisallowUnknownFields` なので version フィールド等で明示移行
- 案 B: 「agent 非依存の 1 cwd = 1 WS」を仕様として固定し、書き手が食い違ったら
  loud に SKIP

### 3.8 推奨する着手順序

| 段 | 内容 | 理由 |
|---|---|---|
| ~~P0~~ | ~~herdrapi 型に `agent`/`agent_session`/`tokens` を追加、organize の二重 decode を廃止~~ **完了（v0.5.22）** | これ無しでは他の全部が命名規約に依存し続ける |
| P1 | `ResolveAgentKind` の一元化（3 系統を統合） | identity の単一地点化 |
| P2 | producer に `agent` を載せる（空なら載せない） | Web 出し分け・per-agent 命令の入力 |
| P3 | Cloud Run に新命令名を allowlist 追加（旧名残置）して**先行**デプロイ | 順序を誤ると命令が全滅 |
| P4 | Spec テーブル（Resume/Updater/Install）導入 + CLI/遠隔命令の `--agent` | |
| P5 | Web UI の出し分け・文言動的化 | |
| P6 | シム入口の一般化（`herdr-drover shim <agent>` or argv[0] multi-call） | 表面契約なので最後 |
| P7 | README/SETUP/DESIGN/**TODO.md** の同時更新 | TODO.md は「正」 |

---

## 4. 移行時の危険（壊れやすい不変条件）

1. **encode/decode の厳密往復を片側だけ変えると 3 サブシステムが黙って対象 0 件に
   なる**。`isClaudeAgentName` は shim・lifecycle・organize の共有 decode。
   encode と対で変えること。

2. **注入 pane の除外は 3 系統に散在している**（producer / restartclaude /
   organize）。追加も削除も忘れると「↗窓 を再起動する」「偽 cwd をルール化する」等の
   実害が出る。**新しい pane 列挙経路を足すときは必ず除外を入れる**。
   ~~⚠ `restartclaude.go` は token キーを literal で二重定義している~~
   → v0.5.22 で `herdrapi.InjTokenPC/SID` 参照へ統一済み。判定は
   `hasInjectToken(tokens)` の 1 関数に集約（pane.list / agent.list 共通）。

3. **注入 pane の `cwd` は偽値**。ルール書込経路（capture / learn）で拾うと
   wsmap が汚染される。

4. **`mirror_agents` を有効にすると `pane.agent` が汚染される**（§1.4）。
   `pane.agent` を identity に格上げする設計は「検出値」ではなく
   「検出値 ∪ 外部申告値」を読むことになる。**既知 21 label への exact-match を必須に**。

5. **単独 pane の Tab はプロセス終了で自動 close される**。restart の 2 段
   フォールバックはこの herdr 挙動と、「復元不能な session を指すと CLI が即 exit
   する」という claude の挙動に依存する。**不正な session 参照で即死せず対話 picker
   やエラープロンプトに落ちるエージェントでは、壊れた pane を `done` として報告する**
   （フォールバックが発火しない）。他エージェント導入時は**再実測が必要**。

6. **`resolveClaudeBin` の「argv[0] が 2 種類以上で error」は identity 拡大と
   同時に恒常発火する**。agent 別グルーピングを同じコミットで入れること。

7. **15 分タイムアウトの共有**。N エージェントで予算を食い合い、後段が
   `signal: killed` になる（しかも親切なメッセージは claude 名で出る）。

8. **`agent_kind: "herdr-drover"` を流用しない**。「PC がどのコーディング
   エージェントを持つか」は**別キー**（例 `coding_agents: [...]`）で。

9. **テストが silent に無効化される罠**（§2.2 B9）。

10. **herdr の検出はプロセス名基準**。alias 表に無い basename で起動すると検出が
    丸ごと無効化される。`exec` でプロセス名が変わるケースも同様。

---

## 5. 一般化以前に直すべき既存の不整合

分類とは独立に、調査中に見つかった実装とドキュメントの乖離:

| 場所 | 問題 |
|---|---|
| `restartclaude.go` の identity | herdr UI から直接起動した claude セッションが restart/update の全経路から**今すでに**除外されている（organize は拾うのに lifecycle だけ拾っていない） |
| Web「復帰」(restart-proxy) の説明 | 「現在の claude を終了し --resume で別プロセスとして復帰します」が**実装と不一致**。実体は bridge の張り直しのみで claude プロセスにも jsonl にも触れない。allowlist コメント（命令カタログの正）にも同じ誤りがある |
| Web の「pid- は jsonl から UUID を自動解決」 | cm 時代の記述。drover に pid- sid も jsonl 解決も存在しない |
| term.js の画像送信 | UI にあるが no-op（bridge が parse-and-drop） |
| devices.js の diagPre keys | cm 残骸（pid/start_time/usage_percent/reset_time は常に undefined）、`agent_status` が漏れている |
| TODO.md の resume backstop | 「未着手・token 方式」と書いているが実装は agent_session 方式で完了済み |
| README の organize 説明 | 「切り出し」と書いているが現行は Tab ごと引っ越しに格上げ済み |


---

## 6. 参考: 主要ファイル

| ファイル | 役割 |
|---|---|
| `cmd/herdr-drover/claudeshim.go` | シム・命名・resume backstop |
| `cmd/herdr-drover/restartclaude.go` | restart 対象選定・argv 書換 |
| `cmd/herdr-drover/updateclaude.go` | 更新・バイナリ解決・update-all |
| `cmd/herdr-drover/organize.go` | 同定関数の実質的な正・注入除外の見本 |
| `cmd/herdr-drover/reconcile.go` | ↗窓・mirror |
| `internal/herdrapi/types.go` | **P0 の改修点** |
| `internal/session/producer.go` | クラウド session スキーマ |
| `internal/commands/commands.go` | 遠隔命令 dispatch |
| `drover-cloud/state/commands.go` | allowlist・Command スキーマ |
| `drover-cloud/web/{web,ui}.go`, `web/static/{devices,term}.js` | Web UI |

herdr（参照のみ・改変禁止）: `src/detect/mod.rs`, `src/agent_resume.rs`,
`src/api/schema/{panes,agents,common,integrations}.rs`,
`src/app/{creation,api_helpers,terminal_targets}.rs`, `src/terminal/state.rs`,
`src/integration/registry.rs`
