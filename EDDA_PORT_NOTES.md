# Edda 移植メモ（romancy 並行制御強化の逆輸入）

romancy で実施した reliability 強化を Edda (Python) へ移植する際の作業メモ。
romancy 側の参照実装は `internal/storage/{sqlite,postgres,mysql}.go`、設計説明は
`CLAUDE.md` §15、形式モデルは `spec/`。

## Edda 移植状況（2026-02 実施）

実装済み（edda `storage/sqlalchemy_storage.py` / `channels.py` / `locking.py`）:
- `try_acquire_lock` → 単一の条件付き UPDATE + rowcount 判定（SELECT FOR UPDATE
  + UPDATE の2段を置き換え。timeout 読取は別の plain SELECT で、claim 自体は原子的）
- competing claim takeover：同一インスタンス冪等・`claimed_at` が
  `CHANNEL_CLAIM_STALE_SECONDS`(300s) 超過・owner が terminal（update→insert フォールバック）
- `get_pending_channel_messages` competing：「別 live インスタンスの fresh claim」
  のみで隠す形に変更（自分の claim は可視=replay drain）
- `register_channel_receive_and_release_lock` → tx 内 pending 再チェック +
  `registered: bool` 返却（False = 待ち登録せず status=running のままロック解放）
- `requeue_channel_waits_with_pending` 新設 + `auto_resume_stale_workflows_periodically`
  (leader タスク) に配線
- `receive`：pending を反復して claim（claim 失敗時の全体再試行を解消）、
  **履歴記録→メッセージ削除**の順序に修正（旧コードは delete→append で、
  クラッシュ時にメッセージ喪失し得た）
- outbox：`claimed_at` カラム追加（shared schema `20261123000000_outbox_claimed_at`、
  edda submodule にも同ファイル追加済み）、claim 時刻刻印、
  stale `processing` の `COALESCE(claimed_at, published_at)` 再 claim、
  mark_* 系で `claimed_at` クリア。edda は 'failed' を claim 対象に含める
  設計のため release-to-pending 相当は既に成立
- 回帰テスト `tests/test_channel_competing.py::TestCompetingClaimTakeover`
  （stale/terminal takeover・fresh claim 排他・冪等・register recheck・requeue sweep）

実装済み（エポック phase）:
- **エポック fencing + fenced writes**：`locking.new_lock_identity(worker_id)`
  → `worker_id#<hex>` を全 acquire 経路で発行。`update_instance_status`/
  `update_instance_activity`/`append_history` に `lock_id=` 引数、不一致で
  `LockLostError`。ctx が `lock_id` を保持し run 中の書き込みをフェンス
  （ctx.lock_id=None なら無フェンス — テスト/スタンドアロン ctx 互換）
- **heartbeat → キャンセル連携**：replay の `workflow_lock` に
  `refresh_interval=60` を配線。`refresh_lock` 失敗で owner task に
  `task.cancel()` — stale runner は実行継続しない
- **`claim_resumable_workflows`**：SKIP LOCKED select + UPDATE を1 tx に集約。
  `_resume_running_workflows` がエポック1つで batch claim→各行
  `resume_by_name(lock_id=)` で引き継ぎ
- `resume_workflow`/`resume_by_name`/`_execute_workflow_logic` に `lock_id`
  引数 — `already_locked` 経路で claim エポックを引き継ぐ
- 回帰テスト `tests/test_epoch_fencing.py` 6件
- leader `system_locks`：romancy と同じ判断で変更不要と結論

## 移植対象（romancy で実装・検証済み）

### 1. ロックプロトコル
- [ ] `try_acquire_lock` を check-then-update（SELECT FOR UPDATE + UPDATE）から
      **単一の条件付き UPDATE + affected-rows 判定**に置き換え。
      Edda の現行実装は専用セッション+tx なので正しいが、条件付き UPDATE 版は
      1 RTT・tx 不要でこちらが強い（romancy `TryAcquireLock` 参照）。
- [ ] heartbeat：実行中は `RefreshLock` 相当を定期発行しリースを延長。
      `ErrLockNotHeld` 相当を検出したら run をキャンセル。
- [ ] fenced writes：status/output/history の全書き込みを
      `WHERE locked_by = worker` 付き UPDATE に（Edda の `update_*` 系を確認）。
- [ ] `ErrInstanceAlreadyExists` のような型付き duplicate エラー。
- [ ] resumable 取得の workflow 名フィルタ + `source_hash` 一致チェック。
- [ ] `claim_resumable_workflows`：find+claim の一括化
      （PG: CTE+FOR UPDATE SKIP LOCKED、MySQL: tx、SQLite: 単一UPDATE）。
      Edda の `find_resumable_workflows` は素の SELECT なので2フェーズのまま。

