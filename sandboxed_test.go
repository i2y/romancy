package romancy

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/i2y/romancy/internal/storage"
)

func sandboxedTestName(t *testing.T, suffix string) string {
	t.Helper()
	return strings.ReplaceAll(t.Name(), "/", "-") + "-" + suffix
}

func waitForTerminalInstance(t *testing.T, app *App, instanceID string) *storage.WorkflowInstance {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		inst, err := app.Storage().GetInstance(ctx, instanceID)
		require.NoError(t, err)
		if inst != nil && (inst.Status == storage.StatusCompleted || inst.Status == storage.StatusFailed) {
			return inst
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("workflow %s did not reach a terminal state", instanceID)
	return nil
}

func waitForInstanceRow(t *testing.T, app *App, instanceID string) *storage.WorkflowInstance {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		inst, err := app.Storage().GetInstance(ctx, instanceID)
		require.NoError(t, err)
		if inst != nil {
			return inst
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("instance %s was not created", instanceID)
	return nil
}

func workflowStringOutput(t *testing.T, inst *storage.WorkflowInstance) string {
	t.Helper()
	require.NotNil(t, inst.OutputData, "workflow finished without output; status=%s", inst.Status)
	var out string
	require.NoError(t, json.Unmarshal(inst.OutputData, &out))
	return out
}

func TestSandboxed_IssueCapabilityDenied(t *testing.T) {
	app, cleanup := supervisorTestApp(t)
	defer cleanup()

	wf := DefineWorkflow(sandboxedTestName(t, "issue"), func(ctx *WorkflowContext, input json.RawMessage) (string, error) {
		_, _, err := IssueCapability(ctx, storage.CapabilityKindChannelPub, "sandboxed-test-channel")
		if err != nil {
			return "err:" + err.Error(), nil
		}
		return "ok", nil
	})
	RegisterWorkflow(app, wf)

	id, err := StartWorkflow(context.Background(), app, wf, json.RawMessage(`{}`), WithSandboxed(true))
	require.NoError(t, err)
	inst := waitForTerminalInstance(t, app, id)
	require.Equal(t, storage.StatusCompleted, inst.Status)
	assert.True(t, inst.Sandboxed)
	out := workflowStringOutput(t, inst)
	assert.Contains(t, out, ErrSandboxed.Error())

	// Control: an unsandboxed instance of the same workflow may mint tokens.
	id, err = StartWorkflow(context.Background(), app, wf, json.RawMessage(`{}`))
	require.NoError(t, err)
	inst = waitForTerminalInstance(t, app, id)
	require.Equal(t, storage.StatusCompleted, inst.Status)
	assert.False(t, inst.Sandboxed)
	assert.Equal(t, "ok", workflowStringOutput(t, inst))
}

func TestSandboxed_InheritedByWorkflowSpawn(t *testing.T) {
	app, cleanup := supervisorTestApp(t)
	defer cleanup()

	child := DefineWorkflow(sandboxedTestName(t, "child"), func(ctx *WorkflowContext, input json.RawMessage) (string, error) {
		return "child-ok", nil
	})
	RegisterWorkflow(app, child)

	parent := DefineWorkflow(sandboxedTestName(t, "parent"), func(ctx *WorkflowContext, input json.RawMessage) (string, error) {
		childID, err := StartWorkflow(ctx.Context(), ctx.App(), child, json.RawMessage(`{}`))
		if err != nil {
			return "err:" + err.Error(), nil
		}
		return childID, nil
	})
	RegisterWorkflow(app, parent)

	parentID, err := StartWorkflow(context.Background(), app, parent, json.RawMessage(`{}`), WithSandboxed(true))
	require.NoError(t, err)
	parentInst := waitForTerminalInstance(t, app, parentID)
	require.Equal(t, storage.StatusCompleted, parentInst.Status)
	childID := workflowStringOutput(t, parentInst)
	require.NotContains(t, childID, "err:")

	childInst := waitForInstanceRow(t, app, childID)
	assert.True(t, childInst.Sandboxed, "child of a sandboxed parent must inherit the sandbox flag")
}

func TestSandboxed_ExplicitSandboxSpawnRejectedForSandboxedParent(t *testing.T) {
	app, cleanup := supervisorTestApp(t)
	defer cleanup()

	child := DefineWorkflow(sandboxedTestName(t, "explicit-child"), func(ctx *WorkflowContext, input json.RawMessage) (string, error) {
		return "child-ok", nil
	})
	RegisterWorkflow(app, child)

	parent := DefineWorkflow(sandboxedTestName(t, "explicit-parent"), func(ctx *WorkflowContext, input json.RawMessage) (string, error) {
		_, err := StartWorkflow(ctx.Context(), ctx.App(), child, json.RawMessage(`{}`), WithSandboxed(true))
		if err != nil {
			return "err:" + err.Error(), nil
		}
		return "unexpected-ok", nil
	})
	RegisterWorkflow(app, parent)

	parentID, err := StartWorkflow(context.Background(), app, parent, json.RawMessage(`{}`), WithSandboxed(true))
	require.NoError(t, err)
	parentInst := waitForTerminalInstance(t, app, parentID)
	require.Equal(t, storage.StatusCompleted, parentInst.Status)
	out := workflowStringOutput(t, parentInst)
	assert.Contains(t, out, ErrSandboxed.Error())
}

func TestSandboxed_ExplicitSandboxSpawnAllowedForUnsandboxedParent(t *testing.T) {
	app, cleanup := supervisorTestApp(t)
	defer cleanup()

	child := DefineWorkflow(sandboxedTestName(t, "allowed-child"), func(ctx *WorkflowContext, input json.RawMessage) (string, error) {
		return "child-ok", nil
	})
	RegisterWorkflow(app, child)

	parent := DefineWorkflow(sandboxedTestName(t, "allowed-parent"), func(ctx *WorkflowContext, input json.RawMessage) (string, error) {
		childID, err := StartWorkflow(ctx.Context(), ctx.App(), child, json.RawMessage(`{}`), WithSandboxed(true))
		if err != nil {
			return "err:" + err.Error(), nil
		}
		return childID, nil
	})
	RegisterWorkflow(app, parent)

	parentID, err := StartWorkflow(context.Background(), app, parent, json.RawMessage(`{}`))
	require.NoError(t, err)
	parentInst := waitForTerminalInstance(t, app, parentID)
	require.Equal(t, storage.StatusCompleted, parentInst.Status)
	childID := workflowStringOutput(t, parentInst)
	require.NotContains(t, childID, "err:")

	childInst := waitForInstanceRow(t, app, childID)
	assert.True(t, childInst.Sandboxed)
}

func TestSandboxed_InheritedBySupervisedChild(t *testing.T) {
	ctx := context.Background()
	app, cleanup := supervisorTestApp(t)
	defer cleanup()

	child := DefineWorkflow(sandboxedTestName(t, "sup-child"), func(ctx *WorkflowContext, input json.RawMessage) (string, error) {
		return "child-ok", nil
	})
	RegisterWorkflow(app, child)

	cfg := SuperviseConfig{
		Strategy:      OneForOne,
		MaxRestarts:   2,
		WithinSeconds: 60,
		PollSeconds:   0.1,
	}
	specs := []ChildSpec{
		{Name: "a", Workflow: child.Name(), Input: json.RawMessage(`{}`), Restart: RestartTemporary},
	}
	sup := DefineSupervisor(sandboxedTestName(t, "supervisor"), cfg, specs)
	RegisterWorkflow(app, sup)

	supID, err := StartWorkflow(ctx, app, sup, SupervisorState{}, WithSandboxed(true))
	require.NoError(t, err)
	childID := fmt.Sprintf("%s:a:g1", supID)
	childInst := waitForInstanceRow(t, app, childID)
	assert.True(t, childInst.Sandboxed, "supervised child must inherit the supervisor's sandbox flag")
}
