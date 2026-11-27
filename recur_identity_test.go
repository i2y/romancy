package romancy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/i2y/romancy/internal/storage"
)

// Recur continues the same instance (SEMANTICS §1): everything bound to its
// ID — name, issued capabilities, the monitors watching it — survives, no
// DOWN is sent for a recur, and the compensations of a finished iteration
// are dropped, so a failure in a later iteration does not undo it.
func TestRecur_KeepsIdentityAndDropsCompensations(t *testing.T) {
	// DOWN is emitted by the leader's cleanup tick: run it often.
	app, cleanup := createTestApp(t,
		WithWorkflowResumptionInterval(50*time.Millisecond),
		WithRecurCheckInterval(50*time.Millisecond),
		WithEventTimeoutInterval(50*time.Millisecond),
		WithLeaderHeartbeatInterval(50*time.Millisecond),
		WithStaleLockInterval(100*time.Millisecond),
	)
	defer cleanup()
	ctx := context.Background()
	require.NoError(t, app.Start(ctx))

	var undone atomic.Int32
	reserve := DefineActivity("recur_ident_reserve",
		func(ctx context.Context, _ string) (string, error) { return "reserved", nil },
		WithCompensation[string, string](func(context.Context, string) error { undone.Add(1); return nil }))

	type iteration struct {
		N int `json:"n"`
	}
	var mu sync.Mutex
	seen := map[string]bool{}
	target := DefineWorkflow("recur_ident_target", func(ctx *WorkflowContext, in iteration) (string, error) {
		mu.Lock()
		seen[ctx.InstanceID()] = true
		mu.Unlock()
		switch in.N {
		case 0:
			if err := RegisterName(ctx, "recur-ident"); err != nil {
				return "", err
			}
			if _, _, err := IssueCapability(ctx, storage.CapabilityKindChannelPub, "recur-ident-ch"); err != nil {
				return "", err
			}
			if _, err := reserve.Execute(ctx, "x"); err != nil {
				return "", err
			}
			if err := Subscribe(ctx, "recur-ident-go", ModeBroadcast); err != nil {
				return "", err
			}
			if _, err := Receive[string](ctx, "recur-ident-go"); err != nil {
				return "", err
			}
		case 2:
			holder, err := Whereis(ctx, "recur-ident")
			if err != nil {
				return "", err
			}
			if holder != ctx.InstanceID() {
				return "", fmt.Errorf("name held by %q after recur", holder)
			}
			return "", errors.New("the last iteration fails")
		}
		_, err := Recur(ctx, iteration{N: in.N + 1})
		return "", err
	})
	watcher := DefineWorkflow("recur_ident_watcher", func(ctx *WorkflowContext, targetID string) (string, error) {
		if err := Subscribe(ctx, "recur-ident-down", ModeDirect); err != nil {
			return "", err
		}
		if err := Monitor(ctx, targetID, "recur-ident-down"); err != nil {
			return "", err
		}
		msg, err := Receive[map[string]any](ctx, "recur-ident-down")
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%v", msg.Data["status"]), nil
	})
	goPub := DefineWorkflow("recur_ident_go", func(ctx *WorkflowContext, _ struct{}) (bool, error) {
		return true, Publish(ctx, "recur-ident-go", "go")
	})
	RegisterWorkflow(app, target)
	RegisterWorkflow(app, watcher)
	RegisterWorkflow(app, goPub)

	targetID, err := StartWorkflow(ctx, app, target, iteration{})
	require.NoError(t, err)
	require.True(t, waitForStatus(t, ctx, app, targetID, storage.StatusWaitingForMessage, 10*time.Second))
	watcherID, err := StartWorkflow(ctx, app, watcher, targetID)
	require.NoError(t, err)
	require.True(t, waitForStatus(t, ctx, app, watcherID, storage.StatusWaitingForMessage, 10*time.Second))
	_, err = StartWorkflow(ctx, app, goPub, struct{}{})
	require.NoError(t, err)

	inst := waitForTerminalInstance(t, app, targetID)
	require.Equal(t, storage.StatusFailed, inst.Status, "the last iteration fails")
	var input iteration
	require.NoError(t, json.Unmarshal(inst.InputData, &input))
	require.Equal(t, 2, input.N, "the instance carries the last iteration's input")
	mu.Lock()
	require.Equal(t, map[string]bool{targetID: true}, seen, "every iteration runs as the same instance")
	mu.Unlock()
	require.Zero(t, undone.Load(), "a failure after a recur undid an earlier iteration")

	sealed, err := app.Storage().HasCapabilityGrants(ctx, storage.CapabilityKindChannelPub, "recur-ident-ch")
	require.NoError(t, err)
	require.True(t, sealed, "the capability issued before the recur is gone")
	archived, err := app.Storage().GetArchivedHistory(ctx, targetID)
	require.NoError(t, err)
	require.NotEmpty(t, archived, "the iterations' history was not archived")

	w := waitForTerminalInstance(t, app, watcherID)
	require.Equal(t, storage.StatusCompleted, w.Status)
	require.Equal(t, `"failed"`, string(w.OutputData), "the first DOWN must be the failure, not a recur")
}

