package romancy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/i2y/romancy/hooks"
	"github.com/i2y/romancy/internal/replay"
	"github.com/i2y/romancy/internal/storage"
)

func countTimers(t *testing.T, s storage.Storage, instanceID string) int {
	t.Helper()
	var n int
	if err := s.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM workflow_timer_subscriptions WHERE instance_id = ?`, instanceID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A run that lost its lock leaves no timer behind: the timer and the
// waiting status commit together after a lock proof.
func TestTimerSuspend_LostLockRegistersNothing(t *testing.T) {
	const id = "timer-fence-1"
	store := newAbortTestStore(t, id)
	engine := replay.NewEngine(store, &hooks.NoOpHooks{}, "w1")
	err := engine.StartWorkflow(context.Background(), id, "abort_test", nil, func(execCtx *replay.ExecutionContext) (any, error) {
		wfCtx := NewWorkflowContextFromExecution(execCtx)
		if _, err := store.DB().ExecContext(wfCtx.Context(),
			`UPDATE workflow_instances SET locked_by = 'thief#1' WHERE instance_id = ?`, id); err != nil {
			return nil, err
		}
		return nil, Sleep(wfCtx, time.Hour)
	})
	if !errors.Is(err, storage.ErrLockNotHeld) {
		t.Fatalf("suspend after losing the lock = %v; want ErrLockNotHeld", err)
	}
	if n := countTimers(t, store, id); n != 0 {
		t.Fatalf("a run without the lock registered %d timers", n)
	}
	if inst, _ := store.GetInstance(context.Background(), id); inst.Status != storage.StatusRunning {
		t.Fatalf("status = %s; want running (the new holder's)", inst.Status)
	}
}

// A timer whose instance is not waiting for a timer is removed, not fired:
// firing it would record a completion into, and wake, an instance that waits
// for something else.
func TestTimer_NotAwaitedIsRemovedWithoutFiring(t *testing.T) {
	app, cleanup := replayTestApp(t)
	defer cleanup()
	ctx := context.Background()
	wf := DefineWorkflow("timer_not_awaited", func(ctx *WorkflowContext, _ struct{}) (string, error) {
		return "ran", nil
	})
	RegisterWorkflow(app, wf)
	const id = "timer-not-awaited-1"
	s := app.Storage()
	if err := s.CreateInstance(ctx, &storage.WorkflowInstance{
		InstanceID: id, WorkflowName: wf.Name(), Status: storage.StatusWaitingForMessage, InputData: []byte(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterTimerSubscription(ctx, &storage.TimerSubscription{
		InstanceID: id, TimerID: "sleep:1", ActivityID: "sleep:1", ExpiresAt: time.Now().Add(-time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for countTimers(t, s, id) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the timer was never handled")
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond) // a resume, if any, would have run by now
	inst, err := s.GetInstance(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if inst.Status != storage.StatusWaitingForMessage {
		t.Fatalf("status = %s; want waiting_for_message (not woken)", inst.Status)
	}
	if h := historyTypes(t, s, id); len(h) != 0 {
		t.Fatalf("history = %v; want nothing recorded for a timer nobody awaited", h)
	}
}
