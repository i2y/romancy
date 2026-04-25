//go:build pglocal

package storage

import (
	"context"
	"database/sql"
	"os"
	"sync"
	"testing"
	"time"
)

// Run with:
//   ROMANCY_TEST_POSTGRES_URL="postgres://romancy:romancy@localhost:5432/romancy_test?sslmode=disable" \
//     go test -tags=pglocal -count=1 -v ./internal/storage/ -run TestPGLocal

func pgLocalURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("ROMANCY_TEST_POSTGRES_URL")
	if url == "" {
		t.Fatal("ROMANCY_TEST_POSTGRES_URL must be set")
	}
	return url
}

func pgLocalSetup(t *testing.T) *PostgresStorage {
	t.Helper()
	store, err := NewPostgresStorage(pgLocalURL(t))
	if err != nil {
		t.Fatalf("NewPostgresStorage: %v", err)
	}
	ctx := context.Background()
	if err := InitializeTestSchemaPostgres(ctx, store); err != nil {
		store.Close()
		t.Fatalf("InitializeTestSchemaPostgres: %v", err)
	}
	pgLocalCleanup(t, store)
	return store
}

func pgLocalCleanup(t *testing.T, s *PostgresStorage) {
	t.Helper()
	ctx := context.Background()
	tables := []string{
		"channel_message_claims",
		"channel_delivery_cursors",
		"channel_messages",
		"channel_subscriptions",
		"workflow_group_memberships",
		"workflow_compensations",
		"outbox_events",
		"workflow_timer_subscriptions",
		"workflow_history_archive",
		"workflow_history",
		"workflow_instances",
		"workflow_definitions",
		"system_locks",
	}
	for _, tbl := range tables {
		if _, err := s.db.ExecContext(ctx, "TRUNCATE TABLE "+tbl+" RESTART IDENTITY CASCADE"); err != nil {
			t.Logf("truncate %s: %v (ignoring)", tbl, err)
		}
	}
}

// insertInstance inserts a row directly with a chosen framework value.
// Bypasses CreateInstance to allow framework='python'.
func pgLocalInsertInstance(t *testing.T, s *PostgresStorage, id, name, framework string, status WorkflowStatus) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	// Insert workflow_definitions first (FK)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO workflow_definitions (workflow_name, source_hash, source_code, created_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT DO NOTHING
	`, name, "test-hash", "// test", now)
	if err != nil {
		t.Fatalf("insert workflow_definitions: %v", err)
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO workflow_instances (
			instance_id, workflow_name, framework, status, input_data, source_hash,
			started_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $7)
	`, id, name, framework, string(status), "{}", "test-hash", now)
	if err != nil {
		t.Fatalf("insert workflow_instances: %v", err)
	}
}

