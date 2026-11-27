package romancy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/i2y/romancy/hooks"
	"github.com/i2y/romancy/internal/replay"
	"github.com/i2y/romancy/internal/storage"
	"github.com/i2y/romancy/retry"
)

// newAbortTestStore returns an in-memory store with the schema and one
// running instance.
func newAbortTestStore(t *testing.T, instanceID string) storage.Storage {
	t.Helper()
	store, err := storage.NewSQLiteStorage(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	if err := storage.InitializeTestSchema(ctx, store); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateInstance(ctx, &storage.WorkflowInstance{
		InstanceID:   instanceID,
		WorkflowName: "abort_test",
		Status:       storage.StatusRunning,
	}); err != nil {
		t.Fatal(err)
	}
	return store
}

// flakyStorage fails the next failRegister calls of RegisterActorName the
// way a driver does when the connection drops.
type flakyStorage struct {
	storage.Storage
	failRegister atomic.Int32
}

func (f *flakyStorage) RegisterActorName(ctx context.Context, name, instanceID, framework string) error {
	if f.failRegister.Add(-1) >= 0 {
		return errors.New("driver: bad connection")
	}
	return f.Storage.RegisterActorName(ctx, name, instanceID, framework)
}

func historyTypes(t *testing.T, s storage.Storage, instanceID string) []string {
	t.Helper()
	events, _, err := s.GetHistoryPaginated(context.Background(), instanceID, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, fmt.Sprintf("%s@%s", e.EventType, e.ActivityID))
	}
	return out
}

