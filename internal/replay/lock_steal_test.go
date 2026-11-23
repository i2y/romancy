package replay

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/i2y/romancy/hooks"
	"github.com/i2y/romancy/internal/storage"
)

// stealLock backdates an instance's lease through a second connection and
// lets another engine take it, simulating a worker whose lease expired
// mid-activity (e.g. a task running longer than the lease).
func stealLock(t *testing.T, instanceID string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file::memory:?cache=shared")
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	_, err = db.ExecContext(context.Background(), `
		UPDATE workflow_instances
		SET lock_expires_at = datetime('now', '-1 hour')
		WHERE instance_id = ?
	`, instanceID)
	require.NoError(t, err)
}

// TestEngine_LockStealAbort runs the full take-over scenario end to end:
// engine A is mid-run when its lease is expired and engine B claims the
// instance, finishes it, and A's heartbeat discovers the loss and aborts
// the run with ErrLockNotHeld instead of writing over B's result.
func TestEngine_LockStealAbort(t *testing.T) {
	old := lockRefreshEvery
	lockRefreshEvery = 20 * time.Millisecond
	defer func() { lockRefreshEvery = old }()

	store := setupTestStorage(t)
	ctx := context.Background()

	engineA := NewEngine(store, &hooks.NoOpHooks{}, "worker-A")
	engineB := NewEngine(store, &hooks.NoOpHooks{}, "worker-B")

	instance := &storage.WorkflowInstance{
		InstanceID:   "steal-target",
		WorkflowName: "wf",
		Status:       storage.StatusRunning,
		InputData:    []byte(`{}`),
	}
	require.NoError(t, store.CreateInstance(ctx, instance))

	// Runner A blocks until the heartbeat cancels its context; B runs the
	// same workflow to completion.
	runnerA := func(ec *ExecutionContext) (any, error) {
		<-ec.Context().Done()
		return nil, ec.Context().Err()
	}
	runnerB := func(ec *ExecutionContext) (any, error) {
		require.NoError(t, ec.RecordActivityResult("act:0", "B-result", nil))
		return "B-output", nil
	}

	errCh := make(chan error, 1)
	go func() { errCh <- engineA.StartWorkflow(ctx, instance.InstanceID, "wf", nil, runnerA) }()

	// Wait for A to hold the lock, then backdate the lease and let B take it.
	require.Eventually(t, func() bool {
		inst, err := store.GetInstance(ctx, instance.InstanceID)
		return err == nil && inst != nil && strings.HasPrefix(inst.LockedBy, "worker-A#")
	}, 5*time.Second, 5*time.Millisecond)

	stealLock(t, instance.InstanceID)
	require.NoError(t, engineB.ResumeWorkflow(ctx, instance.InstanceID, runnerB))

	err := <-errCh
	require.True(t, errors.Is(err, storage.ErrLockNotHeld), "want ErrLockNotHeld, got %v", err)

	// B's completion stands: status and output are B's, not overwritten by A.
	got, err := store.GetInstance(ctx, instance.InstanceID)
	require.NoError(t, err)
	assert.Equal(t, storage.StatusCompleted, got.Status)
	assert.JSONEq(t, `"B-output"`, string(got.OutputData))
}

// TestEngine_LockStealFencedWrites verifies the last line of defence: even
// when the stale run ignores the cancellation and tries to write a result
// anyway, the fenced history append answers ErrLockNotHeld and the row B
// completed is untouched.
func TestEngine_LockStealFencedWrites(t *testing.T) {
	store := setupTestStorage(t)
	ctx := context.Background()

	engineA := NewEngine(store, &hooks.NoOpHooks{}, "worker-A")
	engineB := NewEngine(store, &hooks.NoOpHooks{}, "worker-B")

	instance := &storage.WorkflowInstance{
		InstanceID:   "fence-target",
		WorkflowName: "wf",
		Status:       storage.StatusRunning,
		InputData:    []byte(`{}`),
	}
	require.NoError(t, store.CreateInstance(ctx, instance))

	proceed := make(chan struct{})
	var writeErr error
	runnerA := func(ec *ExecutionContext) (any, error) {
		<-proceed
		// The stale worker writes through a fresh context, exactly as a
		// non-cooperative activity would once it unblocks.
		writeErr = ec.RecordActivityResultWithContext(context.Background(), "act:0", "A-result", nil)
		return nil, writeErr
	}
	runnerB := func(ec *ExecutionContext) (any, error) {
		require.NoError(t, ec.RecordActivityResult("act:0", "B-result", nil))
		return "B-output", nil
	}

	errCh := make(chan error, 1)
	go func() { errCh <- engineA.StartWorkflow(ctx, instance.InstanceID, "wf", nil, runnerA) }()

	require.Eventually(t, func() bool {
		inst, err := store.GetInstance(ctx, instance.InstanceID)
		return err == nil && inst != nil && strings.HasPrefix(inst.LockedBy, "worker-A#")
	}, 5*time.Second, 5*time.Millisecond)

	stealLock(t, instance.InstanceID)
	require.NoError(t, engineB.ResumeWorkflow(ctx, instance.InstanceID, runnerB))
	close(proceed)

	err := <-errCh
	require.True(t, errors.Is(err, storage.ErrLockNotHeld), "want ErrLockNotHeld, got %v", err)
	require.True(t, errors.Is(writeErr, storage.ErrLockNotHeld), "want fenced write ErrLockNotHeld, got %v", writeErr)

	got, err := store.GetInstance(ctx, instance.InstanceID)
	require.NoError(t, err)
	assert.Equal(t, storage.StatusCompleted, got.Status)
	assert.JSONEq(t, `"B-output"`, string(got.OutputData))
}
