---
name: herdr-drover
description: Operate and repair a herdr terminal session fleet from any Claude session — diagnose with herdr-drover status, repair duplicated/zombie tabs after a reboot, choose correctly between organize / restart-agent-session / restart-daemon, start a new agent Tab from the CLI (tab create → pane run), and send a prompt to another agent pane without losing it (agent prompt / bracketed paste). Use when the user says things like "herdr のタブを修復して" "再起動したのでタブを直して" "herdr の調子がおかしい" "セッションが空 pane になった" "新しいタブで claude を起こして" "あのセッションに指示を送って" "repair the herdr tabs" or asks what state the herdr fleet is in. Requires the herdr-drover plugin (github.com/4noha/herdr-drover).
---

# herdr-drover — herdr セッション群の診断と修復

## 前提

- `herdr` は Homebrew 側にあるので、Bash tool では毎回 **`export PATH="/opt/homebrew/bin:$PATH"`** を付ける
  （付けないと `herdr: command not found` になる）。`herdr-drover` は `~/.local/bin/herdr-drover`。
- ndjson API は **1 接続 = 1 リクエスト**。CLI は毎回 JSON を 1 行返すだけなので `python3 -I -c` で整形して読む。
- **herdr 0.9.5 の CLI は `herdr <group> <sub> --help` が効かない**（全体の usage が出るだけ。実測 2026-11-21）。
  サブコマンド一覧は `herdr tab` / `herdr pane` / `herdr agent` のように**グループ名だけ**で出す。
  フラグは `herdr completion zsh` の出力、API の引数は `herdr api schema --json`（公式スキーマ）、
  herdr 自身の公式ガイドは `herdr --skill` で確かめる。**書式を推測で書かない。**
- 裸の `herdr` は TUI を起こす／attach する＝探索目的で叩かない。

## まず状況を掴む（破壊的操作の前に必ず）

```sh
herdr-drover status     # daemon 生存・herdr 接続・設定（PC_ID は必ず <host>-herdr）
herdr pane list         # 全 pane の agent / agent_status / agent_session / cwd
herdr workspace list    # ws ごとの tab 数・focused
```

`pane list` の読み方（**この 2 つの区別が修復の全て**）:

| 見え方 | 意味 |
|---|---|
| `agent` あり ＋ `agent_status` が idle/working/blocked/done | **本物の agent が生きている pane** |
| `agent` 無し ＋ `agent_status: "unknown"` | agent がいない pane（復元直後の殻・素シェル・ビューア） |
| `agent_session.value` | 会話 ref。**同じ値を持つ pane が 2 枚あれば重複** |
| `tokens.drover_inj_pc` あり / label が `↗` 始まり | **リモート注入 pane。絶対に触らない** |
| `tokens.drover_inj_cwd` | 注入 pane の**リモート側の実 cwd**。⚠ 注入 pane の `cwd` フィールドは中立パス（`~/.herdr-drover`）で**出自の判断に使えない**。他セッションへ命令を投げる相手を選ぶときは**必ずこの token を読む**（title の `↗pc:実パス` を parse しない。token が無ければ「不明」として扱い `cwd` へ fallback しない） |

`blocked` は herdr が承認・質問 UI を認識した状態。`agent` があるのに `unknown` なら「agent はいるが状態を
分類できない」（完了の証拠ではない。`agent` 無しの `unknown` は上の表どおり殻）。`idle` / `done` は入力を受けられる状態だが、見慣れないダイアログは
`idle` のまま止まっていることがある＝**入力前に必ず `pane read` で画面を見る**。

## 再起動後のタブ修復（実測済みの手順）

**症状**: 再起動でレイアウト復元が走ると、pane 内で `herdr-drover <agent> --resume <ref>`
シムが起きる → シムは仕様どおり**新しい Tab** に本物の agent を立て、**元の pane は observe
ビューアに化ける** → Tab が倍増する。ユーザーはこれを「タブが壊れた」と呼ぶ。