### 2. Outbox
- [ ] Edda の outbox は skip-locked + `processing` マーク済み（正しい）。
      romancy で追加した **stale claim 回収**（romancy は専用 `claimed_at` カラムを追加済み — migration `20261123000000_outbox_claimed_at`。stale 判定は `COALESCE(claimed_at, published_at)`。edda も同カラムを使うか、published_at 兼用を維持するなら COALESCE 互換を必須とする。claim 時刻が
      threshold より古い `processing` を他リレーヤーが再 claim）と
      **release-to-pending**（送信失敗時に即 pending 復帰）を移植。

### 3. チャネル待機登録（missed wakeup）
- [ ] wait 登録+status 更新+ロック解放を**単一トランザクション**にし、
      tx 内で pending を再チェック。pending があれば待ち登録しない
      （`registered=False` を返し status=running のままロックのみ解放）。
      romancy `RegisterChannelReceiveAndReleaseLock` 参照。
- [ ] `RequeueChannelWaitsWithPending` 相当のバックストップ sweep
      （waiting+unlocked で pending あり → running 復帰）を leader の定期
      cleanup に配線。Edda の RC でも publisher scan の残余レースは残るため。

### 4. Competing チャネルの claim（★ 今回 romancy で修正したバグ群）
- [ ] **UUID キー**：`channel_message_claims.message_id` は
      `channel_messages.message_id`（UUID 文字列）への FK。
      数値 row id を挿入すると JOIN 不一致（claim が効かず pending が減らず
      無限再試行）／PG では型・FK 違反。Edda 側の claim 実装が
      `ChannelMessage.id`（数値）を挿入していないか確認すること ——
      romancy はこのバグがあった。
- [ ] takeover 条件：同一インスタンス（冪等）・`claimed_at` が
      300s（`ChannelClaimStaleSeconds`、ロックリースと整合）より古い・
      owner が `completed/failed/cancelled`。orphan claim がメッセージを
      永久に隠さない。
- [ ] pending クエリ全箇所に同じ条件を適用（get_pending・has_pending・
      requeue sweep の3箇所。drift すると sweep が治せない取りこぼしが残る）。
- [ ] deliver 経路でも competing は履歴記録前に claim。
      他者の fresh claim があればロック解放してスキップ。
- [ ] **消費=削除**：competing の受信履歴を書いたら直後に
      claim+メッセージを削除する。残すと「自分の claim は pending に
      可視」と組み合わさって次回の receive が同じメッセージを再配信する
      （romancy で実際に発生したバグ）。削除 API 側で claim を先に
      明示削除してからメッセージを消すこと（FK cascade 頼みは
      SQLite の `PRAGMA foreign_keys` が OFF だと効かない）。

### 5. その他
- [ ] `claimed_at` 等のタイムスタンプはアプリ側 UTC 時刻で書き込む
      （DB の `NOW()`/`CURRENT_TIMESTAMP` はセッション TZ 依存で、
      UTC 比較と混ざると非UTC環境で fresh claim が stale 扱いされ得る。
      romancy では PG/MySQL の takeover UPDATE も Go 側 `time.Now().UTC()`）。
- [x] fencing token：**エポック方式で romancy 実装済み**。`locked_by`
      カラムに平文 workerID ではなく `workerID#<random>` の保持者IDを
      格納し、claim エポックごとに新発行（`storage.NewLockIdentity`）。
      `...IfLocked` の `locked_by = ?` チェックがそのまま fencing に
      なるためスキーマ変更・マイグレーション不要 —— 共有 submodule を
      触らずに済む決定的な利点。同一 workerID 再起動/lease 喪失→再獲得は
      別エポック=別保持者として拒否される。
      Edda 移植時：claim するたび `worker_id + "#" + uuid` を生成し、
      そのエポック内の全ロック操作（heartbeat/fenced write/release）に
      同一値を使うだけでよい。`locked_by` の値を表示以外で worker_id と
      等値比較している箇所があればエポック対応に直すこと。
      romancy で詰まった点：`handleExpiredTimer` がエポック保持のまま
      resume を呼んでいた（旧来の自己再獲得に依存）→ release してから
      resume する形に直した。Edda でも同種の「claim 保持のまま別経路で
      再 claim」パターンを探すこと。
- [ ] 形式モデル：romancy の `spec/InstanceClaim.tla`・
      `spec/WaitRegistration.tla` は言語非依存のプロトコル記述なので、
      Edda の変更に対してもそのまま使える（Go checker 相当を Python で
      書くか TLC のみで検証）。

