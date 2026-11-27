package romancy

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/i2y/romancy/hooks"
)

// lifecycleHooks records the workflow lifecycle hook calls.
type lifecycleHooks struct {
	hooks.NoOpHooks

	mu         sync.Mutex
	starts     []hooks.WorkflowStartInfo
	completes  []hooks.WorkflowCompleteInfo
	failures   []hooks.WorkflowFailedInfo
	suspends   []hooks.WorkflowSuspendedInfo
	activities []hooks.ActivityStartInfo
}

func (h *lifecycleHooks) OnWorkflowStart(_ context.Context, info hooks.WorkflowStartInfo) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.starts = append(h.starts, info)
}

func (h *lifecycleHooks) OnWorkflowComplete(_ context.Context, info hooks.WorkflowCompleteInfo) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.completes = append(h.completes, info)
}

func (h *lifecycleHooks) OnWorkflowFailed(_ context.Context, info hooks.WorkflowFailedInfo) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.failures = append(h.failures, info)
}

func (h *lifecycleHooks) OnWorkflowSuspended(_ context.Context, info hooks.WorkflowSuspendedInfo) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.suspends = append(h.suspends, info)
}

func (h *lifecycleHooks) OnActivityStart(_ context.Context, info hooks.ActivityStartInfo) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.activities = append(h.activities, info)
}

// waitForEnd waits until a run reported its outcome.
func (h *lifecycleHooks) waitForEnd(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		ended := len(h.completes)+len(h.failures) > 0
		h.mu.Unlock()
		if ended {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the workflow did not end")
}

type hookOrder struct {
	OrderID string `json:"order_id"`
}

// A workflow that runs once calls OnWorkflowStart once, with its input.
func TestHooks_WorkflowStartOncePerRun(t *testing.T) {
	h := &lifecycleHooks{}
	// The poll for resumable instances runs as usual: the new instance holds
	// its first run's lock, so no poll can begin a run without InputData.
	app, cleanup := createTestApp(t, WithHooks(h), WithWorkflowResumptionInterval(time.Millisecond))
	defer cleanup()

	wf := DefineWorkflow("hooked_order", func(ctx *WorkflowContext, order hookOrder) (string, error) {
		return "done:" + order.OrderID, nil
	})
	RegisterWorkflow(app, wf)

	ctx := context.Background()
	if err := app.Start(ctx); err != nil {
		t.Fatalf("start app: %v", err)
	}
	defer func() { _ = app.Shutdown(ctx) }()

	instanceID, err := StartWorkflow(ctx, app, wf, hookOrder{OrderID: "ORD-1"})
	if err != nil {
		t.Fatalf("start workflow: %v", err)
	}
	h.waitForEnd(t)

	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.starts) != 1 {
		t.Fatalf("OnWorkflowStart called %d times, want 1", len(h.starts))
	}
	start := h.starts[0]
	if start.InstanceID != instanceID || start.WorkflowName != "hooked_order" {
		t.Errorf("OnWorkflowStart got %s/%s, want %s/hooked_order", start.InstanceID, start.WorkflowName, instanceID)
	}
	if input, ok := start.InputData.(hookOrder); !ok || input.OrderID != "ORD-1" {
		t.Errorf("OnWorkflowStart InputData = %#v, want the workflow input", start.InputData)
	}
	if len(h.completes) != 1 || len(h.failures) != 0 {
		t.Errorf("got %d completions and %d failures, want 1 and 0", len(h.completes), len(h.failures))
	}
}

