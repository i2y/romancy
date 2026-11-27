# Actor OS — 正規仕様・権限モデル設計メモ

Romancy/Edda/Shikibu の3実装を「1つのアクターランタイムの3言語バインディング」
として再定義する際の設計メモ。コードの統合ではなく、**プロトコル（スキーマ+
セマンティクス+形式仕様）の統合**を目指す。

## 1. 概念写像（既存カーネルの再解釈）

| Actor OS 概念 | 既存対応物 | 備考 |
|---|---|---|
| アクター | workflow instance | 永続・一意ID・内部状態(history)を持つ |
| メールボックス | channel (direct/competing/broadcast) | direct=宛先指定、competing=worker queue、broadcast=pubsub |
| スケジューラ | lock/lease/resume | 同一アクターの実行者は常に1つ。lease 喪失→再 claim=アクターマイグレーション |
| 永続化 | 決定論リプレイ+history | event sourcing 的。アクター状態=入力+履歴の再生 |
| `send_after` | timer subscription | expires_at 基準で missed wakeup が構造的に起きない |
| プロセスグループ | workflow_group_memberships | Erlang pg 相当。スキーマ済み、API は romancy のみ実装 |
| 監視(supervision) | stale lock→auto resume | 現状は暗黙の one-for-one。戦略化が課題 |
| IPC(言語間) | cross-language channel | 同一DBを通じて既に動作済み |

## 2. 正規仕様の切り出し

「Actor OS 規格」として以下を1セットにまとめる（durax-io/schema と同じく
共有物として管理するのが正しい）：

1. **DBスキーマ** — 既存 shared schema + 下記拡張
2. **TLA+ specs** — `spec/InstanceClaim.tla`・`WaitRegistration.tla`・
   `CompetingMessageClaim.tla`。言語非依存のカーネル正当性規格として、
   新実装が準拠すべき「憲法」と位置づける
3. **セマンティクス文書** — 状態遷移、claim/takeover規則、fenced-write規則、
   消費=削除規則、wait登録の原子的規則（EDDA_PORT_NOTES.md の知見を正規化）

## 3. アクター API（薄い上層）

各言語ランタイムの上に載せる命名層。既存APIの別名+追加概念のみ：

- `spawn(workflow, input) -> actor_id` — create_instance + start の糖衣
- `send(actor_id, msg)` — direct channel への publish
- `receive()` — channel receive
- `register(actor_id, name)` / `whereis(name)` — 名前レジストリ
  （`channel_subscriptions` 拡張または新テーブル `actor_names`）
- `monitor(actor_id)` — 終了時にダウンメッセージを direct channel で配送
  （実装は supervisor が storage をポーリング or `system_locks` 系の監視行）
- **supervisor = ただのアクター**：子の status を監視し、戦略
  （one_for_one/one_for_all/rest_for_one、restart intensity）に従って
  `resume`/spawn し直すワークフロー。戦略は DB ではなくコードで表現。

## 4. 権限モデル — capability ベース

現状は「DBアクセス権=root」。untrusted エージェントを載せるための
最小限の権限機構。

### 4.1 原則

- **capability モデル**（ACL ではなく）：権限は「持っているだけで使える
  不可偽造トークン」。spawn した親が子に能力を委譲する形で連鎖する
- **`locked_by` エポック機構と同型**：等価チェックで書き込みをフェンスする
  土台に、権限トークンを載せるだけで済む

### 4.2 トークン種別

| capability | 付与操作 | 検証場所 |
|---|---|---|
| `actor:{id}:{secret}` | spawn | send/cancel/status取得 |
| `channel_pub:{name}:{secret}` | subscribe時or親委譲 | publish |
| `channel_sub:{name}:{secret}` | 親委譲 | receive/drain |
| `fs_write:{path}:{secret}` | mount/委譲 | blob/FS 書込 |

形式: `HMAC(secret, kind+id)` をカラムに格納し、操作時に照合。
**平文 secret はDBに置かない**（hash のみ）。親は spawn 時に子へ
input_data 経由で secret を渡す（親→子の能力注入は input に紛れ込ませる
のが自然。子は自分の能力しか知らない）。