## 検証時に注意したこと
- `GetPendingChannelMessagesForInstance` は未購読インスタンスに nil を返す。
  pending ベースの回帰テストでは被検査インスタンスを必ず購読させること
  （購読なしだと pending が空で「隠れている」判定が偽陽性になる）。
- claim 系統合テストは `-tags=integration`（testcontainers）で
  実 PG/MySQL に対して実行。`channel_claim_integration_test.go` の
  `testCompetingClaims` を両ドライバから呼ぶ構成。

## Shikibu 移植状況（2026-11 実施、edda と同一セット）

実装済み（`lib/shikibu/storage/sequel_storage.rb` / `channel_ops.rb` / `channels.rb` / `replay.rb` / `worker.rb`）:
- `try_acquire_lock` → 全 dialect で単一の条件付き UPDATE（Sequel、PG/MySQL の
  SELECT FOR UPDATE SKIP LOCKED 2段を置き換え）
- competing claim takeover：同一インスタンス冪等・`claimed_at` stale(300s)・
  terminal owner（`claim_message` / `claim_message_in_transaction`）
- `get_next_competing_message`：別 live インスタンスの fresh claim のみで隠す
- `register_channel_wait` 新設（tx 内 pending 再チェック + `registered` 返却）、
  `handle_channel_suspend` が使用。`requeue_channel_waits_with_pending` 新設 +
  `run_stale_lock_cleanup`(leader) に配線
- `deliver_channel_message`：competing で履歴記録前に claim、成功後に claim+メッセージ削除
- `receive`：competing と **direct 両方で claim**、履歴記録→削除（consume-delete。
  shikibu は direct が claim していなかった点も修正）
- outbox：`claimed_at` マイグレーション追加（shared schema、3 dialect）、
  claim 刻印、`COALESCE(claimed_at, published_at)` で stale `processing` 再 claim、
  mark_* で claimed_at クリア
- `Workflow#send_to` に `metadata:` 追加（crosstest の arity 不一致を修正）
- 新規 `ChannelOps` モジュールに claim/wait/requeue/pending 系を集約
  （ClassLength/ModuleLength cop の制約内に収めるため）
- spec: `spec/shikibu/competing_claim_spec.rb` 9件（takeover/stale/terminal/冪等/
  register recheck/requeue/outbox stale reclaim）

実装済み（エポック phase）:
- `new_lock_identity` → `worker_id#<hex>`、`update_instance_status`/
  `append_history` に `lock_id:` 引数（不一致で `LockLostError`）
- heartbeat スレッド：`refresh_lock` 失敗で run スレッドに
  `Thread#raise(LockLostError)`、`handle_lock_lost` で早期 return
- `claim_resumable_workflows`（for_update skip_locked + UPDATE、1tx）を
  `run_workflow_resumption` に配線
- spec: `spec/shikibu/epoch_fencing_spec.rb` 4件。164 examples 0 failures、
  rubocop 0 offenses

## 形式モデル（3プロジェクト共通）

romancy の `InstanceClaim.tla` は holder をエージェントとしてモデル化して
おり、エポックは「同 worker の再 acquire = 別エージェント」と同値 — 既存
spec の TakeLock/Die/Steal がそのままエポックプロトコルをカバー（TLC 全構成
グリーン済み、Go checker 状態数一致）。`CompetingMessageClaim.tla` も
takeover/liveness 検証済み（新プロトコル 36 状態全網羅）。バッチ claim は
per-row claim の集約で新規性が薄いため spec 追加は見送り、実機テストで
検証（edda `test_claim_resumable_atomic`、shikibu epoch spec、romancy
`TestClaimResumableWorkflows`）。

---

## 2026-11-25 追記

- **supervisor 移植時の注意**: edda/shikibu は recur で**新インスタンス**を作る（romancy は同一 ID で input 更新）。supervisor ループは recur チェーンをたどる設計にすること。子インスタンスには正しい `source_hash` が必要 — 空だと resumable sweep から見えず永久に進まない
- **romancy の Recur に潜在バグがあった**: 新 input が履歴にだけ書かれ `workflow_instances.input_data` が更新されず、全イテレーションが元 input で再起動していた。`UpdateInstanceInputIfLocked` で修正済み（commit cda1e53）。shikibu では `archive_history` のカラムリスト抜けを修正（commit 60bd58e）
- **クロス言語テスト flake**: edda の `test_cross_language_channel` で Ruby→Python 配送がタイムアウトすることがある（環境依存、既存の flake）

