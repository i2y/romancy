package romancy

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
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