### 4.3 スキーマ追加案

```sql
-- capability テーブル（発行済み権限の台帳。revocation は行削除）
actor_capabilities(
  id TEXT PK,          -- cap token id
  kind TEXT NOT NULL,  -- 'actor'|'channel_pub'|'channel_sub'|'fs_write'
  target TEXT NOT NULL,-- actor_id | channel名 | fs path
  secret_hash TEXT NOT NULL,
  granted_by TEXT,     -- 委譲元 capability id
  expires_at TIMESTAMP,
  created_at TIMESTAMP
)
```

検証は「操作する storage メソッド」層で一括（fenced write と同じパターン：
`WHERE target=? AND secret_hash=?` 条件を UPDATE に混ぜる）。

### 4.4 段階導入

- Phase A: 能力チェックを**オプション**として実装（既存コードは全権限）
- Phase B: `sandboxed: true` フラグ付き spawn で子の能力を限定
- Phase C: PG の RLS による DB層での強制（方言非対称、性能要検証）

## 5. ファイルシステム層

2種類を明確に分離する：

### 5.1 blob store（ペイロード参照用）

- 目的: 大きなメッセージ/activity結果を channel/DB に直接埋め込まない
- 形式: `blob(hash)` CAS。`channel_messages.payload`/`history.event_data` に
  `blob:sha256:...` 参照を許容
- 権限不要（内容アドレスなので推測困難、誰でも読めるが書けない）

### 5.2 共有 FS（エージェント間共有メモリ）

- `fs_nodes(path PK, kind, blob_hash or inline_data, owner_actor, version, updated_at)`
- パス階層: `/shared/...`（全員読める）+ `/actor/{id}/...`（owner 権限）
- 書込は `fs_write` capability 必須。version は OCC（楽観的ロック）
- **リプレイ決定性との相性**: FS 参照は「外部世界の読み書き」として
  activity 経由に限定し、workflow コードから直接参照しない規約
  （replay 時に FS 状態が変わると決定性が崩れるため）

## 6. 実装順序の提案

1. **監視+レジストリ**（小）— actor_names + monitor/monitor通知
2. **capability トークン**（中）— channel 操作に能力検証を段階導入
3. **blob store**（小〜中）— CAS テーブル + ペイロード参照
4. **共有 FS**（大）— セマンティクス確定後に実装
5. **規格の切り出し** — durax-io/schema にスキーマ+TLA+規格を同梱

## 7. リスク・未決事項

- **スコープ膨張**: カーネル固め直後に新機能を積むと schema churn が起きる
  （共有 submodule の上流調整コストも要計上）
- **RLS の方言非対称**: PG では強力な手段になるが SQLite/MySQL では
  アプリ層チェックに留まる。両立は設計上の制約として確定させるべき
- **capability と fencing の関係**: fenced write（lock epoch 保持者のみ
  書ける）と capability（権限保持者のみ操作できる）は別軸。両方必要 —
  「誰の書き込みか」（epoch）と「書く権利があるか」（capability）
- **エージェントの untrusted 範囲**: アクターが外部プロセス/別DBアクセスを
  持つ場合、DB層の権限だけでは不十分。ネットワーク/API 層のゲートウェイが
  別途要るかどうかは用途次第
- **マルチテナンシー**: `framework` 列によるフィルタは言語識別用で、
  テナント分離ではない。本格的な OS 化には namespace/tenant 概念を
  追加すべきか（`instances.tenant` 列等）— 未決

---

## 実装状況アップデート（2026-11-25 時点）

本メモの構想に対し、以下が3言語（romancy/edda/shikibu）で実装済み：

