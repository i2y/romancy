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

- **supervisor 移植時の注意**: （2026-11-27 以降は 3 言語とも recur で同じ ID を使うので、recur チェーンをたどる必要はない。当時の edda/shikibu は recur で新インスタンスを作っていた）。子インスタンスには正しい `source_hash` が必要 — 空だと resumable sweep から見えず永久に進まない
- **romancy の Recur に潜在バグがあった**: 新 input が履歴にだけ書かれ `workflow_instances.input_data` が更新されず、全イテレーションが元 input で再起動していた。`UpdateInstanceInputIfLocked` で修正済み（commit cda1e53）。shikibu では `archive_history` のカラムリスト抜けを修正（commit 60bd58e）
- **クロス言語テスト flake**: edda の `test_cross_language_channel` で Ruby→Python 配送がタイムアウトすることがある（環境依存、既存の flake）


---

## 2026-11-26 追記：レビューで見つかったバグの型（移植チェックリスト）

2026-11-25 実装分（actor primitives・supervisor・sandbox）を 3 言語でレビューして直した際、同じ型のバグが言語をまたいで繰り返し見つかった。新しいプリミティブや新しいポートを書くときのチェックリストとして残す。

### リプレイ
- workflow コードから storage を直接呼ぶプリミティブは、リプレイのたびに再実行される。記録付きステップにすること（romancy `steps.go` の runStep/runEffect、edda `context._run_step`、shikibu `WorkflowContext#step`）。読み取り（Whereis、ListTasks など）も記録しないと、リプレイで分岐が変わる
- ステップの結果はエラーも含めて記録する。センチネルはコードとして保存し、リプレイ後も判定できるようにする（romancy `stepRecord.Code`）
- activity 本体の中ではステップ ID を生成しない。リプレイでは activity 本体が走らないので、カウンタがずれる。直接実行し、activity を記録の単位にする
- `IsReplaying()` は run 単位の定数（historyCount > 0）で、「この呼び出しがリプレイか」ではない。これで判定したせいで、FS が最初のサスペンド以降すべて失敗していた

### fencing
- 副作用は「ロック証明 → 副作用 → 記録」を 1 トランザクションで行う。ロック証明を最後にすると、PG/MySQL で行ロックの順序が cancel と逆になってデッドロックする（shikibu で再現）
- PG の READ COMMITTED では、`INSERT … SELECT … WHERE EXISTS (locked_by = ?)` は行ロックを取らないので、同時の lease 奪取と原子的にならない。インスタンス行を `FOR NO KEY UPDATE`（MySQL は `FOR UPDATE`）でロックするか、条件付き UPDATE で証明する
- ロック喪失（LockLostError / ErrLockNotHeld）を activity のリトライ対象にしない
- 呼び出し側のトランザクションの中で fenced write が拒否されても、呼び出し側のトランザクションを rollback しない。SQLAlchemy の `AsyncSession.rollback()` は savepoint ではなく最外のトランザクションを巻き戻す
- edda の `workflow_lock`：heartbeat が owner task を cancel する方式では、呼び出し元（親の activity など）のタスクに cancel 要求が残る。`asyncio.timeout` と同じく `uncancel()` してから `LockLostError` に変換する。start() はリースを失っても例外にせずインスタンス ID を返す（新しいオーナーが続ける。romancy の StartWorkflow と同じ）
- Recur の archive・掃除・入力更新・recurred 化もロック証明つきの 1 トランザクションにする。リースを失った run の Recur が新オーナーの履歴をアーカイブしていた（romancy・shikibu）

### ステータスと待機
- 終端ステータス（completed/failed/cancelled）を吸収的にする。条件付き UPDATE に `status NOT IN (終端)` を入れる
- requeue（waiting → running）では待機登録（activity_id/timeout_at）も同時に消す。残すと、古いタイムアウトが完了済みインスタンスを再実行する
- receive タイムアウトを発火する前に、待機の activity_id/timeout_at が変わっていないこと、インスタンスがまだ待機中であることを確認する
- 配信は待機登録済みの購読者にだけ行う。待機していない購読者に competing メッセージを渡すと失われる
- タスク通知や DOWN のように「挿入するだけで誰も起こさない」メッセージで待つ経路は、requeue のバックストップでしか起きない。WaitTask/WaitAnyTask のタイムアウトが発火しないバグが 3 言語すべてにあった

