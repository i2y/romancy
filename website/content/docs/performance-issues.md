---
title: "Performance Notes"
weight: 101
draft: true
---

このドキュメントは、Edda (Python版) で発見されたパフォーマンス問題をRomancyでも調査した結果です。
他のセッションへの引き継ぎ用に作成しました。

**作成日**: 2025-12-07
**調査対象**: Romancy (Go版 Edda)

---

## 概要

| # | 問題 | 深刻度 | 状態 | 修正コスト |
|---|------|--------|------|-----------|
| 1 | Thundering Herd + 同期ポーリング | CRITICAL | ❌ 未対応 | 低 |
| 2 | ロック失敗時バックオフなし | CRITICAL | ❌ 未対応 | 低 |
| 3 | 履歴全件ロード | CRITICAL | ❌ 未対応 | 高 |
| 4 | 複合インデックス不足 | HIGH | ❌ 未対応 | 低 |
| 5 | 無制限結果セット | HIGH | ⚠️ 一部対応 | 低 |
| 6 | JSONシリアライズ三重処理 | HIGH | ❌ 未対応 | 中 |
| 7 | Two-Phase Delivery (6 DB往復) | MODERATE | ❌ 未対応 | 中 |
| 8 | インスタンスキャッシングなし | MODERATE | ⚠️ 設計上 | 低 |
| 9 | 分散コーディネーションなし | MODERATE | ❌ 未対応 | 高 |
| 10 | N+1クエリ | - | ✅ 解決済み | - |
| 11 | NOT INサブクエリ | - | ✅ 解決済み | - |

---

## 問題詳細

### 1. Thundering Herd + 同期ポーリング間隔 (CRITICAL)

**場所**: `app.go` の全バックグラウンドタスク

**現状**:
```go
// 固定間隔、ジッターなし
ticker := time.NewTicker(a.config.staleLockInterval)    // 60秒
ticker := time.NewTicker(a.config.timerCheckInterval)   // 10秒
ticker := time.NewTicker(a.config.workflowResumptionInterval) // 1秒
```

**問題**:
- 10ワーカーが同時起動 → 全員が同じタイミングでDBクエリ → 負荷スパイク
- t=0, t=1, t=2... で全ワーカーが同期してクエリ
- ピーク負荷が N倍 (Nはワーカー数)

**修正案**:
```go
import "math/rand"

// ジッター付き間隔を計算
func addJitter(d time.Duration, percent float64) time.Duration {
    factor := 1.0 + percent*(2*rand.Float64()-1)
    return time.Duration(float64(d) * factor)
}

// 使用例: 25%ジッター → 0.75〜1.25倍の範囲でランダム化
ticker := time.NewTicker(addJitter(interval, 0.25))
```

**影響を受ける関数**:
- `runStaleLockCleanup()`
- `runTimerCheck()`
- `runEventTimeoutCheck()`
- `runMessageCheck()`
- `runRecurCheck()`
- `runChannelCleanup()`
- `runWorkflowResumption()`
- `outbox/relayer.go` のポーリングループ

---

### 2. ロック失敗時のバックオフなし (CRITICAL)

**場所**: `app.go:1172-1179`

**現状**:
```go
func (a *App) resumeResumableWorkflows() error {
    workflows, err := a.storage.FindResumableWorkflows(a.ctx)
    // ...
    for _, wf := range workflows {
        go func(wf *storage.ResumableWorkflow) {
            if err := a.resumeWorkflow(a.ctx, wf.InstanceID); err != nil {
                // バックオフなし、即座に諦める
            }
        }(wf)
    }
    return nil
}
```

**問題**:
- 100ワークフローを同時にgoroutine起動
- ロック失敗時は即座に諦める（バックオフなし）
- N ワーカー × 100 ワークフロー = 毎秒大量のロック競合

**修正案**:
```go
const maxConcurrentResumptions = 10

func (a *App) resumeResumableWorkflows() error {
    workflows, err := a.storage.FindResumableWorkflows(a.ctx)
    if err != nil {
        return err
    }

    // セマフォで同時実行数を制限
    sem := make(chan struct{}, maxConcurrentResumptions)

    for _, wf := range workflows {
        sem <- struct{}{}  // Acquire
        go func(wf *storage.ResumableWorkflow) {
            defer func() { <-sem }()  // Release

            // 指数バックオフ付きで再開を試みる
            // retry/policy.go の Exponential を活用可能
            _ = a.resumeWorkflow(a.ctx, wf.InstanceID)
        }(wf)
    }

    return nil
}
```

