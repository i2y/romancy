package romancy

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/i2y/romancy/internal/storage"
)

// genAwareChild fails on generation 1 and completes on later generations —
// deterministic per generation since the instance ID encodes it.
func genAwareChild(t *testing.T) *WorkflowFunc[json.RawMessage, string] {
	return DefineWorkflow("sup-child-genaware", func(ctx *WorkflowContext, input json.RawMessage) (string, error) {
		if strings.HasSuffix(ctx.InstanceID(), ":g1") {
			return "", errors.New("first generation always fails")
		}
		return "ok:" + ctx.InstanceID(), nil
	})
}

func supervisorTestApp(t *testing.T) (*App, func()) {
	t.Helper()
	app, cleanup := createTestApp(t,
		WithWorkflowResumptionInterval(50*time.Millisecond),
		WithRecurCheckInterval(50*time.Millisecond),
		WithTimerCheckInterval(100*time.Millisecond),
		// The supervisor waits PollSeconds between syncs on a timed receive;
		// the timeout checker must run often enough to honor short polls.
		WithEventTimeoutInterval(50*time.Millisecond),
	)
	if err := app.Start(context.Background()); err != nil {
		cleanup()
		t.Fatalf("failed to start app: %v", err)
	}
	return app, cleanup
}

func startSupervisor(t *testing.T, ctx context.Context, app *App, cfg SuperviseConfig, specs []ChildSpec) string {
	t.Helper()
	sup := DefineSupervisor("test-sup-"+strings.ReplaceAll(t.Name(), "/", "-"), cfg, specs)
	RegisterWorkflow(app, sup)
	id, err := StartWorkflow(ctx, app, sup, SupervisorState{})
	require.NoError(t, err)
	return id
}