// A supervised child that recurs stays the same child: the supervisor sees
// 'recurred' as alive and does not restart it.
func TestSupervisor_RecurringChildIsNotRestarted(t *testing.T) {
	ctx := context.Background()
	app, cleanup := supervisorTestApp(t)
	defer cleanup()

	type loop struct {
		N int `json:"n"`
	}
	child := DefineWorkflow("sup-child-recurring", func(ctx *WorkflowContext, in loop) (string, error) {
		if in.N < 3 {
			if err := Sleep(ctx, 100*time.Millisecond); err != nil {
				return "", err
			}
			_, err := Recur(ctx, loop{N: in.N + 1})
			return "", err
		}
		return "done", nil
	})
	RegisterWorkflow(app, child)

	cfg := SuperviseConfig{Strategy: OneForOne, MaxRestarts: 5, WithinSeconds: 60, PollSeconds: 0.05}
	specs := []ChildSpec{{Name: "loop", Workflow: child.Name(), Input: json.RawMessage(`{"n":0}`), Restart: RestartTransient}}
	supID := startSupervisor(t, ctx, app, cfg, specs)

	sup := waitForTerminalInstance(t, app, supID)
	require.Equal(t, storage.StatusCompleted, sup.Status)
	result, err := app.Storage().ListInstances(ctx, storage.ListInstancesOptions{WorkflowNameFilter: child.Name(), Limit: 10})
	require.NoError(t, err)
	ids := make([]string, 0, len(result.Instances))
	for _, inst := range result.Instances {
		ids = append(ids, inst.InstanceID)
	}
	require.Equal(t, []string{supID + ":loop:g1"}, ids, "the recurring child was restarted")
	got, err := app.Storage().GetInstance(ctx, supID+":loop:g1")
	require.NoError(t, err)
	require.Equal(t, storage.StatusCompleted, got.Status)
	require.True(t, strings.Contains(string(got.InputData), `"n":3`), "child input = %s", got.InputData)
}

// A recurred instance is alive and can be cancelled.
func TestCancel_RecurredInstance(t *testing.T) {
	// The recur sweep must not restart the instance before the cancel.
	app, cleanup := createTestApp(t, WithRecurCheckInterval(time.Hour))
	defer cleanup()
	ctx := context.Background()
	require.NoError(t, app.Start(ctx))
	const id = "cancel-recurred-1"
	require.NoError(t, app.Storage().CreateInstance(ctx, &storage.WorkflowInstance{
		InstanceID: id, WorkflowName: "cancel_recurred_unregistered", Status: storage.StatusRecurred,
		InputData: []byte(`{}`),
	}))
	require.NoError(t, CancelWorkflow(ctx, app, id, "test"))
	inst, err := app.Storage().GetInstance(ctx, id)
	require.NoError(t, err)
	require.Equal(t, storage.StatusCancelled, inst.Status)
}

// A recur keeps the broadcast delivery cursor (SEMANTICS §5): the channel
// subscribed again in the next iteration continues after the last message
// delivered before the recur, so a message published while the instance
// was between subscriptions is not lost.
func TestRecur_BroadcastContinuesAfterLastDelivered(t *testing.T) {
	app, cleanup := createTestApp(t,
		WithWorkflowResumptionInterval(50*time.Millisecond),
		WithRecurCheckInterval(50*time.Millisecond),
		WithTimerCheckInterval(50*time.Millisecond),
	)
	defer cleanup()
	ctx := context.Background()
	require.NoError(t, app.Start(ctx))

	publisher := DefineWorkflow("recur_bc_pub", func(ctx *WorkflowContext, msg string) (string, error) {
		return msg, Publish(ctx, "recur-bc", msg)
	})
	type iteration struct {
		N int `json:"n"`
	}
	reader := DefineWorkflow("recur_bc_reader", func(ctx *WorkflowContext, in iteration) (string, error) {
		if in.N == 1 {
			// Not subscribed until this timer fires: the test publishes now.
			if err := Sleep(ctx, 300*time.Millisecond); err != nil {
				return "", err
			}
		}
		if err := Subscribe(ctx, "recur-bc", ModeBroadcast); err != nil {
			return "", err
		}
		msg, err := Receive[string](ctx, "recur-bc", WithReceiveTimeout(5*time.Second))
		if err != nil {
			return "", err
		}
		if in.N == 0 {
			_, err := Recur(ctx, iteration{N: 1})
			return "", err
		}
		return "got:" + msg.Data, nil
	})
	RegisterWorkflow(app, publisher)
	RegisterWorkflow(app, reader)

	publish := func(msg string) {
		t.Helper()
		pid, err := StartWorkflow(ctx, app, publisher, msg)
		require.NoError(t, err)
		require.Equal(t, storage.StatusCompleted, waitForTerminalInstance(t, app, pid).Status)
	}
	id, err := StartWorkflow(ctx, app, reader, iteration{})
	require.NoError(t, err)
	require.True(t, waitForStatus(t, ctx, app, id, storage.StatusWaitingForMessage, 5*time.Second))
	publish("a")
	require.True(t, waitForStatus(t, ctx, app, id, storage.StatusWaitingForTimer, 5*time.Second))
	publish("b")
	inst := waitForTerminalInstance(t, app, id)
	require.Equal(t, storage.StatusCompleted, inst.Status, "output %s", inst.OutputData)
	require.Equal(t, `"got:b"`, string(inst.OutputData))
}
