# DESIGN: slave への一時 SSH エージェント転送（GitHub 操作）

owner（信頼できる自機・Mac）の SSH 秘密鍵を **一切ディスクに置かず**、slave
（共用 PC・同一アカウント・複数人・信用できない人寄り）上で一時的に git/gh の
SSH 認証を行う仕組み。owner の `ssh-agent` を drover-cloud relay 経由で slave に
転送し、**署名は owner Mac 側で実行**する（＝classic SSH agent forwarding を
NAT 越しの relay 上に載せる）。

## 脅威モデルと不変条件

slave は **同一 UID を複数人が共有** ＝ファイルパーミッションで他人と隔離
できない。したがって:

1. **秘密鍵をディスクに置かない**（置いた瞬間に同アカウントの他人が読める＝
   全鍵漏洩）。→ 転送のみ。鍵は owner Mac の agent から出ない。
2. **転送された `SSH_AUTH_SOCK` も同アカウントの他人が掴める**（socket は同 UID）。
   → **`ssh-add -c`（confirm mode）を必須**とし、1 署名ごとに owner Mac 上で
   承認ダイアログを出す。他人が socket を掴んでも無断署名できない。
3. **専用 deploy key を使う**（owner 主鍵ではなく、対象リポジトリだけに限定した
   失効可能な鍵）。万一の被害範囲を当該リポに限定。
4. **一時性**: owner の転送コマンド実行中だけ socket が存在。切断／stop で即撤去。
   grant にも TTL。
5. 既存不変条件を踏襲: **master path はバイト無改変**（relay/state/web の
   owner・master 経路は byte-identical）。slave 認可は既存 slavegrant/SlaveGate
   を再利用（新しい信頼境界を足さない）。

## トポロジ

```
[owner Mac]  ~/.ssh (deploy key) + ssh-agent  (ssh-add -c = confirm)
     │  $SSH_AUTH_SOCK（署名要求のみ流れる。鍵は出ない）
     ▼
  herdr-drover ssh-forward <pc> <sid>
     │  agentfwd mux（DIALER 端）: 新チャネルごとに $SSH_AUTH_SOCK を dial
     ▼
  relay /session?sid=<afSid>&role=viewer&spc=<slavePC>   ← DialViewerFrom
     │  （バイト透過ペアリング。KeyFor が pc 名前空間キーで slave source と突合）
     ▼
  relay /session?sid=<afSid>&role=source  ← slave（slave token / SlaveGate）
     │  agentfwd mux（LISTENER 端）: local unix socket を accept→チャネル化
     ▼
[slave]  ~/.herdr-drover/agent-fwd/<sid>.sock  ← SSH_AUTH_SOCK
     │
   git push git@github.com:...    （ssh が socket 経由で署名要求）
        → 署名は owner Mac の agent が実行（毎回 confirm ダイアログ）
```

`<afSid>` = agent-forward 専用セッション ID。端末バイト線（↗窓/webterm）とは
**別セッション**にして端末ストリームを汚さない（不変条件: 端末はバイト透過）。
命名は派生 sid（例 `<sid>|af`）。`#inj` と同様に URL エスケープ透過。

## agentfwd mux（Phase 1・実装済）

relay の source⇄viewer は **1 対 1・バイト透過**。SSH agent 転送は「短命な接続を
複数（時に並行）」必要とする（`ssh`/git 起動ごとに `$SSH_AUTH_SOCK` へ 1 接続）。
これを **単一の relay パイプ上で多重化**する最小 mux。

- ワイヤ（big-endian）: `[type:1][channel:4][length:4][payload:length]`
  - `type 1 = DATA` / `type 2 = CLOSE`（length 0）。
- **チャネル ID は LISTENER 端（slave＝socket を所有し accept する側）が単調割当**。
  DIALER 端（owner）は未知 ID の最初の DATA で agent を dial（OPEN は暗黙）。
- 単調 ID ＋ 順序保証パイプ ＝ CLOSE 後に同 ID の DATA は来ない。念のため
  DIALER 側は closed 集合で late-DATA の再 dial を防ぐ（防御的）。
- `maxFrame`（512KB）で payload 長を上限（壊れ/悪意パイプの OOM 防御。agent
  メッセージは実際は小さい）。
- パイプ close で全チャネル close。ctx 終了で pipe/listener を畳む。

エントリ:
- `ServeListener(ctx, pipe, ln net.Listener) error` — slave 端。ln を accept し
  各接続をチャネル化。
- `ServeDialer(ctx, pipe, dial func()(net.Conn,error)) error` — owner 端。新
  チャネルごとに dial（＝`net.Dial("unix", $SSH_AUTH_SOCK)`）。

回帰: `mux_test.go`（in-memory pipeListener＋net.Pipe で hermetic）。往復・並行
チャネル・client close 伝播・chunked パイプ再組立・pipe close 全撤去・
frame-too-big 防御を機械検証。

## トリガと認可（Phase 2 ✅・**wake ベース**＝既存機構の完全再利用）

