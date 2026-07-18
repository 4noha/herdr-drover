# herdr-drover: slave（共用 PC）設計 — 私物セッション漏れ防止

**ステータス: 設計（未実装）。ユーザー承認済みの方向 = relay 仲介型。**
本書は「1 アカウントを複数人で使う共用 PC」に、オーナーの私物セッションを
**構造的に見せない**ための credential/認可設計。実装は合成で緑にせず、
各フェーズを実 GCP/実 relay の**敵対的 e2e ゲート**で通す（鉄則は DESIGN.md 末尾）。

## 1. 背景・目的

- 実機 `n9htqcr6g0-herdr` は **1 Google アカウントを複数人で共用する PC**。
- 現状はオーナー（`mac-studio-herdr`）のセッションが**この共用 PC に注入され、
  Web/ローカルで閲覧・操作できてしまう**（＝私物の漏れ）。
- 望む形は **一方向**:
  - **オーナー → 共用 PC**: オーナーは共用 PC のセッションを注入・操作できる（監督用途）。
  - **共用 PC → オーナー**: 共用 PC はオーナーのセッションを**一切見られない**。
- この「見られない」を、共用 PC を使う人が**信用できない**前提で（手動 `attach`・
  設定改竄・grant 自作でも破れない）**構造的に**担保する。

## 2. 脅威モデル

- **攻撃者**: 共用 PC にシェルアクセスを持つ第三者（同僚とは限らない）。
- **守る資産**: オーナーのセッション内容（claude 会話）と、その存在（sid 一覧）。
- **前提**: 攻撃者は共用 PC 上の**あらゆる資格情報・設定・バイナリを読め、任意コマンドを
  実行できる**。したがって「共用 PC 上のソフト側フラグ」は防御にならない。
- **非目標**: オーナー PC の侵害・GCP/relay 自体の侵害・タイミング/サイド
  チャネル・共用 PC 上の**自分の**セッションの相互秘匿（共用 PC 内は一蓮托生）。

## 3. 現行モデルの制約（なぜ難しいか＝実コード根拠）

| 事実 | 出典 | 帰結 |
|---|---|---|
| 全 PC が単一 SA/`uid=cm-owner` で認証・Firestore を全読み | `firestore.rules`・`fbtoken.go:31` | 共用 PC が SA を持つ＝全権 |
| relay 認可は `CheckRelayGrant(sid, role)` ＝**identity 盲目** | `relay.go:56`, `state.go:448` | grant が立てば誰でも viewer 接続可 |
| grant は sid 単位（`relaygrants/{sid+role}`）で pc 非紐付け | `state.go:388-398` | 「この sid は誰のか」を relay が知らない |

→ **根治には relay を identity 対応にするのが必須**。relay は既に SA を持つ唯一の
共通信頼境界。ゆえに**共用 PC の Firestore/relay アクセスを全て relay 経由にして、
relay が SA でスコープを強制**する（relay 仲介型）。

## 4. 決定事項

| 決定 | 内容 | 根拠 |
|---|---|---|
| credential | 共用 PC は**フル SA 鍵を持たない**。持つのは **slave ベアラートークン（uid=`slave:<pc>`）**のみ | SA=全権。渡さないのが唯一の構造的遮断 |
| token 発行 | `fbtoken.go` の `firebaseCustomToken(saJSON, uid, now)` は uid 可変＝**`slave:<pc>` で署名**する路を追加 | 既存署名基盤の再利用（RS256・1h・自動更新） |
| 認可の中心 | **relay** が毎リクエストで token の pc を検証しスコープ強制 | relay=SA 保持の信頼境界・唯一の共通経路 |
| データ面 | 共用 PC は Firestore を直叩きせず**全て relay エンドポイント経由** | slave 側に SA/直アクセスを一切置かない |
| /session | slave トークンは **role=source かつ sid∈自 pc のみ**。**viewer・他 pc は 403** | 覗き見の遮断点。ここが主防御 |
| reconcile | slave は `runRemoteInject` を回さない | 他人を注入しない（漏れの直接原因を除去） |
| role フラグ | 既存 `primary` の隣に `role ∈ {master, slave}` を追加。master 経路は**バイト不変** | 後方互換（単一/既存クラウドは完全不変） |
| sid→pc | relay が `push` 時に sid→pc を学習（TTL）。source-grant/session の検証に使う | grant が pc 非紐付けな穴を relay 側で塞ぐ |
| rules | slave uid を `pcs/{own}` write・他 deny に（**多層防御**・主防御は relay） | サーバ SDK は rules 対象外だが REST/client 経路の保険 |
| 鉄則 | 敵対 e2e で実証・合成で緑にしない・master 経路非回帰・AGPL 衛生・near-$0 維持 | DESIGN.md 継承 |

## 5. アーキテクチャ / データフロー

### enroll（slave 登録）
```
Web owner「＋端末を追加（共用/slave）」→ /api/enroll?role=slave
 → relay が一回限りコード発行（既存 pairing プリミティブ・scope="enroll-slave"）
新 PC: herdr-drover enroll <code> --relay wss://… --slave
 → /enroll が消費 → **SA JSON は返さず** slave トークン発行素材（pc 名＋
   relay が署名する短期トークンの取得口）を渡す
 → ~/.herdr-drover/config.json に role=slave＋relay＋pc を配置（sa.json は置かない）
```
既存 master enroll（`enroll.go`）は**不変**＝sa.json+config を置く従来動作。

