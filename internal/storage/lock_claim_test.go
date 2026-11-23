package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

// newClaimTestStorage creates a fresh temporary SQLite storage for each test.
func newClaimTestStorage(t *testing.T) (*SQLiteStorage, context.Context) {
	t.Helper()

	tmpFile, err := os.CreateTemp("", "romancy-claim-test-*.db")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	_ = tmpFile.Close()
	t.Cleanup(func() { _ = os.Remove(tmpFile.Name()) })

	s, err := NewSQLiteStorage(tmpFile.Name())
	if err != nil {
		t.Fatalf("failed to create storage: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()
	if err := InitializeTestSchema(ctx, s); err != nil {
		t.Fatalf("failed to initialize schema: %v", err)
	}
	return s, ctx
}

func createClaimInstance(t *testing.T, ctx context.Context, s *SQLiteStorage, instanceID, workflowName string) {
	t.Helper()
	now := time.Now()
	err := s.CreateInstance(ctx, &WorkflowInstance{
		InstanceID:   instanceID,
		WorkflowName: workflowName,
		Status:       StatusRunning,
		InputData:    []byte(`{}`),
		StartedAt:    now,
		UpdatedAt:    now,
	})
	if err != nil {
		t.Fatalf("failed to create instance %s: %v", instanceID, err)
	}
}

// TestClaimConcurrentAcquire asserts the safety property verified in
// spec/InstanceClaim.tla: under concurrent claims, at most one worker wins.
func TestClaimConcurrentAcquire(t *testing.T) {
	s, ctx := newClaimTestStorage(t)
	createClaimInstance(t, ctx, s, "inst-1", "wf")

	const workers = 8
	results := make([]bool, workers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			for attempt := 0; attempt < 5; attempt++ {
				ok, err := s.TryAcquireLock(ctx, "inst-1", workerID(i), 300)
				if err == nil {
					results[i] = ok
					return
				}
				// SQLite serializes writers; a busy error just means retry.
			}
		}(i)
	}
	close(start)
	wg.Wait()

	winners := 0
	for _, ok := range results {
		if ok {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("want exactly 1 lock winner, got %d", winners)
	}
}

func workerID(i int) string {
	return "worker-" + string(rune('a'+i))
}

// TestClaimDuplicateInstance verifies unique-violation mapping to
// ErrInstanceAlreadyExists, usable as an idempotent-start key.
func TestClaimDuplicateInstance(t *testing.T) {
	s, ctx := newClaimTestStorage(t)
	createClaimInstance(t, ctx, s, "inst-1", "wf")

	err := s.CreateInstance(ctx, &WorkflowInstance{
		InstanceID:   "inst-1",
		WorkflowName: "wf",
		Status:       StatusRunning,
		StartedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	})
	if !errors.Is(err, ErrInstanceAlreadyExists) {
		t.Fatalf("want ErrInstanceAlreadyExists, got %v", err)
	}
}

// TestClaimRefreshNotHeld verifies RefreshLock fails for a non-holder and
// for a worker whose lease was stolen after expiry.
func TestClaimRefreshNotHeld(t *testing.T) {
	s, ctx := newClaimTestStorage(t)
	createClaimInstance(t, ctx, s, "inst-1", "wf")

	if ok, err := s.TryAcquireLock(ctx, "inst-1", "w1", 300); err != nil || !ok {
		t.Fatalf("w1 acquire: %v %v", ok, err)
	}

	// Non-holder cannot refresh.
	if err := s.RefreshLock(ctx, "inst-1", "w2", 300); !errors.Is(err, ErrLockNotHeld) {
		t.Fatalf("w2 refresh: want ErrLockNotHeld, got %v", err)
	}
	// Holder can.
	if err := s.RefreshLock(ctx, "inst-1", "w1", 300); err != nil {
		t.Fatalf("w1 refresh: %v", err)
	}

	// Backdate the lease, then w2 takes over.
	expireLock(t, ctx, s, "inst-1")
	if ok, err := s.TryAcquireLock(ctx, "inst-1", "w2", 300); err != nil || !ok {
		t.Fatalf("w2 acquire after expiry: %v %v", ok, err)
	}

	if err := s.RefreshLock(ctx, "inst-1", "w1", 300); !errors.Is(err, ErrLockNotHeld) {
		t.Fatalf("w1 refresh after steal: want ErrLockNotHeld, got %v", err)
	}
}

// expireLock backdates the lock lease so the next acquirer takes over.
func expireLock(t *testing.T, ctx context.Context, s *SQLiteStorage, instanceID string) {
	t.Helper()
	_, err := s.db.ExecContext(ctx, `
		UPDATE workflow_instances
		SET lock_expires_at = datetime('now', '-1 hour')
		WHERE instance_id = ?
	`, instanceID)
	if err != nil {
		t.Fatalf("expire lock: %v", err)
	}
}

// TestClaimFencedWrites verifies a worker that lost its lock can no longer
// write history, status, or output: the fencing checks the row's holder.
func TestClaimFencedWrites(t *testing.T) {
	s, ctx := newClaimTestStorage(t)
	createClaimInstance(t, ctx, s, "inst-1", "wf")

	if ok, err := s.TryAcquireLock(ctx, "inst-1", "w1", 300); err != nil || !ok {
		t.Fatalf("w1 acquire: %v %v", ok, err)
	}

	expireLock(t, ctx, s, "inst-1")
	if ok, err := s.TryAcquireLock(ctx, "inst-1", "w2", 300); err != nil || !ok {
		t.Fatalf("w2 acquire: %v %v", ok, err)
	}

	// The stale worker's writes are all rejected.
	if err := s.AppendHistoryIfLocked(ctx, &HistoryEvent{
		InstanceID: "inst-1",
		ActivityID: "act:0",
		EventType:  HistoryActivityCompleted,
	}, "w1"); !errors.Is(err, ErrLockNotHeld) {
		t.Fatalf("stale AppendHistoryIfLocked: want ErrLockNotHeld, got %v", err)
	}
	if err := s.UpdateInstanceStatusIfLocked(ctx, "inst-1", StatusCompleted, "", "w1"); !errors.Is(err, ErrLockNotHeld) {
		t.Fatalf("stale UpdateInstanceStatusIfLocked: want ErrLockNotHeld, got %v", err)
	}
	if err := s.UpdateInstanceOutputIfLocked(ctx, "inst-1", []byte(`{}`), "w1"); !errors.Is(err, ErrLockNotHeld) {
		t.Fatalf("stale UpdateInstanceOutputIfLocked: want ErrLockNotHeld, got %v", err)
	}

	// The current holder's writes still go through.
	if err := s.AppendHistoryIfLocked(ctx, &HistoryEvent{
		InstanceID: "inst-1",
		ActivityID: "act:0",
		EventType:  HistoryActivityCompleted,
	}, "w2"); err != nil {
		t.Fatalf("holder AppendHistoryIfLocked: %v", err)
	}
	if err := s.UpdateInstanceStatusIfLocked(ctx, "inst-1", StatusCompleted, "", "w2"); err != nil {
		t.Fatalf("holder UpdateInstanceStatusIfLocked: %v", err)
	}
	if err := s.UpdateInstanceOutputIfLocked(ctx, "inst-1", []byte(`{"r":1}`), "w2"); err != nil {
		t.Fatalf("holder UpdateInstanceOutputIfLocked: %v", err)
	}
}

// TestClaimResumableNameFilter verifies FindResumableWorkflows restricts to
// the workflow kinds the calling worker runs.
func TestClaimResumableNameFilter(t *testing.T) {
	s, ctx := newClaimTestStorage(t)
	createClaimInstance(t, ctx, s, "a-1", "wf-a")
	createClaimInstance(t, ctx, s, "b-1", "wf-b")
	createClaimInstance(t, ctx, s, "b-2", "wf-b")

	got, err := s.FindResumableWorkflows(ctx, []string{"wf-a"}, 10)
	if err != nil {
		t.Fatalf("FindResumableWorkflows: %v", err)
	}
	if len(got) != 1 || got[0].InstanceID != "a-1" {
		t.Fatalf("want only a-1, got %+v", got)
	}

	got, err = s.FindResumableWorkflows(ctx, nil, 10)
	if err != nil {
		t.Fatalf("FindResumableWorkflows nil: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want all 3 without filter, got %d", len(got))
	}
}

// TestClaimResumableBatch verifies the fused claim: two workers taking
// batches get disjoint sets, claimed rows carry the lease, and
// locked-but-expired instances are claimed too.
func TestClaimResumableBatch(t *testing.T) {
	s, ctx := newClaimTestStorage(t)
	createClaimInstance(t, ctx, s, "a-1", "wf-a")
	createClaimInstance(t, ctx, s, "a-2", "wf-a")
	createClaimInstance(t, ctx, s, "b-1", "wf-b")

	// w1 claims the two wf-a rows it can run.
	got, err := s.ClaimResumableWorkflows(ctx, "w1", []string{"wf-a"}, 300, 10)
	if err != nil {
		t.Fatalf("w1 claim: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("w1: want 2 claimed, got %+v", got)
	}

	// The rows are actually locked by w1: w2 cannot claim them, but wf-b is
	// still claimable for w2.
	got2, err := s.ClaimResumableWorkflows(ctx, "w2", []string{"wf-a", "wf-b"}, 300, 10)
	if err != nil {
		t.Fatalf("w2 claim: %v", err)
	}
	if len(got2) != 1 || got2[0].InstanceID != "b-1" {
		t.Fatalf("w2: want only b-1, got %+v", got2)
	}

	// A locked-but-expired row is claimed by the sweep as well.
	expireLock(t, ctx, s, "a-1")
	got3, err := s.ClaimResumableWorkflows(ctx, "w3", []string{"wf-a"}, 300, 10)
	if err != nil {
		t.Fatalf("w3 claim: %v", err)
	}
	if len(got3) != 1 || got3[0].InstanceID != "a-1" {
		t.Fatalf("w3: want expired a-1, got %+v", got3)
	}
}

// TestClaimResumableConcurrent verifies concurrent batch claims take
// disjoint sets — the thundering-herd case the fused claim replaces.
func TestClaimResumableConcurrent(t *testing.T) {
	s, ctx := newClaimTestStorage(t)
	const n = 12
	for i := 0; i < n; i++ {
		createClaimInstance(t, ctx, s, fmt.Sprintf("inst-%d", i), "wf")
	}

	const workers = 4
	claimed := make([][]*ResumableWorkflow, workers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			for attempt := 0; attempt < 5; attempt++ {
				got, err := s.ClaimResumableWorkflows(ctx, fmt.Sprintf("w%d", i), []string{"wf"}, 300, 5)
				if err == nil {
					claimed[i] = got
					return
				}
			}
		}(i)
	}
	close(start)
	wg.Wait()

	seen := map[string]string{}
	total := 0
	for i, got := range claimed {
		for _, wf := range got {
			total++
			if prev, dup := seen[wf.InstanceID]; dup {
				t.Fatalf("instance %s claimed by both %s and w%d", wf.InstanceID, prev, i)
			}
			seen[wf.InstanceID] = fmt.Sprintf("w%d", i)
		}
	}
	if total == 0 {
		t.Fatal("nobody claimed anything")
	}
	t.Logf("%d instances claimed disjointly by %d workers", total, workers)
}

// TestClaimOutboxBatch verifies outbox claiming: disjoint batches under
// concurrency, stale 'processing' reclaim, and release back to 'pending'.
func TestClaimOutboxBatch(t *testing.T) {
	s, ctx := newClaimTestStorage(t)

	addEvent := func(id string) {
		t.Helper()
		err := s.AddOutboxEvent(ctx, &OutboxEvent{
			EventID:     id,
			EventType:   "test.event",
			EventSource: "test",
			DataType:    "json",
			EventData:   []byte(`{}`),
			Status:      "pending",
			CreatedAt:   time.Now(),
		})
		if err != nil {
			t.Fatalf("add %s: %v", id, err)
		}
	}
	for _, id := range []string{"e1", "e2", "e3"} {
		addEvent(id)
	}

	// First claim takes the batch.
	got, err := s.ClaimOutboxEvents(ctx, time.Now().Add(-time.Minute), 2)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(got) != 2 || got[0].Status != "processing" {
		t.Fatalf("want 2 processing events, got %+v", got)
	}

	// Fresh claims see none of those rows (they are processing and not stale).
	got2, err := s.ClaimOutboxEvents(ctx, time.Now().Add(-time.Minute), 10)
	if err != nil {
		t.Fatalf("claim2: %v", err)
	}
	if len(got2) != 1 || got2[0].EventID != "e3" {
		t.Fatalf("want only e3 left, got %+v", got2)
	}

	// A claim older than staleBefore is reclaimed.
	stale, err := s.ClaimOutboxEvents(ctx, time.Now().Add(time.Minute), 10)
	if err != nil {
		t.Fatalf("stale claim: %v", err)
	}
	if len(stale) != 3 {
		t.Fatalf("want all 3 reclaimed as stale, got %d", len(stale))
	}

	// Release returns a claimed event to 'pending': it is claimable again
	// even though its (fresh) claim is not stale.
	if err := s.ReleaseOutboxEvent(ctx, "e1"); err != nil {
		t.Fatalf("release: %v", err)
	}
	got3, err := s.ClaimOutboxEvents(ctx, time.Now().Add(-time.Minute), 10)
	if err != nil {
		t.Fatalf("claim3: %v", err)
	}
	if len(got3) != 1 || got3[0].EventID != "e1" {
		t.Fatalf("want only released e1, got %+v", got3)
	}
}
