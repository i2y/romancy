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