| 項目 | 状態 |
|---|---|
| supervision tree ライブラリ | ✅ 実装済み — one_for_one / one_for_all / rest_for_one、restart intensity 制限、世代つき決定的 child ID（`{supID}:{name}:{gen}`）。poll authoritative（DOWN/notify は起床信号） |
| sealed channel の capability 強制 | ✅ 実装済み — `channel_pub`/`channel_sub` の grant が存在するチャネルはトークン必須、なければオープン |
| orphan task reclaim | ✅ 実装済み — `claimed_at`+`attempts` 列、300s の stale sweep で `open` に復帰 |
| タスク claim プロトコルの形式検証 | ✅ `schema/spec/TaskClaim.tla` — CAS ありで二重 claim 不可を検証 |
| 規格の切り出し（§6-5） | ✅ `durax-io/schema` に `SEMANTICS.md` + TLA spec + Go checker + TLC CI を集約 |
| sandboxed actor（Phase B） | ✅ 3言語で実装済み — `sandboxed` 永続化、mint 拒否、spawn/recur/supervision 伝播。schema repo へ反映済みで、4リポの submodule pin もワーキングツリー上で更新済み |

### 重要な利用上の注意（形式検証で裏付け）

**タスク実行と one_for_all の兄弟リスタートは at-least-once。**

- タスクの claim には heartbeat がないため、300 秒を超える正規の実行は reclaim され、別ワーカーが**同じ仕事を再実行しうる**。完了書き込みは `claimed_by` の fence で守られる（DB 行の clobber は起きない — `schema/spec/TaskReclaim.tla`）が、副作用の二重実行は防げない。**タスクハンドラは冪等に書くこと**
- one_for_all / rest_for_one で生きている兄弟を止める cancel は**条件付き**（2026-11-26 修正）。cancel が間に合わず兄弟が先に終わっていた場合は、実際のステータスに restart mode を適用する（正常終了した transient/temporary は再起動しない、permanent は再起動する）。終端ステータスは吸収的なので、cancel 済みの子が復活することもない。修正前は cancel の拒否を無視して新世代を spawn していた。なお `schema/spec/SupervisorRestart.tla` の「信頼できる cancel が必要」という説明は仕組みを取り違えていて、必要なのは「0 行なら respawn しない条件付き cancel」と「終端の吸収性」の 2 つ（下記 TLA+ レビュー参照）。**子アクターの副作用は at-least-once のままなので冪等であること**

---

## Phase B 実装ハンドオフ（2026-11-25 確定設計）

### 前提の重要な発見

**delegation に新機構は不要。** capability トークンは `cap:{kind}:{target}:{secret}` 形式の文字列なので、親は spawn input にトークンを含めるだけで委譲が完了する。実装が必要なのは制限側のみ。

### スキーマ（durax-io/schema、migration 作成・schema repo 反映済み）

```sql
-- 20261207000000_add_sandboxed.sql（sqlite/postgres/mysql 3方言すべて）
ALTER TABLE workflow_instances ADD COLUMN sandboxed INTEGER NOT NULL DEFAULT 0;
```

注意: schema repo への push はユーザー承認が必須（前例: 確認なく push しない）。push 後に4リポ（romancy/edda/shikibu/kairo）の pin 更新。

### 強制ルール3つ（これが全て）

1. **sandboxed は mint できない** — romancy では `IssueCapability`（actors.go:99）で自インスタンスの sandboxed を確認して拒否。edda/shikibu の対応箇所も同様
2. **sandboxed 親の子は強制 sandboxed** — spawn/CreateInstance 経路で親が sandboxed なら子も1に固定。脱サンドボックス不可
3. **`sandboxed: true` は spawn オプション** — 非 sandboxed 親のみ指定可能

### 実装ステップ

1. schema migration 3方言 + push（要承認）+ pin 更新 — migration/schema repo 反映済み、pin 更新は各リポのワーキングツリーで実施済み
2. romancy: ストレージ3ドライバ（sqlite/postgres/mysql）の insert/select にカラム追加、`IssueCapability` チェック、spawn 伝播、テスト — 実装済み
3. edda / shikibu へ同様の移植 — 実装済み

### 完成形（LLM エージェント用途）

親が `sandboxed: true` でエージェントを spawn し、必要なチャネル/FS のトークンのみ input で渡す。エージェントはそれ以外の権限を構造的に取得できない。in-process 強制の限界（ストレージ直叩きの迂回）は Phase C（PG RLS）の領域であることを README/SEMANTICS に明記すること。

### 関連する既知の未解決事項（優先度順）

