package romancy

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Activities run from several goroutines of one workflow, each with its own
// activity ID (the pattern the replay page documents), share the run's
// context safely: the bodies are inside an activity, the workflow is not.
func TestConcurrentActivitiesInOneWorkflow(t *testing.T) {
	app, cleanup := createTestApp(t)
	defer cleanup()

	const n = 3
	var started sync.WaitGroup
	started.Add(n)
	release := make(chan struct{})
	var bodiesInActivity atomic.Int32

	steps := make([]*Activity[string, string], n)
	for i := range steps {
		name := fmt.Sprintf("concurrent_step_%d", i)
		steps[i] = DefineActivity(name, func(ctx context.Context, input string) (string, error) {
			if wfCtx := GetWorkflowContext(ctx); wfCtx != nil && wfCtx.InActivity() {
				bodiesInActivity.Add(1)
			}
			started.Done()
			<-release
			return name + ":" + input, nil
		})
	}

	var workflowInActivity atomic.Bool
	wf := DefineWorkflow("concurrent_steps", func(ctx *WorkflowContext, input string) ([]string, error) {
		results := make([]string, n)
		errs := make([]error, n)
		var wg sync.WaitGroup
		for i, step := range steps {
			wg.Add(1)
			go func() {
				defer wg.Done()
				results[i], errs[i] = step.Execute(ctx, input, WithActivityID(fmt.Sprintf("concurrent_step_%d:1", i)))
			}()
		}
		// Every body is running: the workflow itself is not in an activity.
		started.Wait()
		workflowInActivity.Store(ctx.InActivity())
		close(release)
		wg.Wait()
		for _, err := range errs {
			if err != nil {
				return nil, err
			}
		}
		return results, nil
	})
	RegisterWorkflow(app, wf)

	ctx := context.Background()
	if err := app.Start(ctx); err != nil {
		t.Fatalf("start app: %v", err)
	}
	defer func() { _ = app.Shutdown(ctx) }()

	instanceID, err := StartWorkflow(ctx, app, wf, "x")
	if err != nil {
		t.Fatalf("start workflow: %v", err)
	}

	var result *WorkflowResult[[]string]
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		result, err = GetWorkflowResult[[]string](ctx, app, instanceID)
		if err != nil {
			t.Fatalf("get result: %v", err)
		}
		if result.Status == "completed" || result.Status == "failed" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if result.Status != "completed" {
		t.Fatalf("status = %s (error %v), want completed", result.Status, result.Error)
	}
	for i, got := range result.Output {
		if want := fmt.Sprintf("concurrent_step_%d:x", i); got != want {
			t.Errorf("result[%d] = %q, want %q", i, got, want)
		}
	}
	if got := bodiesInActivity.Load(); got != n {
		t.Errorf("%d of %d activity bodies saw InActivity() true", got, n)
	}
	if workflowInActivity.Load() {
		t.Error("the workflow reported InActivity() true while its activities ran")
	}
}