---

### 3. 履歴の全件ロード (CRITICAL)

**場所**:
- `internal/storage/sqlite.go:471-501`
- `internal/storage/postgres.go:469-499`

**現状**:
```go
func (s *SQLiteStorage) GetHistory(ctx context.Context, instanceID string) ([]*HistoryEvent, error) {
    rows, err := conn.QueryContext(ctx, `
        SELECT id, instance_id, activity_id, event_type, event_data, ...
        FROM workflow_history
        WHERE instance_id = ?
        ORDER BY id ASC
    `, instanceID)
    // 全件を []HistoryEvent に格納して返す
}
```

**問題**:
- 長時間ワークフロー（10,000アクティビティ）= 1GB+ メモリ
- O(N)処理時間、リプレイのたびに全件ロード

**修正案**:
1. ページネーション: `LIMIT/OFFSET` でバッチ処理
2. ストリーミング: イテレータパターンで逐次処理
3. Recur活用: 長時間ワークフローは `Recur()` で履歴をアーカイブ

**注意**: 修正コストが高い。リプレイエンジン全体の設計変更が必要。

---

### 4. 複合インデックス不足 (HIGH)

**場所**:
- `internal/migrations/sqlite/000001_init.sql`
- `internal/migrations/postgres/000001_init.sql`

**不足しているインデックス**:

```sql
-- FindResumableWorkflows() 用
-- WHERE status = 'running' AND (locked_by IS NULL OR locked_by = '')
CREATE INDEX idx_instances_status_locked ON workflow_instances(status, locked_by);

-- GetChannelSubscribersWaiting() 用
-- WHERE channel_name = ? AND waiting = 1
CREATE INDEX idx_channel_subs_channel_waiting ON channel_subscriptions(channel_name, waiting);

-- タイムアウトチェック用
CREATE INDEX idx_channel_subs_timeout ON channel_subscriptions(instance_id, timeout_at);

-- CleanupStaleLocks() 用
CREATE INDEX idx_instances_lock_status ON workflow_instances(locked_by, lock_expires_at, status);
```

**修正**: 新しいマイグレーションファイルを追加
- `internal/migrations/sqlite/000003_perf_indexes.up.sql`
- `internal/migrations/postgres/000003_perf_indexes.up.sql`

---

### 5. 無制限の結果セット (HIGH)

**場所**: 複数のストレージメソッド

| クエリ | LIMIT | 状態 | ファイル:行 |
|--------|-------|------|------------|
| `FindResumableWorkflows()` | 100 | ✅ OK | sqlite.go:339 |
| `ListInstances()` | cursor | ✅ OK | sqlite.go:284-290 |
| `GetPendingOutboxEvents()` | param | ✅ OK | sqlite.go:623-630 |
| `FindExpiredTimers()` | なし | ❌ | sqlite.go:587-608 |
| `FindExpiredChannelSubscriptions()` | なし | ❌ | sqlite.go:1187-1217 |
| `FindExpiredMessageSubscriptions()` | なし | ❌ | sqlite.go:1368-1394 |
| `CleanupStaleLocks()` | なし | ❌ | sqlite.go:424-430 |

**修正案**:
```go
// 各クエリに LIMIT 100 を追加し、バッチ処理に変更
func (s *SQLiteStorage) FindExpiredTimers(ctx context.Context) ([]*TimerSubscription, error) {
    rows, err := conn.QueryContext(ctx, `
        SELECT ...
        FROM workflow_timer_subscriptions
        WHERE expires_at <= datetime('now')
        ORDER BY expires_at ASC
        LIMIT 100  -- 追加
    `)
    // ...
}
```

---

### 6. JSONシリアライズ三重処理 (HIGH)

**場所**:
1. `activity.go:227-230` - compensation登録
2. `internal/replay/engine.go:407` - history記録
3. `internal/replay/engine.go:260` - output更新

**現状**: 1アクティビティにつき3回の `json.Marshal()`

```go
// 1回目: Compensation
inputData, err := json.Marshal(input)

// 2回目: History
resultData, err := json.Marshal(result)

// 3回目: Output (同じresultを再度)
resultData, err := json.Marshal(result)
```

**修正案**: 最初のシリアライズ結果をキャッシュして再利用

