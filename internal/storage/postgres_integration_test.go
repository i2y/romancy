//go:build integration

package storage

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

func setupPostgresContainer(t *testing.T) (*PostgresStorage, func()) {
	t.Helper()
	ctx := context.Background()

	container, err := postgres.Run(ctx,
		"postgres:15-alpine",
		postgres.WithDatabase("romancy_test"),
		postgres.WithUsername("romancy"),
		postgres.WithPassword("romancy"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(5*time.Minute)),
	)
	if err != nil {
		t.Fatalf("failed to start PostgreSQL container: %v", err)
	}

	connStr, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		testcontainers.CleanupContainer(t, container)
		t.Fatalf("failed to get connection string: %v", err)
	}

	store, err := NewPostgresStorage(connStr)
	if err != nil {
		testcontainers.CleanupContainer(t, container)
		t.Fatalf("failed to create PostgreSQL storage: %v", err)
	}

	// Use embedded test schema instead of Initialize() (which is now no-op)
	if err := InitializeTestSchemaPostgres(ctx, store); err != nil {
		store.Close()
		testcontainers.CleanupContainer(t, container)
		t.Fatalf("failed to initialize test schema: %v", err)
	}

	cleanup := func() {
		store.Close()
		testcontainers.CleanupContainer(t, container)
	}

	return store, cleanup
}

func cleanupPostgresTables(t *testing.T, store *PostgresStorage) {
	t.Helper()
	ctx := context.Background()

	tables := []string{
		"channel_message_claims",
		"channel_delivery_cursors",
		"channel_subscriptions",
		"channel_messages",
		"workflow_group_memberships",
		"outbox_events",
		"workflow_compensations",
		"workflow_timer_subscriptions",
		"workflow_history_archive",
		"workflow_history",
		"workflow_instances",
		"system_locks",
	}

	for _, table := range tables {
		if _, err := store.db.ExecContext(ctx, "DELETE FROM "+table); err != nil {
			t.Logf("Warning: failed to clean table %s: %v", table, err)
		}
	}
}

func TestPostgresIntegration_Initialize(t *testing.T) {
	store, cleanup := setupPostgresContainer(t)
	defer cleanup()

	// InitializeTestSchemaPostgres should be idempotent
	ctx := context.Background()
	if err := InitializeTestSchemaPostgres(ctx, store); err != nil {
		t.Fatalf("Second InitializeTestSchemaPostgres failed: %v", err)
	}
}

