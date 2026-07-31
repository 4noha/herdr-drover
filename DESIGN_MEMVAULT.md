# DESIGN: 共用 slave の AWS / GCP / GitHub 認証（memvault 連携）

共用 slave（同一 macOS アカウントを複数人が共有）で AWS / GCP / GitHub の認証を
**秘密材をディスクに置かず**行う仕組み。実体は外部プロダクト
[4noha/memvault](https://github.com/4noha/memvault) の daemon で、drover は
その **control plane を叩く thin wrapper** を提供する（`herdr-drover memvault`）。

- 実装: [`cmd/herdr-drover/memvault.go`](cmd/herdr-drover/memvault.go)（CLI）／
  [`internal/memvaultclient/client.go`](internal/memvaultclient/client.go)（HTTP-over-UNIX-socket client）
- memvault 側の正: 同 repo の `README.md`＋`docs/design/multi-owner-retention.md`
- 併存する別方式: SSH エージェント転送＝[DESIGN_SSH_FORWARD.md](DESIGN_SSH_FORWARD.md)
  （§7 で使い分けを示す）

⚠ **この文書は drover から見た連携仕様**。memvault daemon 自体の仕様変更は
memvault repo が正で、ここは追随する側。バージョン差で乖離したら memvault の
README／実装を優先すること。

## 1. 何を解決するか

共用 slave では **ファイルパーミッションで他人と隔離できない**（同一 UID）。
したがって従来型の認証ファイル配置は全て「置いた瞬間に同席者へ漏れる」:

| 従来のやり方 | 共用 slave での問題 |
|---|---|
| `~/.aws/sso/cache/*.json` に SSO トークン | 同 UID の他人が読める＝そのまま assume role できる |
| GCP SA 鍵 JSON をディスクに置く | 同上。しかも鍵は**期限が無い**＝漏れたら失効操作が必要 |
| `gh auth login` で PAT を keychain へ | keychain も同一 UID なら `security` で引ける |
| `~/.ssh/id_*` を置く | 秘密鍵そのものが漏れる（最悪） |

memvault の解は **「秘密材はメモリだけに置き、外向きに使える短命トークンしか
返さない」**。raw material（SA 鍵 JSON・SSO refresh token・PAT・App 秘密鍵）は
operator の laptop から SSH 越しに daemon の標準入力へ流し込まれ、slave の
ディスクには一度も落ちない。

## 2. トポロジ

```
[operator laptop]  raw material（SA 鍵 / SSO cache / PAT / App 秘密鍵）
     │  ssh shared-mac "memvault inject <kind> [--owner NAME]"   ← stdin 越し・ディスク非経由
     ▼
[共用 slave]  memvault daemon（1 プロセス・材料はメモリのみ・mode 0600 socket）
     │            slot map: default("") + owner ごとの slot（multi-owner）
     ├── ctrl socket  $MEMVAULT_CTRL_SOCKET   … inject / claim / release / status / job/*
     └── use socket   $MEMVAULT_USE_SOCKET    … /aws/creds /gcp/* /git/credential
                 │
    ┌────────────┼─────────────────┬────────────────────┐
    ▼            ▼                 ▼                    ▼
 AWS SDK 全部  GCP ツール全部    git / gh            静的 API キー
 credential_   GCE metadata      git-credential      認証リバプロ
 process       impersonator      helper              (--proxy-port)
 (~/.aws/config) (GCE_METADATA_HOST)                 (値は返さず外向きに付与)
                 │
[owner の drover]  herdr-drover memvault {status|whoami|claim|release|
                   issue-inherit-token|job register|job end}   ← ctrl socket のみ
```

**drover の位置づけ**: pane に紐づく operator が誰かを知っているのは herdr 側
なので、operator 切替（claim/release）と job 寿命宣言を drover 経由で行える。
逆に **inject 経路は drover に持たせない**（§6 の不変条件）。

## 3. AWS 連携

### 3.1 材料と交換の連鎖

`memvault inject aws` に渡す 1 profile 分の JSON:

```json
{"profile":"...","start_url":"https://<org>.awsapps.com/start","region":"...",
 "account_id":"...","role_name":"...","refresh_token":"...",
 "client_id":"...","client_secret":"..."}
```

`refresh_token` は laptop で `aws sso login` した結果
`~/.aws/sso/cache/<sha>.json` の `refreshToken` に落ちている値。`client_id` /
`client_secret` は隣の client registration cache から取る。この組み立ては
memvault repo の `tools/build-aws-injection.py`（Python 3 stdlib のみ）が行う:

```sh
# operator laptop 側で実行。stdout の 1 行 JSON をそのまま ssh に流す
python3 tools/build-aws-injection.py my-role \
  | ssh shared-mac "memvault inject aws --owner $(whoami) --ttl 8h"
```

daemon 内の 2 段交換（`internal/stores/aws.go`）:

1. `sso-oidc:CreateToken(grant_type=refresh_token)` → SSO access token（約 1h）
   — **応答に rotated refresh_token が入ることがあり、その場で置き換える**
2. `sso:GetRoleCredentials(sso_access_token, accountId, roleName)` → STS creds
   （`Expiration` までキャッシュ）

⚠ **OIDC refresh は profile の作業リージョンではなく sso-session の region で
行う**（memvault の `e1523d7 fix: use SSO region for AWS injection` で修正済み）。
リージョンを間違えると refresh だけが失敗する。

### 3.2 消費側（slave 上のワークロード）

`~/.aws/config` に `credential_process` を書くだけで **AWS SDK 全部**が通る:

```ini
[profile my-role]
credential_process = /usr/local/bin/memvault aws-creds my-role
region = us-east-1
```

boto3 / aws-sdk-go / aws-sdk-js / terraform aws provider / aws-cdk / aws CLI が
対象。SDK は資格情報の更新ごとに `memvault aws-creds` を起動し、その都度
SSO→STS 交換で 1h の資格情報を得る。

**返るのは STS の短命 creds だけ**で、SSO refresh token は絶対に返らない。
daemon の wipe / TTL 失効後は次回リフレッシュが失敗し、ツールは**明示的に
落ちる**（silent に古い権限で動き続けない）。

### 3.3 drover が関わる点＝job registry

SDK 側は `Expiration`（約 55 分）まで creds をキャッシュする。ところが
`inject --ttl 30m` のように **kind の TTL が SDK キャッシュより短い**と、
「daemon 上の材料は消えたが SDK は生きた creds を握っている」状態になり、
retention の判断が壊れる。memvault はこれを ledger で解く（`noteMint` が
`Expiration` を記録して slot を延命）。

さらに **docker build のような長時間 job** は 1 回の creds では終わらない。
そこで drover が pane 単位で job 寿命を宣言する:

```sh
herdr-drover memvault job register --ttl 4h     # job-id 省略時 $HERDR_PANE_ID
... 長い処理 ...
herdr-drover memvault job end
```

`--job-id` 省略時は **`$HERDR_PANE_ID`** を使う（drover が既に持つ pane 単位の
自然な識別子＝新しい ID 体系を足さない）。`job end` は冪等＝未知の job-id でも
安全に呼べる。

## 4. GCP 連携

### 4.1 材料

```sh
# SA 鍵 JSON をディスク非経由で流す（laptop 側にも slave 側にも落ちない）
ssh shared-mac "memvault inject gcp --owner $(whoami)" < sa.json
```

daemon は RS256 署名 JWT → `oauth2.googleapis.com/token` の交換を行い、
access token（scope ごと）と ID token（audience ごと）を約 50 分キャッシュする。

### 4.2 消費側 — 2 経路

**(a) GCE metadata server impersonator（推奨・ツール無改造）**

```sh
memvault serve --metadata-port 9020 ...
export GCE_METADATA_HOST=127.0.0.1:9020
```

Google の認証ライブラリは全て `GCE_METADATA_HOST` を見る（未設定なら
`metadata.google.internal`）。memvault が互換エンドポイントを 127.0.0.1 に立てる
ことで、**ADC の標準ディスカバリに乗る全ツール**がそのまま vault からトークンを
取る: `gcloud` / `gsutil` / `bq` / terraform google provider / cdk /
`python google-cloud-*` / 各言語の `google-auth-*`。

実装する endpoint（`internal/daemon/metadata.go`）:

```
GET /                                     （probe）
GET /computeMetadata/v1/                  （probe: SDK version discovery）
GET /computeMetadata/v1/project/project-id
GET /computeMetadata/v1/instance/service-accounts/default/token
GET /computeMetadata/v1/instance/service-accounts/default/email
GET /computeMetadata/v1/instance/service-accounts/default/scopes
GET /computeMetadata/v1/instance/service-accounts/default/identity?audience=...
```

契約: **リクエストにもレスポンスにも `Metadata-Flavor: Google` が必須**（実 GCE が
そうなっており、認証ライブラリはこれを見てペイロードを信頼する。ブラウザからの
素アクセスも弾ける）。GCP kind が wipe / 失効すると `/token` は **503** を返し、
ツールは "could not refresh token" として明示的に失敗する。

**(b) 直接 endpoint（自作クライアント向け）**

```sh
TOKEN=$(memvault gcp-id-token --audience 'https://my-cloud-run-xyz.a.run.app')
curl -H "Authorization: Bearer $TOKEN" https://my-cloud-run-xyz.a.run.app/...
```

use socket の `/gcp/id-token?audience=...` / `/gcp/access-token?scope=...` を
直接叩く形。drover-cloud の Cloud Run relay を認証付きで呼ぶ用途に合う。

## 5. GitHub 連携

GitHub は **git が in-band にトークンを要求する**ため、AWS/GCP と違って
「トークンを呼び出し元に返す」必要がある。memvault はそのぶん経路を意図的に
狭めている（`internal/stores/git.go`: 「host は注入済みマップと exact-match で
検証し、git-credential-helper プロトコル 1 個だけが答える。他の用途で取り出す
手段は無い」）。

### 5.1 kind `git` — PAT / installation token を host ごとに持つ

```sh
echo '{"github.com":"github_pat_...","gitlab.com":"glpat-..."}' \
  | ssh shared-mac "memvault inject git --owner $(whoami) --ttl 8h"
```

- host は **小文字化した完全一致**（wildcard / サブドメイン探索なし）。未知 host は
  **404**＝typo が silent に通らない。
- 消費は git-credential helper として:

  ```sh
  git config --global credential.helper '!memvault git-credential'
  ```

  `/git/credential?host=<host>` は `{"username":"x-access-token","password":<tok>}`
  を返す。`get` 以外の op（`store` / `erase`）は **no-op**＝git に vault の状態を
  変更させない（トークン寿命は vault が単独で所有する）。404 時は stderr に
  「inject git kind first」を出し、git は次の helper／プロンプトへ落ちる。

### 5.2 kind `github_app` — ⚠ **inject までしか繋がっていない（未完）**

```json
{"app_id":"123456","installation_id":"78901234",
 "private_key":"-----BEGIN RSA PRIVATE KEY-----\n..."}
```

RSA 秘密鍵は inject 時に 1 度だけパースされ、**vault メモリから出ない**。
`GitHubAppStore` は installation access token（`ghs_*`）を発行してキャッシュする
機能を持つ。

⚠ **だが 2026-07-31 時点でその発行経路はどの endpoint からも呼ばれていない**。
`/git/credential` は `slot.git`（kind `git`）だけを見るので、**`github_app` を
inject しても git 認証は通らない**。実測:

```
memvault inject github_app          # → {"ok":true,"kind":"github_app"}
curl '/git/credential?host=github.com'
                                    # → HTTP 404 no git credential held
memvault status                     # → github_app_loaded=true, git_loaded=false
```

`daemon.go` 内で `sl.ghapp` を触るのは inject / wipe / `Loaded()`（status 用）
だけ＝**token を作る側の呼び出しが無い**。したがって現状 GitHub 認証は
**kind `git`（§5.1）だけが実働**で、`github_app` は「材料は保持できるが使えない」
状態。`status` に `github_app_loaded: true` が出ても**認証が通る保証にならない**
（これが罠。`git_loaded` の方を見る）。

将来これを繋げば PAT より **App の方が望ましい**: 権限が repo 単位で絞れ、token
自体が短命で、発行元の鍵を失効させれば全 token が死ぬ。

### 5.3 `gh` CLI

`gh` は git-credential helper を見ないので wrapper を使う（memvault repo の
`tools/gh-mv`）。

⚠ **`tools/gh-mv` は別ブランチ `origin/feat/gh-mv-tool`（commit `5890285`）に
しか無い**。`origin/main` にも、drover が相手にしている
`feat/multi-owner-retention` にも**入っていない**（実測）。使うにはその
ブランチから持ってくるか、cherry-pick が必要。

要点:

- `git-credential get` で毎回トークンを取り、**`GH_TOKEN` env として gh の子
  プロセスにだけ渡す**。**OS keychain には保存しない**（vault の TTL モデルを保つ）。
- shell function ではなく**独立スクリプト**＝非対話 shell（multiplexer 駆動の
  エージェント session、`ssh host 'gh ...'` の one-shot）でも `~/.zshrc` の
  source なしに動く。これは **drover の pane から使う上で本質的**。
- 各人が自分の socket を持つので `~/.zshenv` に
  `export MEMVAULT_SOCKET="$HOME/.memvault-<your-name>.sock"` を置く運用
  （`.zshenv` は非対話 shell も読む）。

⚠ **`git_loaded` / `git_hosts` / `github_app_loaded` は `/status` の top-level に
出るようになったばかり**（memvault 側 `feat/multi-owner-retention`）。それ以前の
daemon ではこの 3 つが top-level に無い（`slots[""]` 側には出る）。

## 5.4 ⚠ 実測 trap（2026-07-31・隔離した実 daemon で確認）

資料化に際して実 memvault daemon（`--socket`/`--ctrl-socket`/`--use-socket` を
`/tmp` に隔離）で全経路を通したときに踏んだもの。**いずれも silent に間違った
結果になる**ので運用前に知っておく必要がある。

### (a) `active_operator` が居ると inject 先と参照先の slot がズレる

最も刺さる。**`inject` は `--owner` 省略で default slot（`""`）に入るが、
参照側（`/git/credential` 等）は `--owner` 省略時に active operator の slot を
見る**。実測:

```
memvault inject git                       # → default slot に入る
herdr-drover memvault claim               # → active_operator=alice
curl '/git/credential?host=github.com'    # → HTTP 404（alice の slot は空）
curl '/git/credential?host=github.com&owner='   # → HTTP 200（default slot）
herdr-drover memvault release             # → active が空に
curl '/git/credential?host=github.com'    # → HTTP 200
```

404 の本文は `no git credential held for host "github.com"` で、**「host が違う」
ようにしか読めない**（実際は slot 違い）。multi-owner を使うなら
**inject 側も `--owner` を必ず明示する**のが正しい運用。

**2026-07-31 対処済**: `herdr-drover memvault status` / `whoami` がこの状態を
検出して stderr に警告を出すようにした（stdout の JSON は素のまま）。

```
⚠ slot ズレ: active_slot="alice" は材料が空だが default slot に git がある。
  参照側 (/aws/creds /gcp/* /git/credential) は --owner 省略時に active slot を見るので、
  この状態では材料があるのに 404 / 503 になる（404 本文は host 違いに見える）。
  対処: inject を --owner alice でやり直す、または `herdr-drover memvault release` で default slot に戻す。
```

判定を書くときに踏んだ**二次の trap**: `claim` は slot を materialize しない。
memvault の slot は `slotForOwner` の lazy 生成で、`inject` か参照が来た時点で
初めて生える。よって **claim 直後の `/status` は `slots={"":{…}}` のままで
`alice` のキーが無い**（実測）。この不在を「判定不能」として黙る実装にすると、
**最も普通の踏み方でちょうど警告が出ない**。生成直後の slot は必ず空なので、
`slots` オブジェクトがある応答での**エントリ不在は「空」で確定**とする。
「本当に判定不能」なのは `slots` 自体が無い応答（multi-owner 前の daemon）だけ。

### (b) split-socket でも legacy socket が `$HOME` に作られる

`--ctrl-socket` / `--use-socket` を指定しても、**`--socket` を省略すると
`$MEMVAULT_SOCKET`（既定 `$HOME/.memvault.sock`）が「後方互換の catch-all」として
併せて listen される**（deprecation ログは出る）。実測ログ:

```
split-socket mode: MEMVAULT_SOCKET=/Users/<me>/.memvault.sock still accepts all
endpoints for back-compat but is deprecated; use MEMVAULT_CTRL_SOCKET / MEMVAULT_USE_SOCKET
```

つまり **split-socket にしただけでは全 endpoint を受ける socket が残る**＝
ctrl/use 分離の意図が達成されない。分離したいなら `--socket` も別パスへ明示する。
検証目的で daemon を立てるときは **`--socket` を省略すると本番運用中の
`$HOME/.memvault.sock` を奪う**ので必ず明示すること。

**2026-07-31 対処**: 挙動そのものは single-socket 互換のため据え置き（変えると
既存 slave の `.zshenv` 運用が全部壊れる）。代わりに memvault README に
「#### Split-socket mode」節を新設し、この catch-all 挙動と「本番 socket を
奪い得る」ことを明記した（commit `8dec677`）。

### (c) `herdr-drover memvault status` が daemon の top-level 5 キーを捨てていた

**2026-07-31 修正済**。`memvaultclient.Status` の struct にフィールドが無いキーが
JSON デコードで落ちていた。実測の差分（daemon が返すが drover が出さなかった）:

```
git_hosts / git_loaded / github_app_loaded / kind_ttl_remain_sec / routes
```

つまり **GitHub 連携の状態（`git_loaded` / `github_app_loaded`）と kind ごとの
残 TTL が top-level に出ない**＝`herdr-drover memvault status` だけでは「GitHub
材料が入っているか」「あと何分で切れるか」が分からなかった。**鉄則⑤（silent な
skip 禁止）違反**。

直し方は 2 案あった:

| 案 | 評価 |
|---|---|
| `Status` struct に 5 フィールドを足す | ❌ 同じ穴が次の拡張で再発する（この 5 キーもそうやって漏れた） |
| 表示を生の `map[string]any` にする | ✅ daemon が今後足すキーに自動追随 |

採った形（構造的に再発しない側）:

- `Status.Raw map[string]any` を追加し、`Status()` で **同じ body を 2 回
  decode**（typed＋map）。1 往復のまま生の応答を失わない。
- **表示は `Raw`／分岐は typed field** という規約を struct の doc comment に
  明記（`internal/memvaultclient/client.go`）。
- 回帰テスト `TestMemvaultStatusShowsEveryDaemonField` が実 daemon を起動し
  「daemon が返したキーが 1 つでも出力に無ければ FAIL」を固定。旧コード
  （typed struct 表示）では上記 5 キーで実際に FAIL することを確認済。

### (d) 同梱 CLI が daemon 自身の split-socket env を読んでいなかった

**2026-07-31 memvault 側で修正**（commit `8dec677`）。(b) の続きで見つけたもの。
daemon は split-socket 起動時に `$MEMVAULT_CTRL_SOCKET` / `$MEMVAULT_USE_SOCKET`
を announce するのに、**同梱 `memvault` CLI 側は `$MEMVAULT_SOCKET` 一本しか
見ていなかった**＝split-socket で起動した daemon に対して**全 11 サブコマンドが
legacy path を叩き続ける**。legacy socket が無い構成では即死、ある構成では
catch-all に落ちて (b) の分離が完全に無意味化する。

`platform.CtrlSocketPath()` / `UseSocketPath()` を追加し、plane ごとに
再ルーティング（use-plane = `gcp-id-token` / `git-credential` / `aws-creds`、
残り 8 つが control-plane）。**plane 間の相互 fallback はしない**——誤った plane
へ落ちると (e) の 404 になるだけで、silent に間違った結果になる。

drover 側は最初から `internal/memvaultclient` が CTRL を優先していたので影響なし
（§5.4(g) の実測表で確認済）。

### (e) 誤った plane を叩いた 404 が「材料が無い」と誤診断される

**2026-07-31 memvault 側で修正**（commit `8dec677`）。404 には 2 種類ある:

| 出どころ | 本文 | 意味 |
|---|---|---|
| handler の `writeErr` | JSON（`{"error":…}`） | endpoint はある。「その host の材料を持っていない」 |
| net/http の mux | `404 page not found`（text/plain） | **この socket にその endpoint が無い** |

これを区別せず、後者にも「inject git kind first」と出していた＝**既に入っている
材料を再 inject しに行かされる**。use-plane の呼び出しが ctrl socket に着地する
のは split-socket 移行中に普通に起きる。`isUnknownEndpoint()`（404 かつ本文が
`{` で始まらない）で判別し、wrong-plane であることと正しい env 名を告げるように
した。

### (f) inherit token が一意でなかった（既存 flaky test の真因）

**2026-07-31 memvault 側で修正**（commit `8dec677`）。`./...` で
`TestInheritTokenTampered` が落ちたので**自分の変更のせいだと決めつけず**
bisect した結果、**HEAD で 30 回中 3 回 FAIL** する既存 flaky だった。真因は
テストではなく本体:

`base64.RawURLEncoding` は**末尾の未使用ビットを無視して decode する**。payload の
長さが 3 mod 4 だと最終文字は 2 bit しか意味を持たないので、**4 通りのうち 3 通りが
同一に decode する**。実測（200k 回 brute-force で捕獲）:

```
...MTU  → alice|bob|1785482415
...MTX  → alice|bob|1785482415   ← 別の token 文字列が同じ grant として検証を通る
```

署名検証は通るので**認可としては壊れていない**が、**token 文字列が 1 つの grant の
一意な名前でなくなる**＝revocation list / replay cache / audit の dedup が
「同じ grant の別表記」を取りこぼす。`base64.RawURLEncoding.Strict()` へ変更
（非正規パディングを拒否）。テスト側も「最終文字だけ叩く」確率的な形をやめ、
**両 segment の全 index を決定的に sweep** する形に書き換え。修正後 flaky 0/30。

### (g) 検証で確認できた「資料どおり」の項目

| 主張 | 実測 |
|---|---|
| claim conflict = exit 3 | ✅（`{"active":"alice","advise":"run again with force=1…"}` を stderr に出して 3） |
| release conflict = exit 3 | ✅ |
| usage error = exit 2 | ✅（未知サブコマンド・引数なし） |
| `$MEMVAULT_CTRL_SOCKET` が `$MEMVAULT_SOCKET` に勝つ | ✅（legacy を無効パスにしても成功） |
| legacy `$MEMVAULT_SOCKET` へフォールバック | ✅（CTRL 未設定で成功） |
| socket 到達不能は loud に失敗 | ✅ exit 1＋`dial unix …: no such file or directory` |
| `--job-id` 省略で `$HERDR_PANE_ID` | ✅（`{"job_id":"p42","owner":"alice"}`） |
| `job end` は冪等 | ✅（未知 id で `{"existed":false,"ok":true}` exit 0） |
| job-id も PANE_ID も無い＝エラー | ✅ exit 1（推測しない） |
| git host は両方向で小文字化・exact-match | ✅（`GitLab.com` で inject → `gitlab.com`/`GitLab.com` 双方 200） |
| 未知 host は 404 | ✅（`bitbucket.org`） |
| git credential は `x-access-token` を返す | ✅ |
| metadata: `Metadata-Flavor` 無しは拒否 | ✅ **403** |
| metadata: GCP 未 inject の `/token` | ✅ **503** `failed to mint access token` |
| metadata: 応答に `Metadata-Flavor: Google` | ✅ |

## 6. drover 側の設計判断（なぜこの形か）

### 6.1 inject 経路を drover に持たせない（意図的な非対応）

raw material（SA 鍵・SSO refresh token・PAT・App 秘密鍵）を送る経路は
**各 operator の laptop からの SSH tunnel が担当**する。drover は inject の
入口を持たない。理由:

- drover は slave 上で常駐する daemon＝そこに inject 経路を作ると「共用機の
  常駐プロセスが raw material を扱う」ことになり、memvault の設計契約
  （材料は laptop から出さない・共用機には短命トークンだけ）を drover が破る。
- drover 経由にしても laptop 側の材料読み出しは laptop でしか出来ない＝
  中継が 1 段増えるだけで、得られる安全性はゼロ。

同様に **proxy / metadata / credential_process の use-plane も drover は通さない**。
これらは pane の env（`$MEMVAULT_SOCKET` / `$GCE_METADATA_HOST` /
`credential_process`）で解決する memvault 本体の責務。

### 6.2 drover が持つのは control plane だけ

| drover サブコマンド | memvault endpoint | 用途 |
|---|---|---|
| `memvault status` | `GET /status` | daemon 生存・kind ごとの残 TTL・slot 一覧 |
| `memvault whoami` | `GET /whoami` | active operator と実効 slot（inherit 込み） |
| `memvault claim` | `POST /claim` | 自分を active に（`--force` / `--inherit --token`） |
| `memvault release` | `POST /release` | active を降りる |
| `memvault issue-inherit-token` | `POST /issue-inherit-token` | 自分の slot を他人に貸す consent 発行 |
| `memvault job register` / `job end` | `POST /job/register` / `/job/end` | 長時間 job の寿命宣言（§3.3） |

### 6.3 operator 名の決定順序

`--operator` → `$MEMVAULT_OPERATOR` → `$USER`。pane env に運用者名を宣言する
余地を残しつつ、最後は必ず `$USER` に落ちる。3 つ全部が空なら**推測せずエラー**
（鉄則③＝ヒューリスティック分類をしない）。

`job register` / `job end` の `--owner` は省略可で、空＝default slot（従来の
1-tenant ケース）。ただし明示されなければ claim/release と同じ順序で
`$MEMVAULT_OPERATOR` / `$USER` を優先する（**同じコマンド列で操作対象 slot が
ブレないようにする**ため）。

### 6.4 socket の 2 面性（split-socket モード）

memvault は phase 1d で ctrl / use を別 socket に分離した。drover の client は
用途ごとに正しい socket を選ぶ:

| 関数 | env の優先順序 | 使う endpoint |
|---|---|---|
| `CtrlSocketPath()` | `$MEMVAULT_CTRL_SOCKET` → `$MEMVAULT_SOCKET` → `$HOME/.memvault.sock` | claim / release / whoami / status / job/* / issue-inherit-token |
| `UseSocketPath()` | `$MEMVAULT_USE_SOCKET` → `$MEMVAULT_SOCKET` → `$HOME/.memvault.sock` | `/gcp/id-token` / `/gcp/access-token` / `/aws/creds` |

いずれも legacy `$MEMVAULT_SOCKET` にフォールバックする＝**単一 socket 運用の
daemon がそのまま動く**（後方互換）。socket が決定できない場合は「memvault は
無い」として素直に失敗する（`HOME` すら無い環境で空文字を返す）。

### 6.5 終了コードの割り当て

| code | 意味 |
|---|---|
| 0 | 成功 |
| 1 | generic runtime error |
| 2 | usage error（未知サブコマンド・引数不足） |
| **3** | **claim / release の conflict**（他 operator が active／自分が active でない） |

conflict を 3 で分けるのは、**自動化から「奪ってよいか」を判断させるため**。
1 と混ぜると「daemon が落ちている」と「他人が使っている」が区別できない。
conflict 時は daemon が返した JSON を stderr にそのまま出す（誰が active かが
分かる）。

## 7. SSH エージェント転送との使い分け

同じ「共用 slave の GitHub 認証」を [DESIGN_SSH_FORWARD.md](DESIGN_SSH_FORWARD.md)
も解く。両者は排他ではなく守備範囲が違う:

| | memvault（本文書） | SSH agent forwarding |
|---|---|---|
| 対象 | AWS / GCP / GitHub / 任意の静的 API キー | GitHub（SSH remote）のみ |
| slave 上の秘密 | 短命トークン（メモリのみ） | 無し（署名は owner Mac で実行） |
| owner の常時接続 | **不要**（TTL の間は自律動作） | **必要**（切断で即失効） |
| 1 署名ごとの承認 | 無し（TTL 内は自由に使える） | **あり**（`ssh-add -c`） |
| 同 UID の他人 | socket を掴めば TTL 内は使える | socket を掴んでも confirm で止まる |
| 非対話・長時間 job | 得意（`job register` で延命） | 苦手（承認ダイアログが人を要求） |

**選び方**: エージェントに長時間まわす作業（terraform apply・docker build・
CI 相当）は memvault。人が見ている前での一時的な push は SSH forwarding
（1 操作ごとの承認が付く方が強い）。

## 8. 脅威モデル（memvault から継承・drover 側の含意）

**防げるもの**: ディスク上の材料漏洩（そもそも置かない）／材料の長期残存
（TTL・hard cap）／ログ漏洩（**イベントだけ記録し秘密は一切ログに出さない**）／
core dump 経由の漏洩（`RLIMIT_CORE=0`）。

**防げないもの（明記された非目標）**:

- **同一 UID の敵対的な同席者**。socket は mode 0600＝これが唯一の認可境界で、
  同 UID の任意プロセスは全 endpoint を呼べる。**inject は無条件上書き**で、
  nonce も window state も無い（「同 UID で到達可能な場所に置ける認証情報は
  同 UID から読めるので、境界を増やさず攻撃面だけ増える」という判断）。
- バイナリ自体の改竄（user-writable path に居る）。
- 機械をまたぐ隔離。**1 台 1 daemon**が前提で、slot モデルは「共用機の中で
  協力的な少人数を分ける」ためのもの。

⚠ したがって **memvault は「協力的なチームの事故を防ぐ」道具**であり、
「敵対的な同席者から守る」道具ではない。

### 8.1 per-UID 隔離の実効範囲（2026-08-01 実測）

daemon を専用アカウント（`_shared_noaki` UID/GID 401・home `/Users/_shared_noaki`
mode 700・shell `/usr/bin/false`・`AuthenticationAuthority` 無し＝ログイン不可）で
動かす形を slave-1 / slave-2 の 2 台で運用中。**効く範囲と効かない範囲を実測で
確定させたので、「per-UID にしたから敵対的同席者にも耐える」とは書かないこと**。

**効く（kernel authz）**: UNIX socket は mode 0600＋親ディレクトリ 700 で、
共用ログインからの `connect(2)` は EACCES（errno 13）。実測:

```
$ ls -la /Users/_shared_noaki/.memvault.sock
ls: /Users/_shared_noaki/.memvault.sock: Permission denied
```

**効かない (1) — TCP 面に UID 認可が無い**: proxy（9010）と metadata（9011）は
`127.0.0.1` に bind するだけで、**呼び出し元 UID を一切見ない**。実測で
`nobody` からでも応答する。materials が実際に出ていくのはこの経路なので、
per-UID 化は**漏洩経路そのものには触れていない**。metadata 側の唯一の門は
`Metadata-Flavor: Google` ヘッダで、memvault 自身のコメントが
"a weak defence" と書いている（ヘッダ無し=403・未 inject=503 は実測どおり）。

**効かない (2) — sudo で迂回できる**: 共用ログインは 2 台とも
`admin` グループに属する。slave-1 はさらに `/etc/sudoers.d/memvault-runner` が
`NOPASSWD: ALL` なので、**このログインを使える人は誰でも**
`sudo -n -u _shared_noaki memvault status` で 0600 境界を越えられる。slave-2 は
NOPASSWD を作らなかったが、共用パスワードを知っていれば同じことができる。
＝**共用ログインの上では per-UID 隔離は構造的に有効でない**。

したがって per-UID で実際に得られているのは「敵対的同席者への耐性」ではなく
**事故の抑止**（他人の socket を素で掴めない・`ls` で他人の材料の存在が見えない・
プロセス所有者で監査できる）である。敵対的同席者が要件なら**ログイン自体を
分ける**（per-UID の daemon ではなく per-UID の**ログイン**）かハードウェア鍵。

なお per-UID 化は consumer を全部連れて行かないと**認証が全断する**
（socket が 0700 home に移るため）。その修復＝memvault
`tools/memvault-git-credential`（TODO.md §A0 の 2026-08-01 節）。

## 9. multi-owner モデルの要点（drover が叩く相手の状態機械）

- **slot map** = default slot（owner `""`）+ owner ごとの slot。`--owner` 無しの
  呼び出しは従来どおり default slot＝**既存スクリプトは無改造で動く**。
- **`--owner alice` への inject は誰も上書きしない**。材料が消えるのは
  **release / claim --force / TTL** の 3 つだけ。
- **inherit はコピーしない**。operator Y が owner X の slot を使うとき、読むのは
  X の slot 実体。**X が wipe すれば Y は即座に失う**。
- **inherit token は daemon 起動ごとに再生成される HMAC 鍵で署名**＝daemon を
  再起動すれば発行済み token は全部失効する（緊急時の一括失効手段になる）。
- `claim --force` は前の operator の slot を **wipe して**奪う。drover から
  `--force` を叩くのは、相手が離席していると分かっている時だけにする。

## 10. 現状と未了

- ✅ drover 側 CLI・client 実装（`memvault.go` 316 行 / `memvaultclient/client.go` 331 行）
  ＝ブランチ `feat/memvault-integration`（commit `0b264bc` / `78492c9` / `c680504`）。
- ⏳ **未 merge**（`main` は `v0.5.34` = `da25a20`。feat は main から 3 commit 先）。
- ⏳ **pane env への `MEMVAULT_*` 自動注入は未実装**。現状は各人が**自分の
  セッションで** `export MEMVAULT_SOCKET=...` する運用。⚠ **`~/.zshenv` には
  書かない**＝共用ログインでは全員のシェルに読まれるので、他人のシェルに自分の
  vault を押し込む（自分の名前でコミットしつつ他人のトークンで push する状態）。
  `.zshenv` に置くのは PATH だけ。drover が pane 生成時に注入する余地はあるが、
  **socket 名 = 個人名なので「この pane の operator は誰か」を drover が決める
  必要があり、設計判断が残っている**。
- ⏳ 実機 e2e（AWS / GCP / GitHub の 3 系統を共用 slave 上で通す）は未記録
  （材料の実 inject はユーザー判断で今回スコープ外＝両 slave の daemon は
  全 kind `*_loaded: false`）。
- ⚠ **slave-1 の `/etc/sudoers.d/memvault-runner` が `NOPASSWD: ALL` のまま**
  ＝要件に絞った whitelist へ戻す作業が残っている（TODO.md 側で追跡）。
  **slave-2 には作っていない**（対話 sudo のみ）。
- ⚠ **`~/bin/ai-agent` は per-UID を知らない**。`ai-agent status` は socket と
  port を共用 UID から stat/lsof して判定するので、per-UID daemon を
  **`down` と誤報する**（slave-1 実測: noaki が `MEMVAULT down / PROXY down`
  なのに daemon は `state = running` / pid 24081）。`ai-agent` は slave 上に
  しか無く（drover も memvault も管理していない）＝どこで版管理するかの
  判断ごと未了。

## 参照

- memvault: <https://github.com/4noha/memvault>（README＝運用の正・
  `docs/design/multi-owner-retention.md`＝slot / retention の設計意図）
- 本 repo: [DESIGN_SLAVE.md](DESIGN_SLAVE.md)＋[DESIGN_SLAVE_SPEC.md](DESIGN_SLAVE_SPEC.md)
  （共用 PC の信頼境界）／[DESIGN_SSH_FORWARD.md](DESIGN_SSH_FORWARD.md)（§7）
- CLI 仕様（引数・exit code・socket/operator 解決順）の正: [SPEC.md](SPEC.md) §2.3b
- 進行中作業・未了項目: [TODO.md](TODO.md) §A0