// A failed workflow whose compensation also fails calls OnWorkflowFailed once,
// with both errors.
func TestHooks_WorkflowFailedOnceWhenCompensationFails(t *testing.T) {
	h := &lifecycleHooks{}
	app, cleanup := createTestApp(t, WithHooks(h))
	defer cleanup()

	errWorkflow := errors.New("shipping failed")
	reserve := DefineActivity("hooked_reserve",
		func(ctx context.Context, orderID string) (string, error) { return "reserved:" + orderID, nil },
		WithCompensation[string, string](func(ctx context.Context, orderID string) error {
			return errors.New("release failed")
		}),
	)
	wf := DefineWorkflow("hooked_saga", func(ctx *WorkflowContext, orderID string) (string, error) {
		if _, err := reserve.Execute(ctx, orderID); err != nil {
			return "", err
		}
		return "", errWorkflow
	})
	RegisterWorkflow(app, wf)

	ctx := context.Background()
	if err := app.Start(ctx); err != nil {
		t.Fatalf("start app: %v", err)
	}
	defer func() { _ = app.Shutdown(ctx) }()

	if _, err := StartWorkflow(ctx, app, wf, "ORD-2"); err != nil {
		t.Fatalf("start workflow: %v", err)
	}
	h.waitForEnd(t)

	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.failures) != 1 {
		t.Fatalf("OnWorkflowFailed called %d times, want 1", len(h.failures))
	}
	failure := h.failures[0].Error
	if !errors.Is(failure, errWorkflow) {
		t.Errorf("OnWorkflowFailed error %v does not wrap the workflow error", failure)
	}
	if !strings.Contains(failure.Error(), "release failed") {
		t.Errorf("OnWorkflowFailed error %q does not report the compensation failure", failure)
	}
	if len(h.starts) != 1 {
		t.Errorf("OnWorkflowStart called %d times, want 1", len(h.starts))
	}
}

// A run that waits ends with OnWorkflowSuspended; the run that resumes the
// workflow starts and ends on its own. Activity hooks get the workflow name.
func TestHooks_WaitingRunEndsWithSuspendHook(t *testing.T) {
	h := &lifecycleHooks{}
	app, cleanup := createTestApp(t, WithHooks(h),
		WithTimerCheckInterval(10*time.Millisecond),
		WithWorkflowResumptionInterval(10*time.Millisecond))
	defer cleanup()

	nap := DefineActivity("hooked_nap_step", func(ctx context.Context, s string) (string, error) { return s, nil })
	wf := DefineWorkflow("hooked_napper", func(ctx *WorkflowContext, order hookOrder) (string, error) {
		if _, err := nap.Execute(ctx, order.OrderID); err != nil {
			return "", err
		}
		if err := Sleep(ctx, 10*time.Millisecond); err != nil {
			return "", err
		}
		return "woke:" + order.OrderID, nil
	})
	RegisterWorkflow(app, wf)

	ctx := context.Background()
	if err := app.Start(ctx); err != nil {
		t.Fatalf("start app: %v", err)
	}
	defer func() { _ = app.Shutdown(ctx) }()

	instanceID, err := StartWorkflow(ctx, app, wf, hookOrder{OrderID: "ORD-3"})
	if err != nil {
		t.Fatalf("start workflow: %v", err)
	}
	h.waitForEnd(t)

	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.suspends) != 1 {
		t.Fatalf("OnWorkflowSuspended called %d times, want 1", len(h.suspends))
	}
	suspend := h.suspends[0]
	if suspend.InstanceID != instanceID || suspend.WorkflowName != "hooked_napper" ||
		suspend.Reason != hooks.SuspendWaiting || suspend.Error != nil {
		t.Errorf("OnWorkflowSuspended got %+v, want %s/hooked_napper waiting without error", suspend, instanceID)
	}
	if len(h.starts) != 2 || len(h.completes) != 1 || len(h.failures) != 0 {
		t.Errorf("got %d starts, %d completions and %d failures, want 2, 1 and 0",
			len(h.starts), len(h.completes), len(h.failures))
	}
	if len(h.activities) == 0 {
		t.Fatal("OnActivityStart was not called")
	}
	for _, a := range h.activities {
		if a.WorkflowName != "hooked_napper" {
			t.Errorf("OnActivityStart WorkflowName = %q, want hooked_napper", a.WorkflowName)
		}
	}
}