⚠設計当初は command 線（`ssh-forward-start/stop` allowlist 追加）を想定したが、
実装では **wake トリガ**に変更＝attach（↗窓）と**完全に同一の機構**を再利用でき、
`state.ValidCommands`／`CommandRunner`／web を**一切改変しない**（最小差分・最小リスク）。

afSid = `afwd:<label|nonce>`（`sshfwdPrefix="afwd:"`。pane_id `w1:p2` とも `#inj`
とも exact-prefix で非衝突）。

- owner CLI `herdr-drover ssh-forward <pc> [label]`（`sshforward.go`）:
  1. `st.PutRelayGrant(afSid,"viewer",60s)` ＋ `st.Wake(pc, afSid)`（attach と同一）。
  2. `relayclient.DialViewerFrom(url, afSid, slavePC)`（spc 経由で slave source と突合）。
  3. `agentfwd.Owner(conn, $SSH_AUTH_SOCK)`。Ctrl-C/切断で撤去、backoff 再接続。
- slave（`webTerm.handleWake` が `isSSHForwardSid(sid)` で分岐 → `handleSSHForwardWake`）:
  1. `PutRelayGrant(afSid,"source",60s)` ＋ `dialSource(afSid)`（bearer 付き・bridge と同一 seam）。
  2. `SlaveSocket(~/.herdr-drover/agent-fwd/<name>.sock)`（0600・stale 除去）。
  3. `agentfwd.Slave(conn, ln)`。owner 切断/ctx 終了で戻り **socket 撤去**（＝撤去は
     owner の Ctrl-C で自動＝relay が source を畳む→Slave が return）。
  dedup（active map）は bridge 経路と共有＝多重 wake は無視。

認可の肝: **owner viewer は既存 ↗窓 と同じ `spc=slavePC` 経路**で slave source と
ペアリング（relay KeyFor が pc 名前空間キーへ）。slave source は slave token（SlaveGate）。
grant が無ければ relay が 403。master path は無改変。

env UX（エージェント対エージェント）: owner CLI が `~/.herdr-drover/agent-fwd/
<name>.sock`（`~` 相対）を表示 → owner の claude が slave の claude に
`SSH_AUTH_SOCK=<そのパス> git clone/pull …` をインライン実行させる（起動済プロセスの
env は触らない。socket 実体は slave のシェルが `~` を展開して到達）。

## env UX（Phase 2）

slave の pane は既に起動済プロセス（claude/shell）＝env を後から差し込めない。
`ssh-forward-start` の Ack が socket パスを返し、owner は slave シェルで:

```sh
export SSH_AUTH_SOCK=~/.herdr-drover/agent-fwd/<sid>.sock
git push            # 署名のたびに owner Mac で confirm
```

を実行（or `GIT_SSH_COMMAND="ssh -o IdentityAgent=<sock>"`）。socket パスは
セッション固定＝alias 化可能。将来: 新規 pane 起動時に env を仕込む糖衣。

## セキュリティ特性まとめ

| リスク | 対策 |
|---|---|
| 秘密鍵の slave 漏洩 | 鍵は Mac の agent から出ない（転送のみ） |
| 同アカウント他人の socket 掴み | `ssh-add -c` で毎署名 owner 承認 |
| 万一の鍵/署名悪用の被害範囲 | 専用 deploy key（対象リポ限定・失効可能） |
| 常設化 | owner コマンド実行中のみ・切断/stop/TTL で撤去 |
| 端末ストリーム汚染 | agent 線は別 relay セッション（afSid） |
| 未認可接続 | 既存 slavegrant/SlaveGate（master path 無改変） |

**残存リスク（honest）**: confirm を連打で許可する運用や、owner Mac 自体の
侵害には対抗できない。共用 slave 上で GitHub を owner 名義で操作する行為自体の
リスクは残る（本機構は「鍵を置かない・毎回承認・被害限定」で最小化するもの）。

## Phase 分割

- **Phase 1 ✅**: agentfwd mux コア＋hermetic テスト（本 PC 完結）。実 relay.Server
  越し（httptest・no-auth）の owner(viewer)↔slave(source) 転送 e2e も緑。
- **Phase 2 ✅（コード完成・build darwin+linux 緑・unit/統合テスト緑・回帰なし）**:
  owner CLI `ssh-forward`（`sshforward.go`）／slave の wake 分岐
  `handleSSHForwardWake`（`webterm.go`＋`sshforward.go`）／main dispatch。
  **wake ベースで state/web/CommandRunner 無改変**。helper 回帰（`sshforward_test.go`：
  afSid 判定・sock 名 sanitize・~ 表示と実体の basename 一致・104 byte 上限）。
  ⚠**未検証**: 実 Firestore wake→slave 起動→実 SSH_AUTH_SOCK での git は Phase 3。
- **Phase 3（要実機・operational）**: (a) release 発行 (b) **slave を更新**（現行
  v0.4.4 は本コードを持たない＝`herdr-drover update` 要） (c) 専用 deploy key を
  owner の agent へ `ssh-add -c` 登録 (d) 実 relay＋実 slave＋実 GitHub で
  `SSH_AUTH_SOCK=<sock> git clone/pull` が通ることを確認（deploy key・confirm）。
