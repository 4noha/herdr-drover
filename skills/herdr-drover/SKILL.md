---
name: herdr-drover
description: Operate and repair a herdr terminal session fleet from any Claude session — diagnose with herdr-drover status, repair duplicated/zombie tabs after a reboot, and choose correctly between organize / restart-agent-session / restart-daemon. Use when the user says things like "herdr のタブを修復して" "再起動したのでタブを直して" "herdr の調子がおかしい" "セッションが空 pane になった" "repair the herdr tabs" or asks what state the herdr fleet is in. Requires the herdr-drover plugin (github.com/4noha/herdr-drover).
---

# herdr-drover — herdr セッション群の診断と修復

## 前提

- `herdr` は Homebrew 側にあるので、Bash tool では毎回 **`export PATH="/opt/homebrew/bin:$PATH"`** を付ける
  （付けないと `herdr: command not found` になる）。`herdr-drover` は `~/.local/bin/herdr-drover`。
- ndjson API は **1 接続 = 1 リクエスト**。CLI は毎回 JSON を 1 行返すだけなので `python3 -c` で整形して読む。

## まず状況を掴む（破壊的操作の前に必ず）

```sh
herdr-drover status     # daemon 生存・herdr 接続・設定（PC_ID は必ず <host>-herdr）
herdr pane list         # 全 pane の agent / agent_status / agent_session / cwd
herdr workspace list    # ws ごとの tab 数
```

`pane list` の読み方（**この 2 つの区別が修復の全て**）:

| 見え方 | 意味 |
|---|---|
| `agent` あり ＋ `agent_status` が idle/working/blocked/done | **本物の agent が生きている pane** |
| `agent` 無し ＋ `agent_status: "unknown"` | agent がいない pane（復元直後の殻・素シェル・ビューア） |
| `agent_session.value` | 会話 ref。**同じ値を持つ pane が 2 枚あれば重複** |
| `tokens.drover_inj_pc` あり / label が `↗` 始まり | **リモート注入 pane。絶対に触らない** |
| `tokens.drover_inj_cwd` | 注入 pane の**リモート側の実 cwd**。⚠ 注入 pane の `cwd` フィールドは中立パス（`~/.herdr-drover`）で**出自の判断に使えない**。他セッションへ命令を投げる相手を選ぶときは**必ずこの token を読む**（title の `↗pc:実パス` を parse しない。token が無ければ「不明」として扱い `cwd` へ fallback しない） |

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
2. **新 pane が `blocked` なら先に解除する**。多くは agent 起動時の
   「Is this a project you trust?」で止まっている。`herdr pane read <pane> --source visible
   --lines 20 --format text` で**画面を必ず読んでから** `herdr pane send-keys <pane> enter`。
   解除後、`agent_session.value` が旧 pane と一致することを確認する
   ＝**会話 resume 成功の証拠**。ここが通るまで次へ進まない。
3. **旧 pane を閉じる**。`herdr pane close <pane_id>`。本物の agent は herdr server の
   子プロセスなので巻き添えにならない。**まず 1 枚だけ閉じて双子が無傷なことを実測してから**
   残りを一括する（万一巻き添えなら被害が 1 枚で済む）。
4. **`focused: true` の旧 pane は最後**。先に `herdr tab focus <新 tab_id>` で視界を実 pane へ
   移してから閉じる（いきなり閉じるとユーザーの画面が飛ぶ）。
5. **双子のいない旧 pane は「重複」ではなく「再起動で失われたセッション」**。閉じてよいが、
   会話が戻らないことを**明示的に報告する**（黙って畳まない）。

最後に `herdr pane list` で「1 セッション = 1 tab」になったこと、
`ps` で残骸シムが居ないことを確認して報告する。

## コマンドの使い分け

| やりたいこと | コマンド |
|---|---|
| Tab を wsmap ルールの Workspace へ整理 | `herdr-drover organize --dry-run` → 良ければ `organize` |
| 新バイナリを掴ませたい（会話は維持） | `herdr-drover restart-agent-session --dry-run` で計画確認 → 実行 |
| daemon 自体の再起動 | `launchctl kickstart -k gui/$(id -u)/com.4noha.herdr-drover` |
| このセッションの Tab を別 WS へ | `mv-tab` skill（`herdr-drover mv-tab --self --dst-ws-label <label>`） |

`restart-agent-session` は `agent_status=working` を既定 skip、同居 pane のある Tab も skip する。
**skip されたものは必ずユーザーに列挙して伝える**（silent に飛ばさない）。

## 絶対にやってはいけないこと

- **裸の `pkill herdr` / `pkill -f herdr`**（過去にユーザーの実サーバを殺した事故あり）。
  止めるなら `herdr server stop`、それ以外は**自分が spawn した PID だけ**を対象にする。
- **`↗` 付き pane / `tokens.drover_inj_pc` を持つ pane を閉じる・触る**（他 PC のリモート注入窓）。
- **herdr の外の端末から張られたビューアを壊す**。`herdr terminal session control <pane>` を
  持つプロセスがあれば、その pane は WezTerm 等から見られている＝pane を閉じない。
- **推測で直す**。「動かない」はまず `pane read` で画面を見て実再現してから触る。
- **silent な skip / 丸め**。閉じた・飛ばした・失われたものは必ず数と理由を報告する。

## 入力を送るときの実測 trap（herdr 0.7.4）

- テキスト送信は **`herdr pane send-text`**（`send_input` は `\r` が落ちる）。
  送ってから `pane read` で入力欄に入ったことを確認し、`herdr pane send-keys <pane> enter` で確定。
- **送信成否は「文字が消えたか」ではなく応答の有無で判定する**
  （`working` に変わった／`Mulling…` が出た等）。
- 他のセッションへ用件を転送したときは、**送った先とフォーカスの戻し先をユーザーに報告する**。