func errText(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

// A storage error inside a primitive is not the primitive's outcome
// (SEMANTICS §2): it is not recorded, the run ends without compensating or
// failing the instance — even when the workflow swallows the error — and a
// later run executes the primitive again.
func TestRunAbort_StorageErrorIsNotRecorded(t *testing.T) {
	const id = "abort-storage-1"
	store := &flakyStorage{Storage: newAbortTestStore(t, id)}
	store.failRegister.Store(1)

	var compensations, afterRuns atomic.Int32
	reserve := DefineActivity("abort_storage_reserve",
		func(ctx context.Context, _ string) (string, error) { return "reserved", nil })
	after := DefineActivity("abort_storage_after",
		func(ctx context.Context, _ string) (string, error) { afterRuns.Add(1); return "ran", nil })

	engine := replay.NewEngine(store, &hooks.NoOpHooks{}, "w1")
	engine.SetCompensationRunner(func(context.Context, string, string, []byte) error {
		compensations.Add(1)
		return nil
	})
	var registerErr, afterErr error
	runner := func(execCtx *replay.ExecutionContext) (any, error) {
		wfCtx := NewWorkflowContextFromExecution(execCtx)
		if _, err := reserve.Execute(wfCtx, "x"); err != nil {
			return nil, err
		}
		if err := wfCtx.Storage().AddCompensation(wfCtx.Context(), &storage.CompensationEntry{
			InstanceID: id, ActivityID: "abort_storage_reserve:1", ActivityName: "abort_storage_reserve", Args: []byte(`"x"`),
		}); err != nil {
			return nil, err
		}
		registerErr = RegisterName(wfCtx, "abort-storage-name") // swallowed on purpose
		_, afterErr = after.Execute(wfCtx, "x")
		return "done", nil
	}

	ctx := context.Background()
	err := engine.StartWorkflow(ctx, id, "abort_test", nil, runner)
	if !errors.Is(err, ErrRunAborted) {
		t.Fatalf("run = %v; want ErrRunAborted", err)
	}
	if !errors.Is(registerErr, ErrRunAborted) || !errors.Is(afterErr, ErrRunAborted) {
		t.Fatalf("RegisterName = %v, later activity = %v; want both ErrRunAborted", registerErr, afterErr)
	}
	if afterRuns.Load() != 0 || compensations.Load() != 0 {
		t.Fatalf("aborted run ran %d activities and %d compensations; want none", afterRuns.Load(), compensations.Load())
	}
	inst, err := store.GetInstance(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if inst.Status != storage.StatusRunning || inst.LockedBy != "" {
		t.Fatalf("instance after abort = %s locked_by=%q; want running and unlocked", inst.Status, inst.LockedBy)
	}
	if got := historyTypes(t, store, id); len(got) != 1 || got[0] != "ActivityCompleted@abort_storage_reserve:1" {
		t.Fatalf("history after abort = %v; want only the first activity", got)
	}

	// The next run executes the primitive again and finishes.
	if err := engine.ResumeWorkflow(ctx, id, runner); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if registerErr != nil || afterErr != nil || afterRuns.Load() != 1 || compensations.Load() != 0 {
		t.Fatalf("resume: register=%v after=%v runs=%d compensations=%d", registerErr, afterErr, afterRuns.Load(), compensations.Load())
	}
	if inst, _ := store.GetInstance(ctx, id); inst.Status != storage.StatusCompleted {
		t.Fatalf("status after resume = %s; want completed", inst.Status)
	}
	if owner, _ := store.WhereisActor(ctx, "abort-storage-name"); owner != id {
		t.Fatalf("name owner = %q; want %q", owner, id)
	}
}

// A run that learns from a fenced step that it lost its lock must not
// compensate: the new holder owns the instance (SEMANTICS §1).
func TestRunAbort_LockLossNeverCompensates(t *testing.T) {
	const id = "abort-lock-1"
	store := newAbortTestStore(t, id)
	var compensations atomic.Int32
	engine := replay.NewEngine(store, &hooks.NoOpHooks{}, "w1")
	engine.SetCompensationRunner(func(context.Context, string, string, []byte) error {
		compensations.Add(1)
		return nil
	})
	runner := func(execCtx *replay.ExecutionContext) (any, error) {
		wfCtx := NewWorkflowContextFromExecution(execCtx)
		if err := wfCtx.Storage().AddCompensation(wfCtx.Context(), &storage.CompensationEntry{
			InstanceID: id, ActivityID: "undo:1", ActivityName: "abort_lock_undo", Args: []byte(`"x"`),
		}); err != nil {
			return nil, err
		}
		if _, err := wfCtx.Storage().DB().ExecContext(wfCtx.Context(),
			`UPDATE workflow_instances SET locked_by = 'thief#1' WHERE instance_id = ?`, id); err != nil {
			return nil, err
		}
		// The workflow hands the step's error back as its own.
		return nil, RegisterName(wfCtx, "abort-lock-name")
	}
	err := engine.StartWorkflow(context.Background(), id, "abort_test", nil, runner)
	if !errors.Is(err, storage.ErrLockNotHeld) || !errors.Is(err, ErrRunAborted) {
		t.Fatalf("run = %v; want an abort caused by ErrLockNotHeld", err)
	}
	if n := compensations.Load(); n != 0 {
		t.Fatalf("a run that lost its lock ran %d compensations", n)
	}
	inst, err := store.GetInstance(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if inst.Status != storage.StatusRunning {
		t.Fatalf("status = %s; want running (left to the new holder)", inst.Status)
	}
	for _, h := range historyTypes(t, store, id) {
		if h == "WorkflowFailed@" {
			t.Fatalf("a run that lost its lock recorded a failure: %v", historyTypes(t, store, id))
		}
	}
}

// An activity that gives up is history (SEMANTICS §2): its transaction is
// rolled back, the failure is recorded, and a replay returns it — with its
// types — without running the body again.
func TestActivityFailure_RecordedAndReplayed(t *testing.T) {
	const id = "activity-failure-1"
	store := newAbortTestStore(t, id)
	ctx := context.Background()
	if _, err := store.DB().ExecContext(ctx, `CREATE TABLE failure_probe (v TEXT)`); err != nil {
		t.Fatal(err)
	}

	var terminalRuns, flakyRuns, conflictRuns atomic.Int32
	terminal := DefineActivity("failure_terminal", func(ctx context.Context, _ string) (string, error) {
		terminalRuns.Add(1)
		if _, err := GetWorkflowContext(ctx).Session().ExecContext(ctx, `INSERT INTO failure_probe (v) VALUES ('x')`); err != nil {
			return "", err
		}
		return "", NewTerminalError(errors.New("card declined"))
	})
	flaky := DefineActivity("failure_flaky", func(ctx context.Context, _ string) (string, error) {
		flakyRuns.Add(1)
		return "", errors.New("upstream unavailable")
	}, WithRetryPolicy[string, string](retry.Fixed(2, time.Millisecond)))
	conflict := DefineActivity("failure_conflict", func(ctx context.Context, _ string) (string, error) {
		conflictRuns.Add(1)
		return "", fmt.Errorf("settle: %w", ErrTaskConflict)
	}, WithRetryPolicy[string, string](retry.NoRetry()))

	type observed struct {
		msgs                          [3]string
		terminal, exhausted, conflict bool
	}
	var runs []observed
	runner := func(execCtx *replay.ExecutionContext) (any, error) {
		wfCtx := NewWorkflowContextFromExecution(execCtx)
		_, e1 := terminal.Execute(wfCtx, "x")
		_, e2 := flaky.Execute(wfCtx, "x")
		_, e3 := conflict.Execute(wfCtx, "x")
		var te *TerminalError
		var re *RetryExhaustedError
		runs = append(runs, observed{
			msgs:      [3]string{errText(e1), errText(e2), errText(e3)},
			terminal:  errors.As(e1, &te) && te.Err != nil && te.Err.Error() == "card declined",
			exhausted: errors.As(e2, &re) && re.Attempts == 2 && re.ActivityName == "failure_flaky",
			conflict:  errors.Is(e3, ErrTaskConflict),
		})
		return nil, Sleep(wfCtx, time.Hour)
	}

	engine := replay.NewEngine(store, &hooks.NoOpHooks{}, "w1")
	if err := engine.StartWorkflow(ctx, id, "abort_test", nil, runner); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := engine.ResumeWorkflow(ctx, id, runner); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("runs = %d; want 2", len(runs))
	}
	if !runs[0].terminal || !runs[0].exhausted || !runs[0].conflict {
		t.Fatalf("first run errors lost their types: %+v", runs[0])
	}
	if runs[1] != runs[0] {
		t.Fatalf("replay observed %+v; first run observed %+v", runs[1], runs[0])
	}
	if terminalRuns.Load() != 1 || flakyRuns.Load() != 2 || conflictRuns.Load() != 1 {
		t.Fatalf("bodies ran %d/%d/%d times; want 1/2/1 (a replay runs none)",
			terminalRuns.Load(), flakyRuns.Load(), conflictRuns.Load())
	}

	var probes int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM failure_probe`).Scan(&probes); err != nil {
		t.Fatal(err)
	}
	if probes != 0 {
		t.Fatalf("the failed attempt's write survived (%d rows)", probes)
	}
	events, _, err := store.GetHistoryPaginated(ctx, id, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var rec map[string]any
	for _, e := range events {
		if e.ActivityID == "failure_terminal:1" {
			if e.EventType != storage.HistoryActivityFailed {
				t.Fatalf("terminal failure recorded as %s", e.EventType)
			}
			if err := json.Unmarshal(e.EventData, &rec); err != nil {
				t.Fatal(err)
			}
		}
	}
	if rec["activity_name"] != "failure_terminal" || rec["terminal"] != true || rec["error_message"] != runs[0].msgs[0] {
		t.Fatalf("terminal failure record = %v", rec)
	}
}

// Failures recorded before the structured payload (a JSON string) still
// replay as their message.
func TestActivityFailure_LegacyRecordReplays(t *testing.T) {
	const id = "activity-failure-legacy"
	store := newAbortTestStore(t, id)
	ctx := context.Background()
	if err := store.AppendHistory(ctx, &storage.HistoryEvent{
		InstanceID: id, ActivityID: "legacy_act:1", EventType: storage.HistoryActivityFailed,
		EventData: []byte(`"legacy failure"`), DataType: "json",
	}); err != nil {
		t.Fatal(err)
	}
	var runs atomic.Int32
	act := DefineActivity("legacy_act", func(ctx context.Context, _ string) (string, error) {
		runs.Add(1)
		return "ran", nil
	})
	var got error
	engine := replay.NewEngine(store, &hooks.NoOpHooks{}, "w1")
	_ = engine.ResumeWorkflow(ctx, id, func(execCtx *replay.ExecutionContext) (any, error) {
		_, got = act.Execute(NewWorkflowContextFromExecution(execCtx), "x")
		return "done", nil
	})
	if errText(got) != "legacy failure" || runs.Load() != 0 {
		t.Fatalf("legacy record replayed as %v (body runs %d); want the recorded failure", got, runs.Load())
	}
}

// A blocking receive refused on a sealed channel records the refusal: the
// replay returns it even after the channel was unsealed (SEMANTICS §2).
func TestReplay_ReceiveRefusalIsRecorded(t *testing.T) {
	app, cleanup := replayTestApp(t)
	defer cleanup()
	ctx := context.Background()
	grantChannelCap(t, app, storage.CapabilityKindChannelSub, "rr-sealed")
	wf := DefineWorkflow("replay_receive_refusal", func(ctx *WorkflowContext, _ struct{}) (string, error) {
		if err := Subscribe(ctx, "rr-sealed", ModeBroadcast); err != nil {
			return "", err
		}
		_, recvErr := Receive[string](ctx, "rr-sealed")
		if err := Subscribe(ctx, "rr-resume", ModeBroadcast); err != nil {
			return "", err
		}
		if _, err := Receive[string](ctx, "rr-resume"); err != nil {
			return "", err
		}
		return fmt.Sprintf("refused=%v", errors.Is(recvErr, ErrChannelForbidden)), nil
	})
	resume := DefineWorkflow("replay_receive_refusal_resume", func(ctx *WorkflowContext, _ struct{}) (bool, error) {
		return true, Publish(ctx, "rr-resume", "go")
	})
	RegisterWorkflow(app, wf)
	RegisterWorkflow(app, resume)
	id, err := StartWorkflow(ctx, app, wf, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if !waitForStatus(t, ctx, app, id, storage.StatusWaitingForMessage, 10*time.Second) {
		t.Fatal("workflow did not park on its second receive")
	}
	if err := app.Storage().RevokeCapability(ctx, "cap-test-channel_sub-rr-sealed"); err != nil {
		t.Fatal(err)
	}
	if _, err := StartWorkflow(ctx, app, resume, struct{}{}); err != nil {
		t.Fatal(err)
	}
	inst := waitForTerminalInstance(t, app, id)
	if want := `"refused=true"`; inst.Status != storage.StatusCompleted || string(inst.OutputData) != want {
		t.Fatalf("instance = %s %s; want completed %s", inst.Status, inst.OutputData, want)
	}
}

// A compensating instance is winding down; cancelling it would stop its
// compensations half-way, so cancel refuses (SEMANTICS §1).
func TestCancel_CompensatingIsRefused(t *testing.T) {
	app, cleanup := replayTestApp(t)
	defer cleanup()
	ctx := context.Background()
	var undone atomic.Int32
	DefineActivity("cancel_compensating_reserve",
		func(ctx context.Context, _ string) (string, error) { return "ok", nil },
		WithCompensation[string, string](func(ctx context.Context, _ string) error { undone.Add(1); return nil }))
	const id = "cancel-compensating-1"
	s := app.Storage()
	if err := s.CreateInstance(ctx, &storage.WorkflowInstance{
		InstanceID: id, WorkflowName: "cancel_compensating", Status: storage.StatusCompensating,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddCompensation(ctx, &storage.CompensationEntry{
		InstanceID: id, ActivityID: "cancel_compensating_reserve:1",
		ActivityName: "cancel_compensating_reserve", Args: []byte(`"x"`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := CancelWorkflow(ctx, app, id, "test"); !errors.Is(err, ErrWorkflowNotCancellable) {
		t.Fatalf("cancel of a compensating instance = %v; want ErrWorkflowNotCancellable", err)
	}
	if n := undone.Load(); n != 0 {
		t.Fatalf("the refused cancel ran %d compensations", n)
	}
	if inst, _ := s.GetInstance(ctx, id); inst.Status != storage.StatusCompensating {
		t.Fatalf("status = %s; want compensating", inst.Status)
	}
}

// SleepUntil takes its timer ID also when the deadline has passed, so a
// replay that runs after the deadline generates the same IDs as the first
// run did before it.
func TestSleepUntil_ReplayKeepsTimerIDs(t *testing.T) {
	const id = "sleep-until-1"
	store := newAbortTestStore(t, id)
	ctx := context.Background()
	deadline := time.Now().Add(300 * time.Millisecond)
	var second error
	runner := func(execCtx *replay.ExecutionContext) (any, error) {
		wfCtx := NewWorkflowContextFromExecution(execCtx)
		if err := SleepUntil(wfCtx, deadline); err != nil {
			return nil, err
		}
		second = Sleep(wfCtx, time.Hour)
		if second != nil {
			return nil, second
		}
		return "done", nil
	}
	engine := replay.NewEngine(store, &hooks.NoOpHooks{}, "w1")
	if err := engine.StartWorkflow(ctx, id, "abort_test", nil, runner); err != nil {
		t.Fatalf("start: %v", err)
	}
	// The first timer fires (as the timer loop records it), after the deadline.
	time.Sleep(time.Until(deadline) + 50*time.Millisecond)
	if err := store.AppendHistory(ctx, &storage.HistoryEvent{
		InstanceID: id, ActivityID: "sleep:1", EventType: storage.HistoryActivityCompleted,
		EventData: []byte(`{"timer_id":"sleep:1"}`), DataType: "json",
	}); err != nil {
		t.Fatal(err)
	}
	if err := engine.ResumeWorkflow(ctx, id, runner); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if sig := replay.AsSuspendSignal(second); sig == nil || sig.ActivityID != "sleep:2" {
		t.Fatalf("second sleep = %v; want a suspension on timer sleep:2", second)
	}
	if inst, _ := store.GetInstance(ctx, id); inst.Status != storage.StatusWaitingForTimer {
		t.Fatalf("status = %s; want waiting_for_timer", inst.Status)
	}
}

// Another instance's direct channel "L:X" is X's own (SEMANTICS §3):
// subscribing to it or reading it is refused, whatever the seal state, at
// every split point of the name; sending to it stays open.
func TestChannels_DirectChannelsArePrivate(t *testing.T) {
	app, cleanup := replayTestApp(t)
	defer cleanup()
	ctx := context.Background()
	for _, id := range []string{"dp-owner", "team:7"} {
		if err := app.Storage().CreateInstance(ctx, &storage.WorkflowInstance{
			InstanceID: id, WorkflowName: "direct_privacy_unregistered", Status: storage.StatusRunning, InputData: []byte(`{}`),
		}); err != nil {
			t.Fatal(err)
		}
	}
	verdict := func(err error) string {
		switch {
		case err == nil:
			return "ok"
		case errors.Is(err, ErrChannelForbidden):
			return "forbidden"
		default:
			return err.Error()
		}
	}
	wf := DefineWorkflow("direct_privacy", func(ctx *WorkflowContext, _ struct{}) (string, error) {
		sub := Subscribe(ctx, "inbox:dp-owner", ModeBroadcast)
		_, read := ReadChannelSince(ctx, "inbox:dp-owner", "")
		send := SendTo(ctx, "dp-owner", "inbox", "hi")
		colon := Subscribe(ctx, "x:team:7", ModeBroadcast)
		plain := Subscribe(ctx, "x:team", ModeBroadcast)
		own := Subscribe(ctx, "inbox", ModeDirect)
		return fmt.Sprintf("sub=%s read=%s send=%s colon=%s plain=%s own=%s",
			verdict(sub), verdict(read), verdict(send), verdict(colon), verdict(plain), verdict(own)), nil
	})
	RegisterWorkflow(app, wf)
	id, err := StartWorkflow(ctx, app, wf, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	inst := waitForTerminalInstance(t, app, id)
	want := `"sub=forbidden read=forbidden send=ok colon=forbidden plain=ok own=ok"`
	if inst.Status != storage.StatusCompleted || string(inst.OutputData) != want {
		t.Fatalf("instance = %s %s; want completed %s", inst.Status, inst.OutputData, want)
	}
	if n := countChannelMessages(t, app, "inbox:dp-owner"); n != 1 {
		t.Fatalf("messages on the owner's direct channel = %d; want the one sent", n)
	}
}