func TestPostgresIntegration_CreateAndGetInstance(t *testing.T) {
	store, cleanup := setupPostgresContainer(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	instance := &WorkflowInstance{
		InstanceID:   "test-instance-1",
		WorkflowName: "test_workflow",
		Status:       StatusRunning,
		InputData:    []byte(`{"key": "value"}`),
		StartedAt:    now,
		UpdatedAt:    now,
	}

	if err := store.CreateInstance(ctx, instance); err != nil {
		t.Fatalf("CreateInstance failed: %v", err)
	}

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

func TestPostgresIntegration_UpdateInstanceStatus(t *testing.T) {
	store, cleanup := setupPostgresContainer(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()

	instance := &WorkflowInstance{
		InstanceID:   "test-status-1",
		WorkflowName: "test_workflow",
		Status:       StatusRunning,
		StartedAt:    now,
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

func TestPostgresIntegration_LockOperations(t *testing.T) {
	store, cleanup := setupPostgresContainer(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()

	instance := &WorkflowInstance{
		InstanceID:   "test-lock-1",
		WorkflowName: "test_workflow",
		Status:       StatusRunning,
		StartedAt:    now,
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

func TestPostgresIntegration_HistoryOperations(t *testing.T) {
	store, cleanup := setupPostgresContainer(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()

	instance := &WorkflowInstance{
		InstanceID:   "test-history-1",
		WorkflowName: "test_workflow",
		Status:       StatusRunning,
		StartedAt:    now,
		UpdatedAt:    now,
	}
	store.CreateInstance(ctx, instance)

	event := &HistoryEvent{
		InstanceID: "test-history-1",
		ActivityID: "activity:1",
		EventType:  HistoryActivityCompleted,
		EventData:  []byte(`{"result": "ok"}`),
		DataType:   "json",
	}

	if err := store.AppendHistory(ctx, event); err != nil {
		t.Fatalf("AppendHistory failed: %v", err)
	}

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

func TestPostgresIntegration_TimerSubscription(t *testing.T) {
	store, cleanup := setupPostgresContainer(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()

	instance := &WorkflowInstance{
		InstanceID:   "test-timer-1",
		WorkflowName: "test_workflow",
		Status:       StatusWaitingForTimer,
		StartedAt:    now,
		UpdatedAt:    now,
	}
	store.CreateInstance(ctx, instance)

	sub := &TimerSubscription{
		InstanceID: "test-timer-1",
		TimerID:    "timer-1",
		ExpiresAt:  time.Now().Add(-1 * time.Hour), // Past time
		ActivityID: "activity:1",
	}

	if err := store.RegisterTimerSubscription(ctx, sub); err != nil {
		t.Fatalf("RegisterTimerSubscription failed: %v", err)
	}

	timers, err := store.FindExpiredTimers(ctx, 100)
	if err != nil {
		t.Fatalf("FindExpiredTimers failed: %v", err)
	}

	if len(timers) != 1 {
		t.Fatalf("len(timers) = %d, want 1", len(timers))
	}

	if err := store.RemoveTimerSubscription(ctx, "test-timer-1", "timer-1"); err != nil {
		t.Fatalf("RemoveTimerSubscription failed: %v", err)
	}
}

func TestPostgresIntegration_OutboxOperations(t *testing.T) {
	store, cleanup := setupPostgresContainer(t)
	defer cleanup()

	ctx := context.Background()

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

	events, err := store.GetPendingOutboxEvents(ctx, 10)
	if err != nil {
		t.Fatalf("GetPendingOutboxEvents failed: %v", err)
	}

	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(events))
	}

	if err := store.MarkOutboxEventSent(ctx, "event-1"); err != nil {
		t.Fatalf("MarkOutboxEventSent failed: %v", err)
	}

	events2, _ := store.GetPendingOutboxEvents(ctx, 10)
	if len(events2) != 0 {
		t.Errorf("len(events) = %d after sent, want 0", len(events2))
	}
}

func TestPostgresIntegration_CompensationOperations(t *testing.T) {
	store, cleanup := setupPostgresContainer(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()

	instance := &WorkflowInstance{
		InstanceID:   "test-comp-1",
		WorkflowName: "test_workflow",
		Status:       StatusRunning,
		StartedAt:    now,
		UpdatedAt:    now,
	}
	store.CreateInstance(ctx, instance)

	comp1 := &CompensationEntry{
		InstanceID:   "test-comp-1",
		ActivityID:   "activity:1",
		ActivityName: "rollback_activity1",
		Args:         []byte(`{"id": "1"}`),
	}
	// Small delay to ensure ordering by created_at
	time.Sleep(10 * time.Millisecond)
	comp2 := &CompensationEntry{
		InstanceID:   "test-comp-1",
		ActivityID:   "activity:2",
		ActivityName: "rollback_activity2",
		Args:         []byte(`{"id": "2"}`),
	}

	store.AddCompensation(ctx, comp1)
	store.AddCompensation(ctx, comp2)

	comps, err := store.GetCompensations(ctx, "test-comp-1")
	if err != nil {
		t.Fatalf("GetCompensations failed: %v", err)
	}

	if len(comps) != 2 {
		t.Fatalf("len(comps) = %d, want 2", len(comps))
	}

	// LIFO: comp2 should come first (most recently added)
	if comps[0].ActivityID != "activity:2" {
		t.Errorf("comps[0].ActivityID = %s, want activity:2 (LIFO)", comps[0].ActivityID)
	}
	if comps[1].ActivityID != "activity:1" {
		t.Errorf("comps[1].ActivityID = %s, want activity:1 (LIFO)", comps[1].ActivityID)
	}
}

func TestPostgresIntegration_Transaction(t *testing.T) {
	store, cleanup := setupPostgresContainer(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()

	// Begin transaction
	txCtx, err := store.BeginTransaction(ctx)
	if err != nil {
		t.Fatalf("BeginTransaction failed: %v", err)
	}

	if !store.InTransaction(txCtx) {
		t.Error("InTransaction should return true")
	}

	instance := &WorkflowInstance{
		InstanceID:   "test-tx-1",
		WorkflowName: "test_workflow",
		Status:       StatusRunning,
		StartedAt:    now,
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

func TestPostgresIntegration_TransactionCommit(t *testing.T) {
	store, cleanup := setupPostgresContainer(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()

	txCtx, err := store.BeginTransaction(ctx)
	if err != nil {
		t.Fatalf("BeginTransaction failed: %v", err)
	}

	instance := &WorkflowInstance{
		InstanceID:   "test-tx-commit-1",
		WorkflowName: "test_workflow",
		Status:       StatusRunning,
		StartedAt:    now,
		UpdatedAt:    now,
	}
	store.CreateInstance(txCtx, instance)

	if err := store.CommitTransaction(txCtx); err != nil {
		t.Fatalf("CommitTransaction failed: %v", err)
	}

	got, _ := store.GetInstance(ctx, "test-tx-commit-1")
	if got == nil {
		t.Error("Instance should exist after commit")
	}
}

func TestPostgresIntegration_SelectForUpdateSkipLocked(t *testing.T) {
	store, cleanup := setupPostgresContainer(t)
	defer cleanup()

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

func TestPostgresIntegration_ChannelOperations(t *testing.T) {
	store, cleanup := setupPostgresContainer(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()

	// Create instance
	instance := &WorkflowInstance{
		InstanceID:   "test-channel-1",
		WorkflowName: "test_workflow",
		Status:       StatusRunning,
		StartedAt:    now,
		UpdatedAt:    now,
	}
	store.CreateInstance(ctx, instance)

	// Subscribe to channel
	if err := store.SubscribeToChannel(ctx, "test-channel-1", "test-channel", ChannelModeBroadcast); err != nil {
		t.Fatalf("SubscribeToChannel failed: %v", err)
	}

	// Get subscription
	sub, err := store.GetChannelSubscription(ctx, "test-channel-1", "test-channel")
	if err != nil {
		t.Fatalf("GetChannelSubscription failed: %v", err)
	}
	if sub == nil {
		t.Fatal("Subscription should exist")
	}
	if sub.Mode != ChannelModeBroadcast {
		t.Errorf("Mode = %q, want %q", sub.Mode, ChannelModeBroadcast)
	}

	// Publish message
	messageID, err := store.PublishToChannel(ctx, "test-channel", []byte(`{"msg": "hello"}`), nil)
	if err != nil {
		t.Fatalf("PublishToChannel failed: %v", err)
	}
	if messageID <= 0 {
		t.Error("Expected positive message ID")
	}

	// Get pending messages
	messages, err := store.GetPendingChannelMessages(ctx, "test-channel", 0, 10)
	if err != nil {
		t.Fatalf("GetPendingChannelMessages failed: %v", err)
	}
	if len(messages) != 1 {
		t.Errorf("len(messages) = %d, want 1", len(messages))
	}

	// Unsubscribe
	if err := store.UnsubscribeFromChannel(ctx, "test-channel-1", "test-channel"); err != nil {
		t.Fatalf("UnsubscribeFromChannel failed: %v", err)
	}
}

func TestPostgresIntegration_GroupOperations(t *testing.T) {
	store, cleanup := setupPostgresContainer(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()

	// Create instances
	for i := 1; i <= 3; i++ {
		instance := &WorkflowInstance{
			InstanceID:   fmt.Sprintf("test-group-%d", i),
			WorkflowName: "test_workflow",
			Status:       StatusRunning,
			StartedAt:    now,
			UpdatedAt:    now,
		}
		store.CreateInstance(ctx, instance)
	}

	// Join group
	store.JoinGroup(ctx, "test-group-1", "workers")
	store.JoinGroup(ctx, "test-group-2", "workers")
	store.JoinGroup(ctx, "test-group-3", "workers")

	// Get members
	members, err := store.GetGroupMembers(ctx, "workers")
	if err != nil {
		t.Fatalf("GetGroupMembers failed: %v", err)
	}
	if len(members) != 3 {
		t.Errorf("len(members) = %d, want 3", len(members))
	}

	// Leave group
	if err := store.LeaveGroup(ctx, "test-group-2", "workers"); err != nil {
		t.Fatalf("LeaveGroup failed: %v", err)
	}

	members2, _ := store.GetGroupMembers(ctx, "workers")
	if len(members2) != 2 {
		t.Errorf("len(members) = %d after leave, want 2", len(members2))
	}

	// Leave all groups
	if err := store.LeaveAllGroups(ctx, "test-group-1"); err != nil {
		t.Fatalf("LeaveAllGroups failed: %v", err)
	}

	members3, _ := store.GetGroupMembers(ctx, "workers")
	if len(members3) != 1 {
		t.Errorf("len(members) = %d after leave all, want 1", len(members3))
	}
}

func TestPostgresIntegration_ListInstances(t *testing.T) {
	store, cleanup := setupPostgresContainer(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC()

	// Create multiple instances
	for i := 1; i <= 5; i++ {
		instance := &WorkflowInstance{
			InstanceID:   fmt.Sprintf("list-test-%d", i),
			WorkflowName: "test_workflow",
			Status:       StatusRunning,
			StartedAt:    now.Add(time.Duration(i) * time.Second),
			UpdatedAt:    now.Add(time.Duration(i) * time.Second),
		}
		store.CreateInstance(ctx, instance)
	}

	// List with limit
	result, err := store.ListInstances(ctx, ListInstancesOptions{Limit: 3})
	if err != nil {
		t.Fatalf("ListInstances failed: %v", err)
	}

	if len(result.Instances) != 3 {
		t.Errorf("len(instances) = %d, want 3", len(result.Instances))
	}

	if result.NextPageToken == "" {
		t.Error("Expected next page token")
	}

	// Get next page
	result2, err := store.ListInstances(ctx, ListInstancesOptions{
		Limit:     3,
		PageToken: result.NextPageToken,
	})
	if err != nil {
		t.Fatalf("ListInstances (page 2) failed: %v", err)
	}

	if len(result2.Instances) != 2 {
		t.Errorf("len(instances) page 2 = %d, want 2", len(result2.Instances))
	}
}