func TestSupervisor_OneForOne_Transient(t *testing.T) {
	ctx := context.Background()
	app, cleanup := supervisorTestApp(t)
	defer cleanup()

	child := genAwareChild(t)
	RegisterWorkflow(app, child)

	cfg := SuperviseConfig{
		Strategy:      OneForOne,
		MaxRestarts:   10,
		WithinSeconds: 60,
		PollSeconds:   0.2,
	}
	specs := []ChildSpec{
		{Name: "a", Workflow: child.Name(), Input: json.RawMessage(`{}`), Restart: RestartTransient},
		{Name: "b", Workflow: child.Name(), Input: json.RawMessage(`{}`), Restart: RestartTransient},
	}
	supID := startSupervisor(t, ctx, app, cfg, specs)

	// Wait for the supervisor itself to complete: both children eventually
	// reach a terminal state — "a" after one restart, "b"… also fails g1 and
	// restarts once, so both complete at g2.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		inst, err := app.Storage().GetInstance(ctx, supID)
		require.NoError(t, err)
		if inst != nil && inst.Status == storage.StatusCompleted {
			return
		}
		if inst != nil && inst.Status == storage.StatusFailed {
			t.Fatalf("supervisor failed unexpectedly")
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("supervisor did not complete in time")
}

func TestSupervisor_PermanentRestartsOnCompletion(t *testing.T) {
	ctx := context.Background()
	app, cleanup := supervisorTestApp(t)
	defer cleanup()

	child := DefineWorkflow("sup-child-quick", func(ctx *WorkflowContext, input json.RawMessage) (string, error) {
		// Only generation 3 completes the supervisor's purpose; g1/g2 also
		// complete — permanent mode restarts regardless.
		return "done", nil
	})
	RegisterWorkflow(app, child)

	cfg := SuperviseConfig{
		Strategy:      OneForOne,
		MaxRestarts:   2, // intensity caps the permanent restart loop
		WithinSeconds: 60,
		PollSeconds:   0.2,
	}
	specs := []ChildSpec{
		{Name: "a", Workflow: child.Name(), Input: json.RawMessage(`{}`), Restart: RestartPermanent},
	}
	supID := startSupervisor(t, ctx, app, cfg, specs)

	// A permanent child that always completes is restarted forever — the
	// intensity cap is what stops it: the supervisor must FAIL with
	// intensity_exceeded. Observe escalating generations along the way.
	deadline := time.Now().Add(20 * time.Second)
	sawG2 := false
	for time.Now().Before(deadline) {
		inst, err := app.Storage().GetInstance(ctx, supID)
		require.NoError(t, err)
		if inst != nil && inst.Status == storage.StatusFailed {
			assert.True(t, sawG2, "should have restarted at least once before failing")
			return
		}
		g2, err := app.Storage().GetInstance(ctx, supID+":a:g2")
		require.NoError(t, err)
		if g2 != nil {
			sawG2 = true
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("supervisor did not hit the intensity cap in time")
}

func TestSupervisor_OneForAll_RestartsSiblings(t *testing.T) {
	ctx := context.Background()
	app, cleanup := supervisorTestApp(t)
	defer cleanup()

	// slow child: sleeps on a durable timer — stays alive until killed
	slow := DefineWorkflow("sup-child-slow", func(ctx *WorkflowContext, input json.RawMessage) (string, error) {
		if err := Sleep(ctx, 30*time.Second); err != nil {
			return "", err
		}
		return "slow-done", nil
	})
	failing := DefineWorkflow("sup-child-failfast", func(ctx *WorkflowContext, input json.RawMessage) (string, error) {
		if strings.HasSuffix(ctx.InstanceID(), ":g1") {
			return "", errors.New("boom")
		}
		return "", errors.New("always fails — intensity will stop the supervisor")
	})
	RegisterWorkflow(app, slow)
	RegisterWorkflow(app, failing)

	cfg := SuperviseConfig{
		Strategy:      OneForAll,
		MaxRestarts:   4,
		WithinSeconds: 60,
		PollSeconds:   0.2,
	}
	specs := []ChildSpec{
		{Name: "slow", Workflow: slow.Name(), Input: json.RawMessage(`{}`), Restart: RestartTransient},
		{Name: "bad", Workflow: failing.Name(), Input: json.RawMessage(`{}`), Restart: RestartTransient},
	}
	supID := startSupervisor(t, ctx, app, cfg, specs)

	// When "bad" fails at g1, one_for_all must restart BOTH children: the
	// still-alive "slow" child is cancelled (best-effort — cancellation in
	// this kernel is cooperative, so g1's terminal status is a race) and
	// respawned at g2. The essential property is the new generation.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		slowG2, err := app.Storage().GetInstance(ctx, supID+":slow:g2")
		require.NoError(t, err)
		if slowG2 != nil {
			// g1 exists and is no longer the tracked generation
			slowG1, err := app.Storage().GetInstance(ctx, supID+":slow:g1")
			require.NoError(t, err)
			require.NotNil(t, slowG1)
			assert.Contains(t, []storage.WorkflowStatus{
				storage.StatusCancelled, storage.StatusCompleted, storage.StatusRunning, storage.StatusFailed,
			}, slowG1.Status)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("one_for_all did not restart the sibling")
}

func TestSupervisor_TemporaryNeverRestarts(t *testing.T) {
	ctx := context.Background()
	app, cleanup := supervisorTestApp(t)
	defer cleanup()

	child := DefineWorkflow("sup-child-temp", func(ctx *WorkflowContext, input json.RawMessage) (string, error) {
		return "", errors.New("always fails")
	})
	RegisterWorkflow(app, child)

	cfg := SuperviseConfig{
		Strategy:      OneForOne,
		MaxRestarts:   10,
		WithinSeconds: 60,
		PollSeconds:   0.2,
	}
	specs := []ChildSpec{
		{Name: "a", Workflow: child.Name(), Input: json.RawMessage(`{}`), Restart: RestartTemporary},
	}
	supID := startSupervisor(t, ctx, app, cfg, specs)

	// temporary child fails once → terminal, no restart → supervisor completes
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		inst, err := app.Storage().GetInstance(ctx, supID)
		require.NoError(t, err)
		if inst != nil && inst.Status == storage.StatusCompleted {
			// no g2 may exist
			g2, err := app.Storage().GetInstance(ctx, supID+":a:g2")
			require.NoError(t, err)
			assert.Nil(t, g2)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("supervisor did not complete in time")
}

// The supervisor waits PollSeconds between syncs instead of spinning: the
// receive's suspend signal parks the run until a DOWN or the poll timeout.
func TestSupervisor_WaitsPollSeconds(t *testing.T) {
	ctx := context.Background()
	app, cleanup := supervisorTestApp(t)
	defer cleanup()

	slow := DefineWorkflow("sup-child-poll-slow", func(ctx *WorkflowContext, input json.RawMessage) (string, error) {
		if err := Sleep(ctx, 30*time.Second); err != nil {
			return "", err
		}
		return "done", nil
	})
	RegisterWorkflow(app, slow)
	cfg := SuperviseConfig{Strategy: OneForOne, PollSeconds: 0.5}
	specs := []ChildSpec{{Name: "s", Workflow: slow.Name(), Input: json.RawMessage(`{}`), Restart: RestartPermanent}}
	supID := startSupervisor(t, ctx, app, cfg, specs)

	time.Sleep(2500 * time.Millisecond)
	var syncs int
	require.NoError(t, app.Storage().DB().QueryRowContext(ctx, `
		SELECT (SELECT COUNT(*) FROM workflow_history WHERE instance_id = ? AND activity_id LIKE '%/sync:%' AND event_type = 'ActivityCompleted')
		     + (SELECT COUNT(*) FROM workflow_history_archive WHERE instance_id = ? AND activity_id LIKE '%/sync:%' AND event_type = 'ActivityCompleted')
	`, supID, supID).Scan(&syncs))
	// 2.5s at one sync per 0.5s poll: about 5, allowing for checker jitter.
	assert.GreaterOrEqual(t, syncs, 2, "the supervisor never re-synced")
	assert.LessOrEqual(t, syncs, 10, "the supervisor re-synced %d times in 2.5s: it is not waiting PollSeconds", syncs)
}

// Under one_for_all a temporary sibling is stopped with the others but never
// restarted (OTP semantics: temporary children are not restarted by any
// strategy), and its cancellation sticks.
func TestSupervisor_OneForAll_StopsTemporarySibling(t *testing.T) {
	ctx := context.Background()
	app, cleanup := supervisorTestApp(t)
	defer cleanup()

	slow := DefineWorkflow("sup-child-temp-slow", func(ctx *WorkflowContext, input json.RawMessage) (string, error) {
		if err := Sleep(ctx, 30*time.Second); err != nil {
			return "", err
		}
		return "slow-done", nil
	})
	child := genAwareChild(t)
	RegisterWorkflow(app, slow)
	RegisterWorkflow(app, child)
	cfg := SuperviseConfig{Strategy: OneForAll, MaxRestarts: 4, WithinSeconds: 60, PollSeconds: 0.2}
	specs := []ChildSpec{
		{Name: "temp", Workflow: slow.Name(), Input: json.RawMessage(`{}`), Restart: RestartTemporary},
		{Name: "bad", Workflow: child.Name(), Input: json.RawMessage(`{}`), Restart: RestartTransient},
	}
	supID := startSupervisor(t, ctx, app, cfg, specs)

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		inst, err := app.Storage().GetInstance(ctx, supID)
		require.NoError(t, err)
		if inst != nil && inst.Status == storage.StatusCompleted {
			tempG1, err := app.Storage().GetInstance(ctx, supID+":temp:g1")
			require.NoError(t, err)
			require.NotNil(t, tempG1)
			assert.Equal(t, storage.StatusCancelled, tempG1.Status, "the stopped temporary child must stay cancelled")
			tempG2, err := app.Storage().GetInstance(ctx, supID+":temp:g2")
			require.NoError(t, err)
			assert.Nil(t, tempG2, "a temporary child must not be restarted")
			badG2, err := app.Storage().GetInstance(ctx, supID+":bad:g2")
			require.NoError(t, err)
			require.NotNil(t, badG2)
			assert.Equal(t, storage.StatusCompleted, badG2.Status)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	inst, _ := app.Storage().GetInstance(ctx, supID)
	t.Fatalf("supervisor did not complete: %+v", inst)
}

// staleReadStorage answers the first status read of each listed instance
// with "running": the child ended between the supervisor's read and its
// cancel.
type staleReadStorage struct {
	storage.Storage
	mu    sync.Mutex
	stale map[string]bool
}

func (s *staleReadStorage) GetInstance(ctx context.Context, instanceID string) (*storage.WorkflowInstance, error) {
	inst, err := s.Storage.GetInstance(ctx, instanceID)
	s.mu.Lock()
	defer s.mu.Unlock()
	if inst != nil && s.stale[instanceID] {
		delete(s.stale, instanceID)
		running := *inst
		running.Status = storage.StatusRunning
		return &running, err
	}
	return inst, err
}

// A child that ends on its own between the sync's read and the cascade's
// cancel is judged by the status it actually ended in, not as if the cancel
// had happened: a transient child that completed stays done, a permanent one
// is restarted.
func TestSupervisor_CancelRaceUsesActualStatus(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sup.db")
	base, err := storage.NewSQLiteStorage(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = base.Close() }()
	if err := storage.InitializeTestSchema(ctx, base); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for id, status := range map[string]storage.WorkflowStatus{
		"sup": storage.StatusRunning, "sup:a:g1": storage.StatusFailed,
		"sup:b:g1": storage.StatusCompleted, "sup:c:g1": storage.StatusCompleted,
	} {
		if err := base.CreateInstance(ctx, &storage.WorkflowInstance{
			InstanceID: id, WorkflowName: "race_child", Status: status,
			InputData: []byte("null"), StartedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	app := NewApp(WithDatabase(path), WithAutoMigrate(false))
	app.storage = &staleReadStorage{Storage: base, stale: map[string]bool{"sup:b:g1": true, "sup:c:g1": true}}
	wfCtx := NewWorkflowContext(ctx, "sup", "w", "", nil, false)
	wfCtx.SetApp(app)

	spec := func(name string, mode RestartMode) ChildSpec {
		return ChildSpec{Name: name, Workflow: "race_child", Restart: mode}
	}
	out, err := superviseSync(ctx, wfCtx, SuperviseConfig{Strategy: OneForAll}, superviseSyncInput{
		SupervisorID: "sup",
		Children: []ChildState{
			{Spec: spec("a", RestartTransient), InstanceID: "sup:a:g1", Generation: 1},
			{Spec: spec("b", RestartTransient), InstanceID: "sup:b:g1", Generation: 1},
			{Spec: spec("c", RestartPermanent), InstanceID: "sup:c:g1", Generation: 1},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	spawned := func(id string) bool {
		inst, err := base.GetInstance(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return inst != nil
	}
	if !spawned("sup:a:g2") {
		t.Error("the failed child was not restarted")
	}
	if spawned("sup:b:g2") {
		t.Error("a transient child that completed before the cancel landed was restarted")
	}
	if b := out.Children[1]; !b.Terminal || b.FinalStatus != string(storage.StatusCompleted) {
		t.Errorf("transient child state = %+v; want terminal, completed", b)
	}
	if !spawned("sup:c:g2") {
		t.Error("a permanent child was not restarted")
	}
}

// A child of another framework is created for that framework's workers: its
// framework, an object input (edda and shikibu take keyword arguments) and
// no source hash, even when this worker registers a Go workflow of the same
// name.
func TestSupervisor_SpawnsChildForItsFramework(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sup.db")
	base, err := storage.NewSQLiteStorage(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = base.Close() }()
	if err := storage.InitializeTestSchema(ctx, base); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	require.NoError(t, base.CreateInstance(ctx, &storage.WorkflowInstance{
		InstanceID: "sup", WorkflowName: "sup", Status: storage.StatusRunning,
		InputData: []byte("null"), StartedAt: now, UpdatedAt: now,
	}))
	app := NewApp(WithDatabase(path), WithAutoMigrate(false))
	app.storage = base
	RegisterWorkflow(app, DefineWorkflow("shared_name", func(ctx *WorkflowContext, in json.RawMessage) (string, error) {
		return "", nil
	}), WithWorkflowSourceHash("go-hash"))
	wfCtx := NewWorkflowContext(ctx, "sup", "w", "", nil, false)
	wfCtx.SetApp(app)

	_, err = superviseSync(ctx, wfCtx, SuperviseConfig{Strategy: OneForOne}, superviseSyncInput{
		SupervisorID: "sup",
		Children: []ChildState{
			{Spec: ChildSpec{Name: "py", Workflow: "shared_name", Framework: FrameworkPython, Restart: RestartTransient}},
			{Spec: ChildSpec{Name: "rb", Workflow: "shared_name", Framework: FrameworkRuby, Input: json.RawMessage(`{"n":1}`), Restart: RestartTransient}},
			{Spec: ChildSpec{Name: "go", Workflow: "shared_name", Restart: RestartTransient}},
		},
	})
	require.NoError(t, err)

	for id, want := range map[string]struct{ framework, input, hash string }{
		"sup:py:g1": {FrameworkPython, `{}`, ""},
		"sup:rb:g1": {FrameworkRuby, `{"n":1}`, ""},
		"sup:go:g1": {FrameworkGo, `null`, "go-hash"},
	} {
		inst, err := base.GetInstance(ctx, id)
		require.NoError(t, err)
		require.NotNil(t, inst, id)
		assert.Equal(t, want.framework, inst.Framework, id)
		assert.JSONEq(t, want.input, string(inst.InputData), id)
		assert.Equal(t, want.hash, inst.SourceHash, id)
	}
}

// The supervisor leaves a child of another framework to that framework's
// workers and sees how it ends like any other child's.
func TestSupervisor_LeavesChildToItsFramework(t *testing.T) {
	ctx := context.Background()
	app, cleanup := supervisorTestApp(t)
	defer cleanup()

	cfg := SuperviseConfig{Strategy: OneForOne, MaxRestarts: 10, WithinSeconds: 60, PollSeconds: 0.2}
	supID := startSupervisor(t, ctx, app, cfg, []ChildSpec{
		{Name: "py", Workflow: "edda_child", Framework: FrameworkPython, Restart: RestartTransient},
	})
	childID := supID + ":py:g1"
	require.Eventually(t, func() bool {
		inst, err := app.Storage().GetInstance(ctx, childID)
		return err == nil && inst != nil
	}, 10*time.Second, 50*time.Millisecond)

	// Several resumption ticks pass: this worker does not claim it.
	time.Sleep(500 * time.Millisecond)
	inst, err := app.Storage().GetInstance(ctx, childID)
	require.NoError(t, err)
	assert.Equal(t, storage.StatusRunning, inst.Status)
	assert.Empty(t, inst.LockedBy)

	// An edda worker's claim (its framework, its workflow names) takes it
	// and completes it.
	db := app.Storage().DB()
	res, err := db.ExecContext(ctx, `UPDATE workflow_instances SET locked_by = 'edda#1'
		WHERE instance_id = ? AND framework = 'python' AND workflow_name = 'edda_child'
		AND status = 'running' AND locked_by IS NULL`, childID)
	require.NoError(t, err)
	claimed, err := res.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, claimed, "an edda worker's claim does not match the child")
	_, err = db.ExecContext(ctx,
		`UPDATE workflow_instances SET status = 'completed', locked_by = NULL WHERE instance_id = ?`, childID)
	require.NoError(t, err)

	// A transient child that completed is not restarted: the supervisor ends.
	require.Eventually(t, func() bool {
		sup, err := app.Storage().GetInstance(ctx, supID)
		return err == nil && sup != nil && sup.Status == storage.StatusCompleted
	}, 20*time.Second, 100*time.Millisecond)
	g2, err := app.Storage().GetInstance(ctx, supID+":py:g2")
	require.NoError(t, err)
	assert.Nil(t, g2)
}