### slave agent（`role=slave`）
```
runOneCloud(role=slave):
 - state は **SA client を生成しない**。relay-mediated client を使う
 - producer:   自セッションを **POST /slave/push**（content_hash ゲートは slave 側で継続＝near-$0）
 - webterm:    **GET /slave/wake**（own pc のみ）→ 起きたら POST /slave/grant(source)→
               WSS /session?sid&role=source で自 pane を observe/control 配信
 - reconcile:  **起動しない**（runRemoteInject をスキップ）
 - command:    受信は任意（P4 判断・restart-agent 程度に限定）
```

### relay 新エンドポイント（認証: slave bearer token）
| エンドポイント | 認可 | 動作（relay が SA で代行） |
|---|---|---|
| `POST /slave/push` | pc==token.pc | `PushStatus(pcs/{token.pc}/sessions)`。sid→pc を記録 |
| `GET /slave/wake` | own pc | `wake/{token.pc}` を watch し stream（SSE/long-poll） |
| `POST /slave/grant` | sid∈token.pc | `PutRelayGrant(sid, "source", ttl)` |
| WSS `/session` | **role=source かつ sid∈token.pc** | 既存 serve。**viewer/他 pc は 403** |

### owner → slave 操作（pane 機能・ご心配2の担保）
```
owner（Web cookie=cm-owner or master PC=SA）が slave の session を **viewer** 接続:
 wake/{slavePC} 書込（owner 権限）→ slave の /slave/wake が受信 →
 slave が source 配信 → relay が (sid, source=slave / viewer=owner) をペアリング →
 owner が閲覧・打鍵 → slave の pane へ pane.send_text
```
slave が許されるのは **source 配信だけ**。viewer 化・他 pc 読取は relay が拒否。
→ **owner からの操作性は完全に保たれ、slave からの覗き見だけが構造的に不能**。

## 6. owner 側の変更（最小・後方互換）
- Web「＋端末を追加」に **master / 共用(slave)** の選択を追加（既定 master）。
- master enroll・既存クラウド・単一クラウドは**完全に不変**（slave は新経路のみ）。

## 7. 実装フェーズ（各ゲート実 GCP/実 relay・合成不可）

1. **P1 relay slave 認可基盤**: `slave:<pc>` token 発行（fbtoken 拡張）＋検証
   ミドルウェア＋`/session` の **identity×sid-ownership** 判定（viewer/他 pc 403）。
   **ゲート**: 実 relay で ①slave token×owner sid×viewer=**403** ②slave token×
   own sid×source=**101/200** ③無資格=403 を機械確認。
2. **P2 relay slave データ面**: `push`/`wake`/`grant` エンドポイント（SA 代行・pc
   スコープ・content_hash ゲート保持）。**ゲート**: 実 Firestore で slave push が
   `pcs/{own}` のみ書け、他 pc read/write が**不能**を機械確認。
3. **P3 slave agent モード**: SA 不使用・relay 経由 state・reconcile off・
   `enroll --slave`。**ゲート（決定的敵対 e2e）**: 実 2PC 相当で
   ①owner→共用 PC の pane 操作=**成功** ②共用 PC に owner の pane が**出ない**
   ③slave 資格で owner セッション viewer=**必ず失敗** を実 relay/実 herdr で実証。
4. **P4 多層防御・UI・配布**: Firestore rules で slave uid 絞り込み・Web slave
   enroll ボタン・SETUP/README・監査。

**全フェーズ跨ぎの決定的テスト**: *slave 資格で owner セッション閲覧＝必ず 403*、
かつ *owner→slave 操作＝成功*。この 2 つが同時に緑でなければ採用しない。

## 8. リスク / 未決（実装時に確定）
- **sid→pc マップの永続性**: relay 再起動で揮発。Firestore（`sidowners/{sid}` TTL）
  へ逃がすか、push を毎回真実源にするか（P1 で決定）。
- **near-$0**: relay 仲介 push でも content_hash ゲートを**slave 側**で維持
  （変化時のみ /slave/push）。relay は受けた分だけ Firestore 書込。
- **command 受信**: slave に restart-agent 等を許すか（P4・最小限）。
- **master 経路の後方互換**: master/単一/既存クラウドの wire・plist・enroll は
  バイト不変であること（回帰で見張る）。
- **代替案（不採用）**: 「slave が Firestore REST を scoped custom token で直叩き」型は
  rules で write は絞れるが、**grant が sid 単位で pc 非紐付け＝relay の identity 対応が
  結局必須**。ゆえに relay 仲介型が上位互換（本設計を採用）。

## 9. 鉄則（DESIGN.md 継承）
1. 推測修正をしない — 実再現してから直す
2. 実テストで担保 — 実 herdr・実 GCP・実 relay の敵対 e2e。合成で緑にしない。
   修正前に旧コードで FAIL を確認
3. ヒューリスティック分類をしない — identity は exact-match（token.pc / sid∈pc）
4. herdr ソースの vendor 禁止（AGPL 衛生）
5. master 経路はバイト不変（後方互換の絶対条件）