// TestPGLocal_FindResumable_FrameworkFilter verifies the new
// `AND framework = 'go'` filter in FindResumableWorkflows.
func TestPGLocal_FindResumable_FrameworkFilter(t *testing.T) {
	s := pgLocalSetup(t)
	defer s.Close()
	defer pgLocalCleanup(t, s)
	ctx := context.Background()

	pgLocalInsertInstance(t, s, "go-1", "wf-go", "go", StatusRunning)
	pgLocalInsertInstance(t, s, "py-1", "wf-py", "python", StatusRunning)
	pgLocalInsertInstance(t, s, "py-2", "wf-py", "python", StatusRunning)

	got, err := s.FindResumableWorkflows(ctx, 100)
	if err != nil {
		t.Fatalf("FindResumableWorkflows: %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("want exactly 1 resumable (go-1), got %d: %+v", len(got), got)
	}
	if got[0].InstanceID != "go-1" {
		t.Fatalf("want go-1, got %s", got[0].InstanceID)
	}
}

// TestPGLocal_TryAcquireLock_BasicSemantics covers:
//   - first acquire succeeds
//   - second worker is rejected
//   - re-entrant: same worker re-acquires its own lock
//   - released lock is acquirable by other workers
func TestPGLocal_TryAcquireLock_BasicSemantics(t *testing.T) {
	s := pgLocalSetup(t)
	defer s.Close()
	defer pgLocalCleanup(t, s)
	ctx := context.Background()

	pgLocalInsertInstance(t, s, "lock-1", "wf-go", "go", StatusRunning)

	ok, err := s.TryAcquireLock(ctx, "lock-1", "worker-A", 300)
	if err != nil || !ok {
		t.Fatalf("worker-A acquire: ok=%v err=%v", ok, err)
	}

	ok, err = s.TryAcquireLock(ctx, "lock-1", "worker-B", 300)
	if err != nil {
		t.Fatalf("worker-B acquire err: %v", err)
	}
	if ok {
		t.Fatal("worker-B should NOT acquire while worker-A holds")
	}

	ok, err = s.TryAcquireLock(ctx, "lock-1", "worker-A", 300)
	if err != nil || !ok {
		t.Fatalf("worker-A re-entrant: ok=%v err=%v", ok, err)
	}

	if err := s.ReleaseLock(ctx, "lock-1", "worker-A"); err != nil {
		t.Fatalf("release: %v", err)
	}

	ok, err = s.TryAcquireLock(ctx, "lock-1", "worker-B", 300)
	if err != nil || !ok {
		t.Fatalf("worker-B acquire after release: ok=%v err=%v", ok, err)
	}
}

// TestPGLocal_TryAcquireLock_ExpiredTakeover verifies that an expired
// lock can be taken over by another worker.
func TestPGLocal_TryAcquireLock_ExpiredTakeover(t *testing.T) {
	s := pgLocalSetup(t)
	defer s.Close()
	defer pgLocalCleanup(t, s)
	ctx := context.Background()

	pgLocalInsertInstance(t, s, "lock-2", "wf-go", "go", StatusRunning)

	// worker-A acquires with -1 sec timeout => already expired.
	ok, err := s.TryAcquireLock(ctx, "lock-2", "worker-A", -1)
	if err != nil || !ok {
		t.Fatalf("worker-A acquire (expired): ok=%v err=%v", ok, err)
	}

	// worker-B should succeed because A's lock is already expired.
	ok, err = s.TryAcquireLock(ctx, "lock-2", "worker-B", 300)
	if err != nil || !ok {
		t.Fatalf("worker-B takeover expired lock: ok=%v err=%v", ok, err)
	}

	var lockedBy string
	if err := s.db.QueryRowContext(ctx,
		"SELECT locked_by FROM workflow_instances WHERE instance_id=$1", "lock-2").
		Scan(&lockedBy); err != nil {
		t.Fatalf("read lock owner: %v", err)
	}
	if lockedBy != "worker-B" {
		t.Fatalf("expected lock owner worker-B, got %q", lockedBy)
	}
}

// TestPGLocal_TryAcquireLock_SkipLockedNonBlocking verifies that the
// new SELECT FOR UPDATE SKIP LOCKED implementation does not block
// when another transaction holds a row-level lock on the same row.
//
// Without SKIP LOCKED the second call would wait for tx to commit/rollback;
// with SKIP LOCKED it returns immediately with ok=false.
func TestPGLocal_TryAcquireLock_SkipLockedNonBlocking(t *testing.T) {
	s := pgLocalSetup(t)
	defer s.Close()
	defer pgLocalCleanup(t, s)
	ctx := context.Background()

	pgLocalInsertInstance(t, s, "lock-3", "wf-go", "go", StatusRunning)

	// Open a separate transaction that locks the row with FOR UPDATE.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	var dummy sql.NullString
	if err := tx.QueryRowContext(ctx,
		"SELECT instance_id FROM workflow_instances WHERE instance_id=$1 FOR UPDATE",
		"lock-3").Scan(&dummy); err != nil {
		t.Fatalf("FOR UPDATE in side tx: %v", err)
	}

	// Now in main connection, attempt to acquire the lock.
	// SKIP LOCKED should make it return false fast.
	deadline := time.Now().Add(2 * time.Second)
	callCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	start := time.Now()
	ok, err := s.TryAcquireLock(callCtx, "lock-3", "worker-X", 300)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("TryAcquireLock under FOR UPDATE: err=%v (elapsed=%s)", err, elapsed)
	}
	if ok {
		t.Fatal("TryAcquireLock should NOT succeed while row is locked by side tx")
	}
	if elapsed > 1*time.Second {
		t.Fatalf("SKIP LOCKED should return fast, took %s", elapsed)
	}
}

// TestPGLocal_TryAcquireLock_ConcurrentRace fires N goroutines at the
// same instance; with SKIP LOCKED + UPDATE pattern, exactly one must win.
func TestPGLocal_TryAcquireLock_ConcurrentRace(t *testing.T) {
	s := pgLocalSetup(t)
	defer s.Close()
	defer pgLocalCleanup(t, s)
	ctx := context.Background()

	pgLocalInsertInstance(t, s, "lock-4", "wf-go", "go", StatusRunning)

	const N = 16
	var wg sync.WaitGroup
	wins := make([]bool, N)
	errs := make([]error, N)
	start := make(chan struct{})

	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			ok, err := s.TryAcquireLock(ctx, "lock-4", workerName(i), 300)
			wins[i] = ok
			errs[i] = err
		}(i)
	}
	close(start)
	wg.Wait()

	winners := 0
	for i := 0; i < N; i++ {
		if errs[i] != nil {
			t.Errorf("worker %d err: %v", i, errs[i])
		}
		if wins[i] {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("expected exactly 1 winner among %d workers, got %d", N, winners)
	}

	// Verify locked_by matches a real worker name (sanity check).
	var lockedBy string
	if err := s.db.QueryRowContext(ctx,
		"SELECT locked_by FROM workflow_instances WHERE instance_id=$1", "lock-4").
		Scan(&lockedBy); err != nil {
		t.Fatalf("read lock owner: %v", err)
	}
	if lockedBy == "" {
		t.Fatal("locked_by should be set after race")
	}
}

func workerName(i int) string {
	return "worker-" + string(rune('A'+i%26)) + string(rune('0'+i/26))
}
