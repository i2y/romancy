//go:build postgres

package storage

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// Test PostgreSQL with:
//   docker compose up -d
//   go test -tags=postgres -v ./internal/storage/...
//
// Or set ROMANCY_TEST_POSTGRES_URL environment variable:
//   export ROMANCY_TEST_POSTGRES_URL="postgres://romancy:romancy@localhost:5432/romancy_test?sslmode=disable"

func getTestPostgresURL() string {
	url := os.Getenv("ROMANCY_TEST_POSTGRES_URL")
	if url == "" {
		url = "postgres://romancy:romancy@localhost:5432/romancy_test?sslmode=disable"
	}
	return url
}

func setupPostgresStorage(t *testing.T) *PostgresStorage {
	t.Helper()

	store, err := NewPostgresStorage(getTestPostgresURL())
	if err != nil {
		t.Fatalf("Failed to create PostgreSQL storage: %v", err)
	}

	ctx := context.Background()
	if err := store.Initialize(ctx); err != nil {
		store.Close()
		t.Fatalf("Failed to initialize storage: %v", err)
	}

	// Clean up tables for test isolation
	cleanupTables(t, store)

	return store
}

func cleanupTables(t *testing.T, store *PostgresStorage) {
	t.Helper()
	ctx := context.Background()

	// Order matters due to foreign key constraints
	tables := []string{
		"workflow_compensations",
		"workflow_outbox",
		"workflow_timer_subscriptions",
		"workflow_event_subscriptions",
		"workflow_history",
		"workflow_instances",
	}

	for _, table := range tables {
		if _, err := store.db.ExecContext(ctx, "DELETE FROM "+table); err != nil {
			t.Logf("Warning: failed to clean table %s: %v", table, err)
		}
	}
}

func TestPostgresStorage_Initialize(t *testing.T) {
	store := setupPostgresStorage(t)
	defer store.Close()

	// Initialize should be idempotent
	ctx := context.Background()
	if err := store.Initialize(ctx); err != nil {
		t.Fatalf("Second Initialize failed: %v", err)
	}
}

