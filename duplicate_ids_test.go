package romancy

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// logBuffer collects what slog's default logger writes.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureWarnings sends slog's default logger, at warning level and above,
// to a buffer until the test ends.
func captureWarnings(t *testing.T) *logBuffer {
	t.Helper()
	return captureLogs(t, slog.LevelWarn)
}

// captureLogs sends slog's default logger, at level and above, to a buffer
// until the test ends.
func captureLogs(t *testing.T, level slog.Level) *logBuffer {
	t.Helper()
	b := &logBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(b, &slog.HandlerOptions{Level: level})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return b
}

// waitForOutput waits until the workflow completes and returns its output.
func waitForOutput[O any](t *testing.T, app *App, instanceID string) O {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		result, err := GetWorkflowResult[O](ctx, app, instanceID)
		if err != nil {
			t.Fatalf("get result: %v", err)
		}
		switch result.Status {
		case "completed":
			return result.Output
		case "failed":
			t.Fatalf("workflow failed: %v", result.Error)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the workflow did not complete")
	var zero O
	return zero
}

// An activity ID used twice in one run is logged with the ID. The later call
// returns the earlier call's result without running.
func TestDuplicateActivityIDIsLogged(t *testing.T) {
	logs := captureWarnings(t)
	app, cleanup := createTestApp(t)
	defer cleanup()

	var bodies atomic.Int32
	step := DefineActivity("dup_step", func(ctx context.Context, s string) (string, error) {
		bodies.Add(1)
		return s, nil
	})
	twice := func(explicitID string) func(*WorkflowContext, string) ([]string, error) {
		return func(ctx *WorkflowContext, _ string) ([]string, error) {
			first, err := step.Execute(ctx, "explicit", WithActivityID(explicitID))
			if err != nil {
				return nil, err
			}
			// No ID given: generated as dup_step:1
			second, err := step.Execute(ctx, "generated")
			if err != nil {
				return nil, err
			}
			return []string{first, second}, nil
		}
	}
	colliding := DefineWorkflow("dup_ids", twice("dup_step:1"))
	distinct := DefineWorkflow("distinct_ids", twice("dup_step:item-1"))
	RegisterWorkflow(app, colliding)
	RegisterWorkflow(app, distinct)

	ctx := context.Background()
	if err := app.Start(ctx); err != nil {
		t.Fatalf("start app: %v", err)
	}
	defer func() { _ = app.Shutdown(ctx) }()

	collidingID, err := StartWorkflow(ctx, app, colliding, "")
	if err != nil {
		t.Fatalf("start workflow: %v", err)
	}
	if got := waitForOutput[[]string](t, app, collidingID); strings.Join(got, ",") != "explicit,explicit" {
		t.Errorf("output = %v, want the first result twice", got)
	}
	if got := bodies.Load(); got != 1 {
		t.Errorf("the activity body ran %d times, want 1", got)
	}
	warning := logs.String()
	for _, want := range []string{"activity ID used twice in one run", "activity_id=dup_step:1", "instance_id=" + collidingID, "workflow=dup_ids"} {
		if !strings.Contains(warning, want) {
			t.Errorf("log %q does not contain %q", warning, want)
		}
	}

	distinctID, err := StartWorkflow(ctx, app, distinct, "")
	if err != nil {
		t.Fatalf("start workflow: %v", err)
	}
	if got := waitForOutput[[]string](t, app, distinctID); strings.Join(got, ",") != "explicit,generated" {
		t.Errorf("output = %v, want both results", got)
	}
	if strings.Contains(logs.String(), distinctID) {
		t.Errorf("a workflow with distinct IDs logged a warning: %s", logs.String())
	}
}