> ⚠ **この倍増は BUG-7 として根治実装済み**（2026-09-04 `d9504b2`。**v0.5.37 で
> release 済**＝他 PC は `self-update` / `update-all` で受け取る。mac-studio は
> 先行して同内容をデプロイ済）。新版は「自 pane がその会話の持ち主なら
> **同 pane で exec**」するので Tab が増えない。効くのは
> **`~/.local/bin/herdr-drover`（シムが実行するのはこちら）が新版で、かつその版が
> 起動したセッション**から。倍増を見つけたら、まず
> `~/.local/bin/herdr-drover version` を確認して**どちらの世界なのかをユーザーに
> 報告する**（黙って畳まない）。
>
> ⚠ **倍増するのは一部の pane だけ**（2026-09-04 実測）。herdr が session.json に
> 残すのは pane 生成時の launch argv だけなので:
> - **明示 argv（`~/.local/bin/claude --resume <uuid>` 等）を持つ pane** →
>   herdr server が実バイナリを直接 spawn（親が server）＝**倍増しない**
> - **`command` を持たず `agent_session` だけある pane**（素シェルで `claude` と
>   打った側）→ herdr が自前 resume を**対話シェル経由**で起こす＝`alias claude`
>   がシムに当たり倍増（親が zsh）
>
> だから修復後は倍増が止まって見えることがある。`ps -eo pid,ppid,command` で
> **シムの親が zsh か server か**を見れば、どちらの pane なのかが分かる。

**⚠ `organize` と `restart-agent-session` は修復手段ではない**。配置は正しく agent も既に
走っているので、どちらも何もしない（`organize --dry-run` は全件 KEEP と出る）。

手順:

1. **ペアを突き合わせる**。`agent_session.value` が同じ「unknown な旧 pane」と「agent が
   生きている新 pane」を対にする。旧 pane 側でシムが動いているかは
   `ps -eo pid,ppid,command | grep "terminal session observe"` で確認できる
   （**シムの子**の observe＝ビューア。**daemon(`herdr-drover agent`) の子**の observe は
   クラウド同期用なので無関係）。
2. **新 pane が `blocked` なら先に理由を読む**。多くは agent 起動時の
   「Is this a project you trust?」で止まっている。`herdr pane read <pane> --source visible
   --lines 20 --format text` で**画面を必ず読む**。⚠ Claude Code の信頼ダイアログは
   **カーソルの初期位置が「No, exit」**（実測 2026-11-21・Claude Code 2.1.330。2026-08 時点は
   「Yes」が既定だった）＝**裸の `send-keys <pane> enter` は claude を終了させ会話を失う**。
   修復はユーザーが既に信頼していた cwd の会話を戻す作業なので、画面で「Yes, I trust this folder」
   の位置を確かめてから `herdr pane send-keys <pane> down` → `herdr pane send-keys <pane> enter`
   で解除する（新しい cwd での初回起動の信頼は別＝下の「起こす」手順どおりユーザーに答えてもらう）。
   解除後、`agent_session.value` が旧 pane と一致することを確認する＝**会話 resume 成功の証拠**。
   ここが通るまで次へ進まない。
3. **旧 pane を閉じる**。`herdr pane close <pane_id>`。本物の agent は herdr server の
   子プロセスなので巻き添えにならない。**まず 1 枚だけ閉じて双子が無傷なことを実測してから**
   残りを一括する（万一巻き添えなら被害が 1 枚で済む）。
4. **`focused: true` の旧 pane は最後**。先に `herdr tab focus <新 tab_id>` で視界を実 pane へ
   移してから閉じる（いきなり閉じるとユーザーの画面が飛ぶ）。
5. **双子のいない旧 pane は「重複」ではなく「再起動で失われたセッション」**。閉じてよいが、
   会話が戻らないことを**明示的に報告する**（黙って畳まない）。

最後に `herdr pane list` で「1 セッション = 1 tab」になったこと、
`ps` で残骸シムが居ないことを確認して報告する。

