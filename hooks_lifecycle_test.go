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

	mu        sync.Mutex
	starts    []hooks.WorkflowStartInfo
	completes []hooks.WorkflowCompleteInfo
	failures  []hooks.WorkflowFailedInfo
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