func TestPostgresStorage_CreateAndGetInstance(t *testing.T) {
	store := setupPostgresStorage(t)
	defer store.Close()

	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	instance := &WorkflowInstance{
		InstanceID:   "test-instance-1",
		WorkflowName: "test_workflow",
		Status:       StatusPending,
		InputData:    []byte(`{"key": "value"}`),
		SourceCode:   "func test() {}",
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	if err := store.CreateInstance(ctx, instance); err != nil {
		t.Fatalf("CreateInstance failed: %v", err)
	}

	// Retrieve the instance
	got, err := store.GetInstance(ctx, "test-instance-1")
	if err != nil {
		t.Fatalf("GetInstance failed: %v", err)
	}

	if got == nil {
		t.Fatal("GetInstance returned nil")
	}

	if got.InstanceID != instance.InstanceID {
		t.Errorf("InstanceID = %q, want %q", got.InstanceID, instance.InstanceID)
	}
	if got.WorkflowName != instance.WorkflowName {
		t.Errorf("WorkflowName = %q, want %q", got.WorkflowName, instance.WorkflowName)
	}
	if got.Status != instance.Status {
		t.Errorf("Status = %q, want %q", got.Status, instance.Status)
	}
	if string(got.InputData) != string(instance.InputData) {
		t.Errorf("InputData = %q, want %q", string(got.InputData), string(instance.InputData))
	}
}

func TestPostgresStorage_UpdateInstanceStatus(t *testing.T) {
	store := setupPostgresStorage(t)
	defer store.Close()

	ctx := context.Background()
	now := time.Now().UTC()

	instance := &WorkflowInstance{
		InstanceID:   "test-status-1",
		WorkflowName: "test_workflow",
		Status:       StatusPending,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	store.CreateInstance(ctx, instance)

	if err := store.UpdateInstanceStatus(ctx, "test-status-1", StatusRunning, ""); err != nil {
		t.Fatalf("UpdateInstanceStatus failed: %v", err)
	}

	got, _ := store.GetInstance(ctx, "test-status-1")
	if got.Status != StatusRunning {
		t.Errorf("Status = %q, want %q", got.Status, StatusRunning)
	}
}

func TestPostgresStorage_LockOperations(t *testing.T) {
	store := setupPostgresStorage(t)
	defer store.Close()

	ctx := context.Background()
	now := time.Now().UTC()

	instance := &WorkflowInstance{
		InstanceID:   "test-lock-1",
		WorkflowName: "test_workflow",
		Status:       StatusPending,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	store.CreateInstance(ctx, instance)

	// Acquire lock
	acquired, err := store.TryAcquireLock(ctx, "test-lock-1", "worker-1", 300)
	if err != nil {
		t.Fatalf("TryAcquireLock failed: %v", err)
	}
	if !acquired {
		t.Error("Expected to acquire lock")
	}

	// Another worker cannot acquire lock
	acquired2, err := store.TryAcquireLock(ctx, "test-lock-1", "worker-2", 300)
	if err != nil {
		t.Fatalf("TryAcquireLock (worker-2) failed: %v", err)
	}
	if acquired2 {
		t.Error("Worker-2 should not acquire lock")
	}

	// Same worker can re-acquire (re-entrant)
	acquired3, err := store.TryAcquireLock(ctx, "test-lock-1", "worker-1", 300)
	if err != nil {
		t.Fatalf("TryAcquireLock (re-entrant) failed: %v", err)
	}
	if !acquired3 {
		t.Error("Same worker should be able to re-acquire lock")
	}

	// Release lock
	if err := store.ReleaseLock(ctx, "test-lock-1", "worker-1"); err != nil {
		t.Fatalf("ReleaseLock failed: %v", err)
	}

	// Now worker-2 can acquire
	acquired4, err := store.TryAcquireLock(ctx, "test-lock-1", "worker-2", 300)
	if err != nil {
		t.Fatalf("TryAcquireLock after release failed: %v", err)
	}
	if !acquired4 {
		t.Error("Worker-2 should acquire lock after release")
	}
}

func TestPostgresStorage_HistoryOperations(t *testing.T) {
	store := setupPostgresStorage(t)
	defer store.Close()

	ctx := context.Background()
	now := time.Now().UTC()

	// Create instance first
	instance := &WorkflowInstance{
		InstanceID:   "test-history-1",
		WorkflowName: "test_workflow",
		Status:       StatusRunning,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	store.CreateInstance(ctx, instance)

	// Append history
	event := &HistoryEvent{
		InstanceID: "test-history-1",
		ActivityID: "activity:1",
		EventType:  "activity_completed",
		EventData:  []byte(`{"result": "ok"}`),
		DataType:   "json",
	}

	if err := store.AppendHistory(ctx, event); err != nil {
		t.Fatalf("AppendHistory failed: %v", err)
	}

	// Get history
	history, hasMore, err := store.GetHistoryPaginated(ctx, "test-history-1", 0, 100)
	if err != nil {
		t.Fatalf("GetHistoryPaginated failed: %v", err)
	}
	if hasMore {
		t.Error("expected hasMore=false for small result set")
	}

	if len(history) != 1 {
		t.Fatalf("len(history) = %d, want 1", len(history))
	}

	if history[0].ActivityID != "activity:1" {
		t.Errorf("ActivityID = %q, want %q", history[0].ActivityID, "activity:1")
	}
}

func TestPostgresStorage_TimerSubscription(t *testing.T) {
	store := setupPostgresStorage(t)
	defer store.Close()

	ctx := context.Background()
	now := time.Now().UTC()

	// Create instance
	instance := &WorkflowInstance{
		InstanceID:   "test-timer-1",
		WorkflowName: "test_workflow",
		Status:       StatusWaitingForTimer,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	store.CreateInstance(ctx, instance)

	// Register timer (already expired)
	sub := &TimerSubscription{
		InstanceID: "test-timer-1",
		TimerID:    "timer-1",
		ExpiresAt:  time.Now().Add(-1 * time.Hour), // Past time
		Step:       1,
	}

	if err := store.RegisterTimerSubscription(ctx, sub); err != nil {
		t.Fatalf("RegisterTimerSubscription failed: %v", err)
	}

	// Find expired timers
	timers, err := store.FindExpiredTimers(ctx)
	if err != nil {
		t.Fatalf("FindExpiredTimers failed: %v", err)
	}

	if len(timers) != 1 {
		t.Fatalf("len(timers) = %d, want 1", len(timers))
	}

	// Remove timer
	if err := store.RemoveTimerSubscription(ctx, "test-timer-1", "timer-1"); err != nil {
		t.Fatalf("RemoveTimerSubscription failed: %v", err)
	}
}

func TestPostgresStorage_OutboxOperations(t *testing.T) {
	store := setupPostgresStorage(t)
	defer store.Close()

	ctx := context.Background()

	// Add outbox event
	event := &OutboxEvent{
		EventID:     "event-1",
		EventType:   "order.created",
		EventSource: "order-service",
		EventData:   []byte(`{"order_id": "123"}`),
		DataType:    "json",
		ContentType: "application/json",
		Status:      "pending",
	}

	if err := store.AddOutboxEvent(ctx, event); err != nil {
		t.Fatalf("AddOutboxEvent failed: %v", err)
	}

	// Get pending events
	events, err := store.GetPendingOutboxEvents(ctx, 10)
	if err != nil {
		t.Fatalf("GetPendingOutboxEvents failed: %v", err)
	}

	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(events))
	}

	// Mark as sent
	if err := store.MarkOutboxEventSent(ctx, "event-1"); err != nil {
		t.Fatalf("MarkOutboxEventSent failed: %v", err)
	}

	// Should not appear in pending
	events2, _ := store.GetPendingOutboxEvents(ctx, 10)
	if len(events2) != 0 {
		t.Errorf("len(events) = %d after sent, want 0", len(events2))
	}
}

func TestPostgresStorage_CompensationOperations(t *testing.T) {
	store := setupPostgresStorage(t)
	defer store.Close()

	ctx := context.Background()
	now := time.Now().UTC()

	// Create instance
	instance := &WorkflowInstance{
		InstanceID:   "test-comp-1",
		WorkflowName: "test_workflow",
		Status:       StatusRunning,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	store.CreateInstance(ctx, instance)

	// Add compensations
	comp1 := &CompensationEntry{
		InstanceID:      "test-comp-1",
		ActivityID:      "activity:1",
		CompensationFn:  "rollback_activity1",
		CompensationArg: []byte(`{"id": "1"}`),
		Order:           1,
		Status:          "pending",
	}
	comp2 := &CompensationEntry{
		InstanceID:      "test-comp-1",
		ActivityID:      "activity:2",
		CompensationFn:  "rollback_activity2",
		CompensationArg: []byte(`{"id": "2"}`),
		Order:           2,
		Status:          "pending",
	}

	store.AddCompensation(ctx, comp1)
	store.AddCompensation(ctx, comp2)

	// Get compensations (should be in LIFO order)
	comps, err := store.GetCompensations(ctx, "test-comp-1")
	if err != nil {
		t.Fatalf("GetCompensations failed: %v", err)
	}

	if len(comps) != 2 {
		t.Fatalf("len(comps) = %d, want 2", len(comps))
	}

	// LIFO: comp2 should come first
	if comps[0].Order != 2 {
		t.Errorf("comps[0].Order = %d, want 2 (LIFO)", comps[0].Order)
	}
	if comps[1].Order != 1 {
		t.Errorf("comps[1].Order = %d, want 1 (LIFO)", comps[1].Order)
	}

	// Mark first as executed
	if err := store.MarkCompensationExecuted(ctx, comps[0].ID); err != nil {
		t.Fatalf("MarkCompensationExecuted failed: %v", err)
	}
}

func TestPostgresStorage_Transaction(t *testing.T) {
	store := setupPostgresStorage(t)
	defer store.Close()

	ctx := context.Background()
	now := time.Now().UTC()

	// Begin transaction
	txCtx, err := store.BeginTransaction(ctx)
	if err != nil {
		t.Fatalf("BeginTransaction failed: %v", err)
	}

	// Verify in transaction
	if !store.InTransaction(txCtx) {
		t.Error("InTransaction should return true")
	}

	// Create instance in transaction
	instance := &WorkflowInstance{
		InstanceID:   "test-tx-1",
		WorkflowName: "test_workflow",
		Status:       StatusPending,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	store.CreateInstance(txCtx, instance)

	// Rollback
	store.RollbackTransaction(txCtx)

	// Instance should not exist
	got, _ := store.GetInstance(ctx, "test-tx-1")
	if got != nil {
		t.Error("Instance should not exist after rollback")
	}
}

func TestPostgresStorage_TransactionCommit(t *testing.T) {
	store := setupPostgresStorage(t)
	defer store.Close()

	ctx := context.Background()
	now := time.Now().UTC()

	// Begin transaction
	txCtx, err := store.BeginTransaction(ctx)
	if err != nil {
		t.Fatalf("BeginTransaction failed: %v", err)
	}

	// Create instance in transaction
	instance := &WorkflowInstance{
		InstanceID:   "test-tx-commit-1",
		WorkflowName: "test_workflow",
		Status:       StatusPending,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	store.CreateInstance(txCtx, instance)

	// Commit
	if err := store.CommitTransaction(txCtx); err != nil {
		t.Fatalf("CommitTransaction failed: %v", err)
	}

	// Instance should exist
	got, _ := store.GetInstance(ctx, "test-tx-commit-1")
	if got == nil {
		t.Error("Instance should exist after commit")
	}
}

func TestPostgresStorage_SelectForUpdateSkipLocked(t *testing.T) {
	store := setupPostgresStorage(t)
	defer store.Close()

	ctx := context.Background()

	// Add multiple outbox events
	for i := 1; i <= 3; i++ {
		event := &OutboxEvent{
			EventID:     fmt.Sprintf("event-%d", i),
			EventType:   "order.created",
			EventSource: "order-service",
			EventData:   []byte(`{}`),
			DataType:    "json",
			ContentType: "application/json",
			Status:      "pending",
		}
		store.AddOutboxEvent(ctx, event)
	}

	// Start transaction 1 and get pending events
	ctx1, _ := store.BeginTransaction(ctx)

	events1, err := store.GetPendingOutboxEvents(ctx1, 2)
	if err != nil {
		t.Fatalf("GetPendingOutboxEvents (tx1) failed: %v", err)
	}
	if len(events1) != 2 {
		t.Errorf("tx1: len(events) = %d, want 2", len(events1))
	}

	// Start transaction 2 and get pending events
	// With FOR UPDATE SKIP LOCKED, it should skip the locked rows
	ctx2, _ := store.BeginTransaction(ctx)

	events2, err := store.GetPendingOutboxEvents(ctx2, 2)
	if err != nil {
		t.Fatalf("GetPendingOutboxEvents (tx2) failed: %v", err)
	}

	// Should only get the remaining unlocked row
	if len(events2) != 1 {
		t.Errorf("tx2: len(events) = %d, want 1 (SKIP LOCKED)", len(events2))
	}

	// Cleanup transactions
	store.RollbackTransaction(ctx1)
	store.RollbackTransaction(ctx2)
}