- Recur / トランザクショナル publish / saga 補償の TLA+ モデル未作成
- タスクの heartbeat/renewal なし — 300s 超タスクは二重実行しうる（at-least-once、TaskReclaim.tla で証明済み）
- ~~one_for_all cancel race は設計固有（SupervisorRestart.tla）— 信頼できる cancel が必要なら別途設計~~ → 2026-11-26 解消（条件付き cancel + 終端ステータスの吸収性。「重要な利用上の注意」参照）
- edda の test_cross_language_channel は環境依存 flake


---

## 実装状況アップデート（2026-11-26、2026-11-25 実装分のレビュー後）

2026-11-25 時点で「実装済み」とした機能には、3 言語とも正しさの穴が多数あった。主なものは次のとおり。

- workflow コードから呼んだプリミティブがリプレイのたびに再実行される（§2 違反）
- PG/MySQL で動かない SQL（MySQL の `?N` プレースホルダ、PG の型推論エラーなど）
- セッション TZ やホスト TZ に依存する時刻比較（shikibu は JST ホストでしか正しく動かなかった）
- リースを失った run による書き込み、終了済みインスタンスの復活

レビューでこれらを修正し、SQLite/PG/MySQL で回帰テストを追加した。修正後に 3 言語で共通になったカーネル規則：

| 規則 | 内容 |
|---|---|
| プリミティブは記録付きステップ | workflow コードから呼ぶ名前・monitor・capability・task・チャネル読み取り・publish/send_to は 1 回だけ実行し、結果をエラーも含めて履歴に記録する。リプレイは記録を返す。activity 本体の中では直接実行する（activity が記録の単位） |
| 副作用は fenced transaction | ロック証明（lease を延長する条件付き UPDATE）→ 副作用 → 記録を 1 トランザクションで行う。リースを失った run は何も適用しない。失敗した副作用はロールバックし、エラーだけを別に記録する。ロック喪失とキャンセルは記録しない |
| 終端ステータスは吸収的 | completed/failed/cancelled から他のステータスへは戻らない。古いタイマー・待機・配信では復活しない |
| 待機登録は fenced | `locked_by` 一致を条件に登録する。requeue は status と待機（activity_id/timeout_at）を同時に消す。receive タイムアウトは待機が変わっていないときだけ発火する |
| competing の消費は原子的 | メッセージ行をロックしてから claim し、claim・受信記録・削除を 1 トランザクションで行う。配信は待機登録済みの購読者にだけ行う |
| 封印は direct の物理名にも及ぶ | チャネル `ch` の grant は `ch:{instance}` にも効く |
| DOWN は 1 回 | スイープは monitor 行の削除に成功したときだけ DOWN を出す。名前は、終了したインスタンスのものを monitor の有無にかかわらず解放する |
| supervisor | 各周期の終わりに poll 間隔だけ待つ（romancy は DOWN で早めに起きる Receive のタイムアウト、edda/shikibu は sleep）。生きている子は条件付き cancel で止め、拒否されたら実ステータスを読み直して restart mode を適用する |
| 時刻は UTC | PG/MySQL のセッション TZ を UTC に固定し、アプリ側も UTC で書く（edda は EddaApp が作るエンジンのみ）。romancy の MySQL 接続は加えて `clientFoundRows=true` を付ける（同一秒の再取得・refresh を「0 行 = 喪失」と誤判定しないため） |
| Recur は 1 トランザクション | romancy はロック証明つきで archive・購読/グループの掃除・入力更新・recurred 化をまとめる。shikibu はロック証明つきで archive・recurred 化・後継インスタンスの作成をまとめる（旧 ID の購読行は残るが、recurred には配信されないので実害はない）。edda は下記の差異を参照 |

### 3 言語で残っている差異

- **名前の重複エラー**：romancy は `ErrActorNameTaken`、edda は `ValueError`、shikibu は `ActorNameTakenError`（記録されるコードはどれも `actor_name_taken`）
- **cancel 後の書き込み**：edda は cancel で `locked_by` を消すので `LockLostError`、romancy は残すので `ErrInstanceTerminated`

### TLA+ スイート（schema/spec）のレビュー結果

