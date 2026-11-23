package storage

import (
	"context"
	"testing"
)

// subscribeAndLock prepares an instance subscribed to a channel and holding
// its own lock, as a workflow about to suspend on Receive would.
func subscribeAndLock(t *testing.T, ctx context.Context, s *SQLiteStorage, instanceID, channel, workerID string, mode ChannelMode) {
	t.Helper()
	createClaimInstance(t, ctx, s, instanceID, "wait_wf")
	if err := s.SubscribeToChannel(ctx, instanceID, channel, mode); err != nil {
		t.Fatalf("failed to subscribe: %v", err)
	}
	acquired, err := s.TryAcquireLock(ctx, instanceID, workerID, 300)
	if err != nil || !acquired {
		t.Fatalf("failed to acquire lock: acquired=%v err=%v", acquired, err)
	}
}

// TestRegisterChannelWait_NoPending verifies the normal suspend path: the wait
// is registered, the instance goes to waiting_for_message, and the lock is
// released.
func TestRegisterChannelWait_NoPending(t *testing.T) {
	s, ctx := newClaimTestStorage(t)
	subscribeAndLock(t, ctx, s, "inst-1", "ch", "w1", ChannelModeBroadcast)

	registered, err := s.RegisterChannelReceiveAndReleaseLock(ctx, "inst-1", "ch", "w1", "receive:1", nil)
	if err != nil {
		t.Fatalf("register failed: %v", err)
	}
	if !registered {
		t.Fatal("expected wait to be registered")
	}

	inst, err := s.GetInstance(ctx, "inst-1")
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	if inst.Status != StatusWaitingForMessage {
		t.Fatalf("expected status %q, got %q", StatusWaitingForMessage, inst.Status)
	}
	if inst.LockedBy != "" {
		t.Fatalf("expected lock released, got locked_by=%q", inst.LockedBy)
	}
}

// TestRegisterChannelWait_PendingSkipsWait is the missed-wakeup regression: a
// message committed before registration must prevent the suspend. The instance
// stays 'running' and unlocked so resumption drains the message.
func TestRegisterChannelWait_PendingSkipsWait(t *testing.T) {
	for _, mode := range []ChannelMode{ChannelModeBroadcast, ChannelModeCompeting} {
		t.Run(string(mode), func(t *testing.T) {
			s, ctx := newClaimTestStorage(t)
			subscribeAndLock(t, ctx, s, "inst-1", "ch", "w1", mode)

			if _, err := s.PublishToChannel(ctx, "ch", []byte(`{"x":1}`), nil); err != nil {
				t.Fatalf("publish failed: %v", err)
			}

			registered, err := s.RegisterChannelReceiveAndReleaseLock(ctx, "inst-1", "ch", "w1", "receive:1", nil)
			if err != nil {
				t.Fatalf("register failed: %v", err)
			}
			if registered {
				t.Fatal("expected wait to be skipped due to pending message")
			}

			inst, err := s.GetInstance(ctx, "inst-1")
			if err != nil {
				t.Fatalf("get instance: %v", err)
			}
			if inst.Status != StatusRunning {
				t.Fatalf("expected status %q, got %q", StatusRunning, inst.Status)
			}
			if inst.LockedBy != "" {
				t.Fatalf("expected lock released, got locked_by=%q", inst.LockedBy)
			}

			// The subscription must not be left in the waiting state.
			sub, err := s.GetChannelSubscription(ctx, "inst-1", "ch")
			if err != nil {
				t.Fatalf("get subscription: %v", err)
			}
			if sub.ActivityID != "" {
				t.Fatalf("expected no waiting activity_id, got %q", sub.ActivityID)
			}
		})
	}
}

// TestRequeueChannelWaitsWithPending verifies the backstop sweep: a waiting,
// unlocked instance with a pending message is flipped back to running, while
// genuinely waiting instances are left alone.
func TestRequeueChannelWaitsWithPending(t *testing.T) {
	for _, mode := range []ChannelMode{ChannelModeBroadcast, ChannelModeCompeting} {
		t.Run(string(mode), func(t *testing.T) {
			s, ctx := newClaimTestStorage(t)

			// inst-1: waiting WITH a pending message (stranded, must requeue).
			subscribeAndLock(t, ctx, s, "inst-1", "ch", "w1", mode)
			registered, err := s.RegisterChannelReceiveAndReleaseLock(ctx, "inst-1", "ch", "w1", "receive:1", nil)
			if err != nil || !registered {
				t.Fatalf("register failed: registered=%v err=%v", registered, err)
			}
			// Simulate the race: publish AFTER registration, delivery missed.
			if _, err := s.PublishToChannel(ctx, "ch", []byte(`{"x":1}`), nil); err != nil {
				t.Fatalf("publish failed: %v", err)
			}

			// inst-2: waiting with NO pending message (must stay waiting).
			subscribeAndLock(t, ctx, s, "inst-2", "ch2", "w1", mode)
			registered, err = s.RegisterChannelReceiveAndReleaseLock(ctx, "inst-2", "ch2", "w1", "receive:1", nil)
			if err != nil || !registered {
				t.Fatalf("register failed: registered=%v err=%v", registered, err)
			}

			n, err := s.RequeueChannelWaitsWithPending(ctx)
			if err != nil {
				t.Fatalf("requeue failed: %v", err)
			}
			if n != 1 {
				t.Fatalf("expected 1 requeued, got %d", n)
			}

			inst1, _ := s.GetInstance(ctx, "inst-1")
			if inst1.Status != StatusRunning {
				t.Fatalf("expected inst-1 running, got %q", inst1.Status)
			}
			inst2, _ := s.GetInstance(ctx, "inst-2")
			if inst2.Status != StatusWaitingForMessage {
				t.Fatalf("expected inst-2 still waiting, got %q", inst2.Status)
			}

			// Idempotent: nothing left to requeue.
			n, err = s.RequeueChannelWaitsWithPending(ctx)
			if err != nil {
				t.Fatalf("requeue failed: %v", err)
			}
			if n != 0 {
				t.Fatalf("expected 0 requeued on second pass, got %d", n)
			}
		})
	}
}

// TestRequeueChannelWaitsWithPending_BroadcastCursorPinned verifies that an
// already-delivered broadcast message (cursor advanced past it) does not count
// as pending.
func TestRequeueChannelWaitsWithPending_BroadcastCursorPinned(t *testing.T) {
	s, ctx := newClaimTestStorage(t)
	subscribeAndLock(t, ctx, s, "inst-1", "ch", "w1", ChannelModeBroadcast)

	// Publish BEFORE the wait registration; the registration itself must see
	// this message, so first consume it by advancing the cursor past it.
	msgID, err := s.PublishToChannel(ctx, "ch", []byte(`{"x":1}`), nil)
	if err != nil {
		t.Fatalf("publish failed: %v", err)
	}
	if err := s.UpdateDeliveryCursor(ctx, "inst-1", "ch", msgID); err != nil {
		t.Fatalf("update cursor: %v", err)
	}

	registered, err := s.RegisterChannelReceiveAndReleaseLock(ctx, "inst-1", "ch", "w1", "receive:1", nil)
	if err != nil || !registered {
		t.Fatalf("expected registered wait (message already delivered), got registered=%v err=%v", registered, err)
	}

	n, err := s.RequeueChannelWaitsWithPending(ctx)
	if err != nil {
		t.Fatalf("requeue failed: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 requeued (cursor past message), got %d", n)
	}
}