### SQL 方言
- MySQL の `?` は、直後が識別子の先頭になれない文字のときだけプレースホルダになる。`?1` は構文エラー
- MySQL の RowsAffected は既定で「変更した行数」なので、同じ値の書き込み（同一秒の再取得・refresh）は 0 になる。Go のドライバでは `clientFoundRows=true` が必要
- MySQL は `UPDATE t … WHERE id IN (SELECT … FROM t)` を拒否する（エラー 1093）。候補を先に SELECT する
- PG の `$n` を型の違う 2 か所で使うと型推論エラー（42P08）になる。パラメータを分ける
- PG ではトランザクション内の制約違反がトランザクション全体を abort する。`ON CONFLICT DO NOTHING` か savepoint を使う
- SQLite（WAL）では、deferred トランザクションの「読んでから書く」が即座に失敗する（別接続が書き込み中なら `SQLITE_BUSY`、読み取り後に別接続がコミットしていれば `SQLITE_BUSY_SNAPSHOT`。どちらも busy timeout は効かない）。最初が読み取りのカーネル内部トランザクションは `BEGIN IMMEDIATE` にする。ユーザーコードを含む activity トランザクションは deferred のまま、競合したらその試行をトランザクションごとやり直す（romancy `attemptInTransaction`）。同じトランザクション内でのリトライでは直らない
- activity のリトライは試行ごとに新しいトランザクションで行う（edda/shikibu は最初からそう。romancy は 2026-11-27 まで全試行を 1 トランザクションで実行していて、失敗した試行の書き込みが成功時にコミットされ、PG では aborted トランザクションのせいでリトライが必ず失敗していた）
- Ruby の sqlite3 gem で Sequel の `busy_timeout` を使うと、GVL を握ったまま待つ。`busy_handler_timeout` を使う
- MySQL の `INSERT IGNORE` は重複だけでなく、切り詰めや FK 違反まで警告にして黙って続ける。重複キーのエラー（1062）だけを no-op にする
- MySQL のセッションは READ COMMITTED に固定する（既定の REPEATABLE READ は古いスナップショットとギャップロックを持ち込む）
- MySQL の識別子の比較はデータベースの照合順序に従う（既定の `utf8mb4_0900_ai_ci` は大文字小文字とアクセントを区別しない）。列ごとに照合順序を変えると、capability の target と、それが守るチャネルやパスの比較がずれて封印を迂回できる
- SQLite の `datetime()` も MySQL の `TIMESTAMP(0)` も秒精度。期限を `<=` で比べるなら秒単位に切り上げて保存しないと、最大 1 秒早く発火する（`<` で比べる側は切り上げると 1 秒遅れる）

### 時刻
- DB 側の `NOW()`/`CURRENT_TIMESTAMP` はセッション TZ に従う。アプリ側の UTC 時刻と混ぜる箇所があるなら、セッション TZ を UTC に固定する（PG `timezone=UTC`、MySQL `time_zone='+00:00'`）
- テストは非 UTC のホスト TZ と非 UTC の DB サーバー TZ の両方で回す。shikibu は JST ホストでだけ通るテストが、UTC/US ホストで 10 分〜8 時間 resume が止まるバグを隠していた

### 「形式モデル」節の訂正

上の「形式モデル（3プロジェクト共通）」節にある「TLC 全構成グリーン済み、Go checker 状態数一致」「新プロトコル 36 状態全網羅」は言い過ぎだった。2026-11-26 のレビューで次のことが分かった。

- InstanceClaim_old と EpochFencing_old/_new の 3 config は、演算子の優先順位と `UNCHANGED` の誤りで TLC が評価できない。「verified」は Go checker の結果だった
- CompetingMessageClaim は consume を 1 ステップにまとめているので、「高々 1 回」が構造上自明になっている

詳細は ACTOR_OS_DESIGN.md の「TLA+ スイートのレビュー結果」を参照。

---

## 2026-11-27 追記：決定と、移植時に揃えること

SEMANTICS.md に反映した決定（詳細は ACTOR_OS_DESIGN.md の「2026-11-27 の決定」）。新しいポートでも同じにする。

- **Recur は同じ ID で続く**：archive・購読/タイマー/グループの掃除・補償の登録の削除・入力更新・recurred 化をロック証明つきの 1 トランザクションで行う。broadcast の配信カーソルは残す。recurred を拾うスイープを必ず用意する（commit 後にクラッシュしても次の iteration が走るように）
- **記録するエラーはカーネルのコードを持つものだけ**。ドライバのエラーなどは run を中断する：記録せず、その run の以後のプリミティブと activity をすべて同じ中断エラーで失敗させ、complete/fail/補償をせずにロックを解放する。Python は `BaseException`、Ruby は `Exception` の派生にして、普通の `except`/`rescue` で握りつぶされないようにする
- **ロック喪失とシャットダウンで補償を走らせない**（romancy は、ステップが `ErrLockNotHeld` を返すと補償していた）
- **activity の諦めた失敗は transactional でも記録する**
- **capability**：期限切れの掃除は各 `(kind, target)` の最新の grant を残す（MySQL は DELETE のサブクエリで同じテーブルを参照できないので派生テーブルで包む）。revoke/unregister は所有者だけ。鍵があれば secret は `HMAC-SHA256(key, capability_id)` の 16 進小文字、記録は `{"id","kind","target","derived":true}`。CloudEvents の受信口も封印を確認し、`capabilitytoken` 拡張属性を保存前に取り除く
- **direct チャネルの秘匿**：`L:X` の X 判定は `:` の全分割点で行う（インスタンス ID が `:` を含みうる）