判定は「部分的に妥当」。TLC が実際に回るモデルの安全性の議論は正しいが、主張ほど強くない。

- 19 config のうち 3 つは演算子の優先順位と `UNCHANGED` の誤りで TLC が評価できず、1 つ（ActorFS_new）は自分の liveness 性質に違反する。コメントの「verified」は Go checker の結果だった
- CI は 6 config しか回さず、終了コードを `|| true` で捨てて grep で判定しているので、上の失敗を検出できない
- EpochFencing は worker ごとに資格情報が 1 つなので、エポックの存在理由である「同じ worker の再取得」を表現できない
- CompetingMessageClaim は consume を 1 ステップにまとめていて、「高々 1 回」が構造上自明になっている。実装は複数の文に分かれていて、そこにバグがあった
- WaitRegistration はステータスと待機登録を 1 変数にまとめていて、requeue の掃除漏れが見えない
- 空虚な性質（TaskReclaim の `SingleWriter` など）と、Go checker と TLC の状態数のずれがある

修正パッチと新しいモデル（EpochFencingRuns、CompetingMessageClaimSplit、WaitRegistrationSub、SupervisorRestartCAS ほか）は 2026-11-27 に schema に適用した。CI は `spec/tlc-matrix.txt` の全 config（55 個）を TLC の終了コードで判定し、tla2tools のバージョンとチェックサムを固定する。

### 2026-11-27 の決定（SEMANTICS.md に反映し、3 言語で実装）

| 項目 | 決定 |
|---|---|
| Recur のアイデンティティ | 3 言語とも同じ ID で続く。archive・購読/タイマー/グループの掃除・補償の登録の削除・入力更新・recurred 化をロック証明つきの 1 トランザクションで行い、broadcast の配信カーソルは残す。`continued_from` はもう書かない。supervisor の ID も子の ID も recur で変わらない |
| activity の失敗 | transactional でも諦めた失敗を `ActivityFailed` として記録する。リプレイは記録した失敗を返す |
| 記録するエラー | カーネル定義のコード（`task_conflict`・`task_not_found`・`actor_name_taken`・`fs_conflict`・`sandboxed`・`channel_forbidden`・`not_owner`・`cursor_not_found`・`invalid_argument`）を持つものだけ。それ以外は run を中断する（記録せず、complete/fail/補償もせず、ロックを解放して `running` のまま残す）。ロック喪失とシャットダウンも同じ。封印チャネルでのブロッキング受信の拒否も記録する |
| 補償中の cancel | 3 言語とも拒否する |
| capability | 期限切れでも封印は解けない（掃除は最新の grant を残す）。revoke は発行者だけ、unregister は名前の保持者だけ（他は `not_owner`）。鍵（オプションか `DURAX_CAPABILITY_KEY`）があれば secret は `HMAC-SHA256(key, capability_id)` で、履歴にトークンを残さない。CloudEvents の受信口も封印を守る（`capabilitytoken` 拡張属性、なければ 403） |
| カーソル読み取り | `read_channel_since` はカーソルのメッセージが消えていたら `cursor_not_found`。数値 ID で読む `read_channel_after` を追加 |
| direct チャネル | 他インスタンスの `L:X` の購読・受信・読み取りは封印の有無にかかわらず拒否。送信は許す |
| MySQL | セッションを READ COMMITTED に固定。インスタンス ID を持つ列（`granted_by`・`owner`・`claimed_by`・`created_by`）を `VARCHAR(255)` に広げた（migration `20261208000000`）。識別子の比較はデータベースの照合順序に従う（バイナリにしたければデータベースごと `utf8mb4_0900_bin` で作る。列単位では変えない） |
| SQLite | ファイル DB は 3 言語とも WAL。読み取りから始まる activity の書き込み競合は、その試行をロールバックして新しいトランザクションでやり直す（リトライに数えない） |
| 期限 | タイマーと受信タイムアウトは早く発火しない（秒精度の列に合わせて、`<=` で比べるタイマーの期限は秒単位に切り上げる） |
| TLA+ | 修正パッチと新モデルを schema に適用し、CI で全 config を検証する |