## 新しい Tab でエージェントを起こす（herdr 0.9.5・実測 2026-11-21）

herdr の設計は **1 Tab = agent pane 1 枚**。**先に Tab を作り、その pane で実バイナリを起動する。**

```sh
# 置き先 workspace を label から引く（label は重複可＝一致が 1 件でなければ止めてユーザーに聞く）
WS=$(herdr workspace list | python3 -I -c "
import sys,json; ws=[w for w in json.load(sys.stdin)['result']['workspaces'] if w['label']=='<置き先の label>']
assert len(ws)==1, [w['workspace_id'] for w in ws]; print(ws[0]['workspace_id'])")
# 1. Tab を作る（pane 1 枚付き）。返り値の .result.root_pane.pane_id を使う
PANE=$(herdr tab create --workspace "$WS" --cwd <プロジェクトのパス> --label <Tab 名> --no-focus \
  | python3 -I -c "import sys,json;print(json.load(sys.stdin)['result']['root_pane']['pane_id'])")
# 2. その pane で実バイナリを起動する（引数は分けて渡せる）
herdr pane run "$PANE" ~/.local/bin/claude --permission-mode bypassPermissions
```

- **実バイナリ（`~/.local/bin/claude`・`/opt/homebrew/bin/codex`）を渡す。PATH 上の `claude` や
  `herdr-drover claude`（シム）は渡さない。** シムは「新規は常に新しい Tab」なので、作った pane の
  中で走ると**さらに 1 枚 Tab を作って自分は observe ビューアに化ける**＝1 起動につき 1 枚余る。
  basename が `claude` なら herdr の agent 検出はそのまま効く（`herdr pane get <pane>` に `agent=claude`）。
- **`--workspace` を必ず付ける。** 省くと**いまフォーカスされている workspace**（ユーザーが見ている所）
  に入る。呼び出し側の `HERDR_WORKSPACE_ID` は見ない（client 未接続の隔離 server で実測）。存在しない ID は
  `workspace_not_found` で止まる。`--no-focus` で視界を奪わず、`--label` を付けて自動採番の Tab 名を残さない。
- `pane run <pane> <cmd> [args...]` はコマンドと Enter をまとめて queue するだけ＝成功は「受理」で
  「起動完了」ではない。公式ガイド（`herdr --skill`）ではシェルのプロンプトにいる pane が前提。
- **その cwd で初めて起こすと Claude Code は信頼ダイアログで止まる**（`agent_status=blocked`。
  初期位置は「No, exit」）。**ユーザーに答えてもらう**（上の修復手順 2 と同じ）。答えると
  `bypass permissions on` が出て `idle` になる。
- 確認は `herdr tab get <tab_id>` の `pane_count` が 1、`herdr pane get <pane>` の `agent` と
  `agent_status`。
- **`herdr agent start <name> --kind claude --pane <pane> -- <args>` でも起こせる**（名前付き agent に
  なり `agent prompt <name>` で指せる。`--kind` の一覧は `herdr agent start` の completion に出る）。
  ただし**未信頼の cwd では約 4 秒で終了コード 1・stderr に `agent_not_ready`**（30 秒のタイムアウト
  は待たない）。claude は起動したままで、ユーザーが信頼に答えれば `herdr agent get <name>` が通る
  ＝**終了コード 1 を失敗と決めつけない**。`--cwd` は無い（旧レシピ `agent start <名前> --cwd … →
  pane move --new-tab` は 0.9.5 で `unknown option: --cwd`）。

## 他の pane／エージェントに入力を送る（herdr 0.9.5・実測 2026-11-17 / 11-21）