```go
type SerializedResult struct {
    Data []byte
    once sync.Once
}

func (s *SerializedResult) Get(v any) ([]byte, error) {
    var err error
    s.once.Do(func() {
        s.Data, err = json.Marshal(v)
    })
    return s.Data, err
}
```

---

### 7. Two-Phase Delivery (MODERATE)

**場所**:
- `internal/storage/sqlite.go:1105-1174`
- `internal/storage/postgres.go:1094-1163`

**現状**: `DeliverChannelMessageWithLock()` で6回のDB操作

1. `TryAcquireLock()` - UPDATE
2. `GetWorkflowInfo` - SELECT
3. `RecordHistory` - INSERT
4. `ClearWaitingState()` - UPDATE
5. `SetStatusRunning` - UPDATE
6. `ReleaseLock()` - UPDATE

**修正案**: 複数UPDATEを1クエリにバッチ化

```sql
-- 3, 4, 5 を1回のトランザクションで
BEGIN;
INSERT INTO workflow_history (...) VALUES (...);
UPDATE channel_subscriptions SET waiting = 0 WHERE ...;
UPDATE workflow_instances SET status = 'running' WHERE ...;
COMMIT;
```

---

### 8. インスタンスキャッシングなし (MODERATE)

**場所**: `app.go:498-504`

**現状**: `GetInstance()` が毎回DBクエリ

**注意**: これは設計上の選択。インスタンス状態は頻繁に変更されるため、キャッシュ無効化が複雑になる。
アクティビティ結果はすでにキャッシュされている（`context.go:25`, `replay/engine.go:51-52`）。

**修正優先度**: 低（現状維持で可）

---

### 9. 分散コーディネーションなし (MODERATE)

**現状**: 全ワーカーが独立して同じバックグラウンドタスクを実行

**問題**:
- 10ワーカーが全員 `FindExpiredTimers()` を実行
- リーダー選出なし
- 作業分散なし

**修正案** (高コスト):
1. リーダー選出: DBベースのロック（`system_locks` テーブルを活用）
2. シャーディング: `instance_id` のハッシュでワーカーを分担

**修正優先度**: 低（他の問題を先に解決）

---

## 推奨対応順序

### Phase 1: 低コスト・高効果（即時対応推奨）

| 優先度 | 修正内容 | ファイル |
|--------|----------|----------|
| 1 | ジッター追加 | `app.go`, `outbox/relayer.go` |
| 2 | 複合インデックス追加 | 新規マイグレーション |
| 3 | LIMIT追加 | `sqlite.go`, `postgres.go` |

### Phase 2: 中コスト

| 優先度 | 修正内容 | ファイル |
|--------|----------|----------|
| 4 | セマフォ + バックオフ | `app.go` |
| 5 | JSONシリアライズ最適化 | `activity.go`, `replay/engine.go` |

### Phase 3: 高コスト（将来対応）

| 優先度 | 修正内容 | ファイル |
|--------|----------|----------|
| 6 | 履歴ストリーミング | storage層, replay engine |
| 7 | 分散コーディネーション | 新規実装 |

---

## 参考: Eddaでの対応状況

| 問題 | Edda状態 |
|------|----------|
| Thundering Herd | ✅ 解決済み（ジッター追加） |
| バックオフなし | ✅ 解決済み |
| 履歴全件ロード | ⚠️ 部分対応 |
| 複合インデックス | ✅ 解決済み |
| N+1クエリ | ✅ 解決済み |
| print()ログ | ✅ 解決済み（logging導入） |

---

## 関連ファイル一覧

```
romancy/
├── app.go                      # バックグラウンドタスク、ジッター追加対象
├── options.go                  # 間隔設定
├── activity.go                 # JSONシリアライズ1回目
├── outbox/
│   └── relayer.go              # outboxポーリング、ジッター追加対象
├── internal/
│   ├── replay/
│   │   └── engine.go           # JSONシリアライズ2,3回目
│   ├── storage/
│   │   ├── sqlite.go           # LIMIT追加対象
│   │   └── postgres.go         # LIMIT追加対象
│   └── migrations/
│       ├── sqlite/
│       │   └── 000003_perf_indexes.up.sql   # 新規作成
│       └── postgres/
│           └── 000003_perf_indexes.up.sql   # 新規作成
└── retry/
    └── policy.go               # 指数バックオフ実装（再利用可能）
```
