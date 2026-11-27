package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func newSQLiteTxTestStorage(t *testing.T) (*SQLiteStorage, context.Context) {
	t.Helper()
	ctx := context.Background()
	s, err := NewSQLiteStorage(filepath.Join(t.TempDir(), "tx.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := InitializeTestSchema(ctx, s); err != nil {
		t.Fatal(err)
	}
	createTestInstance(t, ctx, s, "a", "wf")
	createTestInstance(t, ctx, s, "b", "wf")
	return s, ctx
}

// The kernel's own transactions read before they write (a pending re-check
// before a wait registration, a candidate scan before a requeue). Such a
// transaction must still be able to write after another connection tried to
// commit in between: a deferred SQLite transaction cannot (WAL answers
// SQLITE_BUSY_SNAPSHOT), an immediate one holds the write lock from BEGIN and
// the other writer waits.
func TestSQLite_KernelTransactionSurvivesConcurrentCommit(t *testing.T) {
	s, ctx := newSQLiteTxTestStorage(t)
	txCtx, err := s.beginImmediate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetInstance(txCtx, "a"); err != nil { // read first
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.UpdateInstanceStatus(ctx, "b", StatusWaitingForTimer, "") }()
	select {
	case err := <-done:
		t.Logf("concurrent write committed inside the transaction's window: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := s.UpdateInstanceStatus(txCtx, "a", StatusWaitingForTimer, ""); err != nil {
		_ = s.RollbackTransaction(txCtx)
		t.Fatalf("read-first transaction could not write: %v", err)
	}
	if err := s.CommitTransaction(txCtx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("concurrent write: %v", err)
	}
}

// Activity transactions stay deferred: an activity body runs user code of
// any length, and an open activity transaction that has not written yet must
// not stall every other writer in the meantime.
func TestSQLite_ActivityTransactionDoesNotBlockWriters(t *testing.T) {
	s, ctx := newSQLiteTxTestStorage(t)
	txCtx, err := s.BeginTransaction(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.RollbackTransaction(txCtx) }()
	if _, err := s.GetInstance(txCtx, "a"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.UpdateInstanceStatus(ctx, "b", StatusWaitingForTimer, "") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("concurrent write: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("an open activity transaction that has only read blocks other writers")
	}
}
