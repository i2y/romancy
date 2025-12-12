package storage

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestSQLiteStorage(t *testing.T) {
	// Create a temporary database
	tmpFile, err := os.CreateTemp("", "romancy-test-*.db")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	_ = tmpFile.Close()
	defer func() { _ = os.Remove(tmpFile.Name()) }()

	// Create storage
	storage, err := NewSQLiteStorage(tmpFile.Name())
	if err != nil {
		t.Fatalf("failed to create storage: %v", err)
	}
	defer func() { _ = storage.Close() }()

	ctx := context.Background()

	// Initialize the storage (create tables)
	if err := storage.Initialize(ctx); err != nil {
		t.Fatalf("failed to initialize storage: %v", err)
	}

	t.Run("CreateAndGetInstance", func(t *testing.T) {
		instance := &WorkflowInstance{
			InstanceID:   "test-instance-1",
			WorkflowName: "test-workflow",
			Status:       StatusPending,
			InputData:    []byte(`{"name":"test"}`),
			CreatedAt:    time.Now(),
			UpdatedAt:    time.Now(),
		}

		if err := storage.CreateInstance(ctx, instance); err != nil {
			t.Fatalf("failed to create instance: %v", err)
		}

		got, err := storage.GetInstance(ctx, "test-instance-1")
		if err != nil {
			t.Fatalf("failed to get instance: %v", err)
		}

		if got == nil {
			t.Fatal("expected instance, got nil")
		}

		if got.InstanceID != instance.InstanceID {
			t.Errorf("expected instance_id %s, got %s", instance.InstanceID, got.InstanceID)
		}

		if got.WorkflowName != instance.WorkflowName {
			t.Errorf("expected workflow_name %s, got %s", instance.WorkflowName, got.WorkflowName)
		}

		if got.Status != StatusPending {
			t.Errorf("expected status %s, got %s", StatusPending, got.Status)
		}
	})

	t.Run("UpdateInstanceStatus", func(t *testing.T) {
		err := storage.UpdateInstanceStatus(ctx, "test-instance-1", StatusRunning, "")
		if err != nil {
			t.Fatalf("failed to update status: %v", err)
		}

		got, err := storage.GetInstance(ctx, "test-instance-1")
		if err != nil {
			t.Fatalf("failed to get instance: %v", err)
		}

		if got.Status != StatusRunning {
			t.Errorf("expected status %s, got %s", StatusRunning, got.Status)
		}
	})

	t.Run("TryAcquireLock", func(t *testing.T) {
		acquired, err := storage.TryAcquireLock(ctx, "test-instance-1", "worker-1", 300)
		if err != nil {
			t.Fatalf("failed to acquire lock: %v", err)
		}
		if !acquired {
			t.Error("expected to acquire lock")
		}

		// Try to acquire the same lock with a different worker
		acquired2, err := storage.TryAcquireLock(ctx, "test-instance-1", "worker-2", 300)
		if err != nil {
			t.Fatalf("failed to try acquire lock: %v", err)
		}
		if acquired2 {
			t.Error("should not acquire lock held by another worker")
		}

		// Release the lock
		err = storage.ReleaseLock(ctx, "test-instance-1", "worker-1")
		if err != nil {
			t.Fatalf("failed to release lock: %v", err)
		}

		// Now worker-2 should be able to acquire
		acquired3, err := storage.TryAcquireLock(ctx, "test-instance-1", "worker-2", 300)
		if err != nil {
			t.Fatalf("failed to acquire lock: %v", err)
		}
		if !acquired3 {
			t.Error("expected to acquire released lock")
		}
	})

	t.Run("AppendAndGetHistory", func(t *testing.T) {
		historyEvent := &HistoryEvent{
			InstanceID: "test-instance-1",
			ActivityID: "activity:1",
			EventType:  HistoryActivityCompleted,
			EventData:  []byte(`{"result":"success"}`),
			DataType:   "json",
		}
		err := storage.AppendHistory(ctx, historyEvent)
		if err != nil {
			t.Fatalf("failed to append history: %v", err)
		}

		history, hasMore, err := storage.GetHistoryPaginated(ctx, "test-instance-1", 0, 100)
		if err != nil {
			t.Fatalf("failed to get history: %v", err)
		}
		if hasMore {
			t.Error("expected hasMore=false for small result set")
		}

		if len(history) != 1 {
			t.Fatalf("expected 1 history event, got %d", len(history))
		}

		if history[0].ActivityID != "activity:1" {
			t.Errorf("expected activity_id 'activity:1', got '%s'", history[0].ActivityID)
		}

		if history[0].EventType != HistoryActivityCompleted {
			t.Errorf("expected event_type %s, got %s", HistoryActivityCompleted, history[0].EventType)
		}
	})

	t.Run("TimerSubscription", func(t *testing.T) {
		expiresAt := time.Now().Add(-time.Minute) // Already expired
		timer := &TimerSubscription{
			InstanceID: "test-instance-1",
			TimerID:    "timer:1",
			ExpiresAt:  expiresAt,
			Step:       1,
		}
		err := storage.RegisterTimerSubscription(ctx, timer)
		if err != nil {
			t.Fatalf("failed to register timer subscription: %v", err)
		}

		timers, err := storage.FindExpiredTimers(ctx)
		if err != nil {
			t.Fatalf("failed to find expired timers: %v", err)
		}

		if len(timers) != 1 {
			t.Fatalf("expected 1 expired timer, got %d", len(timers))
		}

		if timers[0].TimerID != "timer:1" {
			t.Errorf("expected timer_id 'timer:1', got '%s'", timers[0].TimerID)
		}
	})

	t.Run("Outbox", func(t *testing.T) {
		event := &OutboxEvent{
			EventID:     "event-1",
			EventType:   "order.created",
			EventSource: "/orders",
			EventData:   []byte(`{"order_id":"123"}`),
			DataType:    "json",
			ContentType: "application/json",
			Status:      "pending",
		}
		err := storage.AddOutboxEvent(ctx, event)
		if err != nil {
			t.Fatalf("failed to add outbox event: %v", err)
		}

		events, err := storage.GetPendingOutboxEvents(ctx, 10)
		if err != nil {
			t.Fatalf("failed to get pending outbox events: %v", err)
		}

		if len(events) != 1 {
			t.Fatalf("expected 1 pending event, got %d", len(events))
		}

		if events[0].EventID != "event-1" {
			t.Errorf("expected event_id 'event-1', got '%s'", events[0].EventID)
		}

		err = storage.MarkOutboxEventSent(ctx, "event-1")
		if err != nil {
			t.Fatalf("failed to mark event sent: %v", err)
		}

		events, err = storage.GetPendingOutboxEvents(ctx, 10)
		if err != nil {
			t.Fatalf("failed to get pending outbox events: %v", err)
		}

		if len(events) != 0 {
			t.Errorf("expected 0 pending events after marking sent, got %d", len(events))
		}
	})

	t.Run("GetNonExistentInstance", func(t *testing.T) {
		got, err := storage.GetInstance(ctx, "non-existent")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != nil {
			t.Errorf("expected nil for non-existent instance, got %+v", got)
		}
	})

	t.Run("PostCommitCallbackExecuted", func(t *testing.T) {
		callbackExecuted := false

		// Begin transaction
		txCtx, err := storage.BeginTransaction(ctx)
		if err != nil {
			t.Fatalf("failed to begin transaction: %v", err)
		}

		// Verify we are in transaction
		if !storage.InTransaction(txCtx) {
			t.Fatal("expected to be in transaction")
		}

		// Register callback
		err = storage.RegisterPostCommitCallback(txCtx, func() error {
			callbackExecuted = true
			return nil
		})
		if err != nil {
			t.Fatalf("failed to register callback: %v", err)
		}

		// Callback should not be executed yet
		if callbackExecuted {
			t.Error("callback should not be executed before commit")
		}

		// Commit transaction
		err = storage.CommitTransaction(txCtx)
		if err != nil {
			t.Fatalf("failed to commit transaction: %v", err)
		}

		// Callback should be executed after commit
		if !callbackExecuted {
			t.Error("callback should be executed after commit")
		}
	})

	t.Run("PostCommitCallbackNotExecutedOnRollback", func(t *testing.T) {
		callbackExecuted := false

		// Begin transaction
		txCtx, err := storage.BeginTransaction(ctx)
		if err != nil {
			t.Fatalf("failed to begin transaction: %v", err)
		}

		// Register callback
		err = storage.RegisterPostCommitCallback(txCtx, func() error {
			callbackExecuted = true
			return nil
		})
		if err != nil {
			t.Fatalf("failed to register callback: %v", err)
		}

		// Rollback transaction
		err = storage.RollbackTransaction(txCtx)
		if err != nil {
			t.Fatalf("failed to rollback transaction: %v", err)
		}

		// Callback should NOT be executed after rollback
		if callbackExecuted {
			t.Error("callback should NOT be executed after rollback")
		}
	})

	t.Run("RegisterPostCommitCallbackOutsideTransaction", func(t *testing.T) {
		// Try to register callback outside of transaction
		err := storage.RegisterPostCommitCallback(ctx, func() error {
			return nil
		})
		if err == nil {
			t.Error("expected error when registering callback outside transaction")
		}
	})

	t.Run("MultiplePostCommitCallbacks", func(t *testing.T) {
		executionOrder := []int{}

		// Begin transaction
		txCtx, err := storage.BeginTransaction(ctx)
		if err != nil {
			t.Fatalf("failed to begin transaction: %v", err)
		}

		// Register multiple callbacks
		for i := 1; i <= 3; i++ {
			idx := i // capture
			err = storage.RegisterPostCommitCallback(txCtx, func() error {
				executionOrder = append(executionOrder, idx)
				return nil
			})
			if err != nil {
				t.Fatalf("failed to register callback %d: %v", i, err)
			}
		}

		// Commit transaction
		err = storage.CommitTransaction(txCtx)
		if err != nil {
			t.Fatalf("failed to commit transaction: %v", err)
		}

		// All callbacks should be executed in order
		if len(executionOrder) != 3 {
			t.Errorf("expected 3 callbacks executed, got %d", len(executionOrder))
		}
		for i, v := range executionOrder {
			if v != i+1 {
				t.Errorf("expected callback %d to execute at position %d, got %d", i+1, i, v)
			}
		}
	})

	t.Run("DeliverChannelMessageWithLock_BroadcastNoDuplicate", func(t *testing.T) {
		// This test verifies that broadcast mode delivery updates the cursor
		// to prevent duplicate message delivery when workflow resumes.

		instanceID := "test-broadcast-dedup"
		channelName := "test-broadcast-channel"
		workerID := "worker-broadcast"

		// 1. Create workflow instance
		instance := &WorkflowInstance{
			InstanceID:   instanceID,
			WorkflowName: "test-broadcast-workflow",
			Status:       StatusRunning,
			InputData:    []byte(`{}`),
			CreatedAt:    time.Now(),
			UpdatedAt:    time.Now(),
		}
		if err := storage.CreateInstance(ctx, instance); err != nil {
			t.Fatalf("failed to create instance: %v", err)
		}

		// 2. Subscribe to channel in broadcast mode
		if err := storage.SubscribeToChannel(ctx, instanceID, channelName, ChannelModeBroadcast); err != nil {
			t.Fatalf("failed to subscribe to channel: %v", err)
		}

		// 3. Acquire lock and set waiting state (simulates workflow calling Receive)
		acquired, err := storage.TryAcquireLock(ctx, instanceID, workerID, 300)
		if err != nil {
			t.Fatalf("failed to acquire lock: %v", err)
		}
		if !acquired {
			t.Fatal("expected to acquire lock")
		}

		if err := storage.RegisterChannelReceiveAndReleaseLock(ctx, instanceID, channelName, workerID, "receive:1", nil); err != nil {
			t.Fatalf("failed to register channel receive: %v", err)
		}

		// 4. Publish a message to the channel
		data := []byte(`{"content": "test broadcast message"}`)
		_, err = storage.PublishToChannel(ctx, channelName, data, nil, "")
		if err != nil {
			t.Fatalf("failed to publish message: %v", err)
		}

		// 5. Get pending messages for this instance
		messages, err := storage.GetPendingChannelMessagesForInstance(ctx, instanceID, channelName)
		if err != nil {
			t.Fatalf("failed to get pending messages: %v", err)
		}
		if len(messages) != 1 {
			t.Fatalf("expected 1 pending message, got %d", len(messages))
		}

		// 6. Deliver the message (this is what DeliverChannelMessageWithLock does)
		result, err := storage.DeliverChannelMessageWithLock(ctx, instanceID, channelName, messages[0], workerID, 300)
		if err != nil {
			t.Fatalf("failed to deliver message: %v", err)
		}
		if result == nil {
			t.Fatal("expected delivery result, got nil")
		}

		// 7. Verify cursor was updated
		cursor, err := storage.GetDeliveryCursor(ctx, instanceID, channelName)
		if err != nil {
			t.Fatalf("failed to get delivery cursor: %v", err)
		}
		if cursor != messages[0].ID {
			t.Errorf("expected cursor to be %d, got %d", messages[0].ID, cursor)
		}

		// 8. Verify no duplicate messages are returned (cursor prevents re-delivery)
		messages2, err := storage.GetPendingChannelMessagesForInstance(ctx, instanceID, channelName)
		if err != nil {
			t.Fatalf("failed to get pending messages after delivery: %v", err)
		}
		if len(messages2) != 0 {
			t.Errorf("expected 0 pending messages after delivery (cursor should prevent duplicate), got %d", len(messages2))
		}

		// 9. Verify message appears exactly once in history
		history, _, err := storage.GetHistoryPaginated(ctx, instanceID, 0, 100)
		if err != nil {
			t.Fatalf("failed to get history: %v", err)
		}
		receiveCount := 0
		for _, h := range history {
			if strings.HasPrefix(h.ActivityID, "receive:") {
				receiveCount++
			}
		}
		if receiveCount != 1 {
			t.Errorf("expected exactly 1 receive event in history, got %d", receiveCount)
		}
	})
}