- **プロンプトは `herdr agent prompt <name|pane_id> "<text>" --wait --timeout <ms>` で送る。**
  herdr が本文を bracketed paste で包み Enter まで 1 回の送信として書く＝多行・1,000 文字超も
  1 メッセージで届く（実測: 19 行 1,042 文字が Claude Code の transcript に 1 件で記録）。
  `blocked`（承認・質問 UI）な agent には `agent_blocked` で**送らずに**止まる。`--wait` は
  idle/done/blocked に落ち着くまで待つ。送信後 5 秒以内に working/blocked が観測できなければ
  `agent_prompt_stalled`＝**再送しない**（届いているかもしれない。`herdr agent read <name>` で画面を見る）。
- **素の `herdr pane send-text` ＋ `send-keys enter` で多行・長文を送らない。** PTY が 1022 バイト
  ずつ渡し、Claude Code は塊ごとに「貼り付けか手入力か」を判定するので、800 文字超では末尾の塊だけ
  別メッセージで送られたり Enter が改行として吸われたりする（2026-11-17 実測。Codex も多行は
  送信されない）。pane 面でどうしても送るなら本文を `ESC[200~ … ESC[201~` で包み、150 ms 置いて
  Enter を別送する。drover の `send-input`（Grok Bot／MCP 経路）は v0.5.57 からこの形。
  なお API の `pane.send_input` は `\r` が落ちる＝`send-text` か `send-keys` を使う（0.7.4 からの trap）。
- `pane read` で入力欄に見える文字は実入力とは限らない（ゴーストサジェスチョン）。
  **送信成否は「文字が消えたか」ではなく `agent_status` が working に変わったか・応答が出たかで判定する。**
- 他のセッションへ用件を転送したときは、**送った先とフォーカスの戻し先をユーザーに報告する**。

## コマンドの使い分け

| やりたいこと | コマンド |
|---|---|
| Tab を wsmap ルールの Workspace へ整理 | `herdr-drover organize --dry-run` → 良ければ `organize` |
| 新バイナリを掴ませたい（会話は維持） | `herdr-drover restart-agent-session --dry-run` で計画確認 → 実行 |
| daemon 自体の再起動 | `launchctl kickstart -k gui/$(id -u)/com.4noha.herdr-drover` |
| このセッションの Tab を別 WS へ | `mv-tab` skill（`herdr-drover mv-tab --self --dst-ws-label <label>`） |
| 既存の Tab を別 WS へ（他人の Tab） | `herdr pane move <pane> --new-tab --workspace <ws_id> --label <Tab 名> --no-focus`（pane_id が変わる。`--tab-label` は `--new-workspace` 専用） |
| 新しい Tab でエージェントを起こす | 上の「新しい Tab でエージェントを起こす」 |
| 別の agent にプロンプトを送る | `herdr agent prompt <name または pane_id> "<text>" --wait`（下の「入力を送る」） |

`restart-agent-session` は `agent_status=working` を既定 skip、同居 pane のある Tab も skip する。
**skip されたものは必ずユーザーに列挙して伝える**（silent に飛ばさない）。

## 絶対にやってはいけないこと

- **裸の `pkill herdr` / `pkill -f herdr`**（過去にユーザーの実サーバを殺した事故あり）。
  止めるなら `herdr server stop`、それ以外は**自分が spawn した PID だけ**を対象にする。
- **`↗` 付き pane / `tokens.drover_inj_pc` を持つ pane を閉じる・触る**（他 PC のリモート注入窓）。
- **herdr の外の端末から張られたビューアを壊す**。`herdr terminal session control <pane>` を
  持つプロセスがあれば、その pane は WezTerm 等から見られている＝pane を閉じない。
- **信頼ダイアログに盲目の `enter` を送らない。** Claude Code は初期位置が「No, exit」なので会話を殺す。
  新しい cwd での初回の信頼（Claude Code の folder trust・codex の「Do you trust…」）はユーザーのもの＝
  代わって答えない。再起動後の修復で既存セッションの信頼を戻すときだけ、画面を読んでから `down` → `enter`。
- **推測で直す**。「動かない」はまず `pane read` で画面を見て実再現してから触る。
- **silent な skip / 丸め**。閉じた・飛ばした・失われたものは必ず数と理由を報告する。
