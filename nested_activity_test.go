package romancy

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/i2y/romancy/hooks"
	"github.com/i2y/romancy/internal/replay"
	"github.com/i2y/romancy/internal/storage"
	"github.com/i2y/romancy/retry"
)

// activityHooks records the activity IDs the activity hooks see.
type activityHooks struct {
	hooks.NoOpHooks
	mu      sync.Mutex
	started []string
	retried []string
}

func (h *activityHooks) OnActivityStart(_ context.Context, info hooks.ActivityStartInfo) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.started = append(h.started, info.ActivityID)
}

func (h *activityHooks) OnActivityRetry(_ context.Context, info hooks.ActivityRetryInfo) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.retried = append(h.retried, info.ActivityID)
}

func (h *activityHooks) snapshot() (started, retried []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.started...), append([]string(nil), h.retried...)
}

// recordedActivityIDs returns the activity IDs of the instance's
// ActivityCompleted events, in order.
func recordedActivityIDs(t *testing.T, s storage.Storage, instanceID string) []string {
	t.Helper()
	events, _, err := s.GetHistoryPaginated(context.Background(), instanceID, 0, 100)
	if err != nil {
		t.Fatalf("get history: %v", err)
	}
	var ids []string
	for _, e := range events {
		if e.EventType == storage.HistoryActivityCompleted {
			ids = append(ids, e.ActivityID)
		}
	}
	return ids
}

// An activity called inside another activity's body is part of the outer
// one: it takes no ID and records nothing, so a replay, which does not run the
// outer body, gives a later top-level call the same ID as the first run did.
func TestNestedActivityReplaysTheTopLevelResult(t *testing.T) {
	h := &activityHooks{}
	app, cleanup := createTestApp(t,
		WithHooks(h),
		WithTimerCheckInterval(20*time.Millisecond),
		WithWorkflowResumptionInterval(20*time.Millisecond))
	defer cleanup()

	inner := DefineActivity("nested_inner", func(_ context.Context, s string) (string, error) {
		return s, nil
	})
	outer := DefineActivity("nested_outer", func(ctx context.Context, _ string) (string, error) {
		return inner.Execute(GetWorkflowContext(ctx), "nested")
	})
	wf := DefineWorkflow("nested_replay", func(ctx *WorkflowContext, _ string) ([]string, error) {
		fromOuter, err := outer.Execute(ctx, "")
		if err != nil {
			return nil, err
		}
		// The rest runs in a second run, which replays the first part.
		if err := Sleep(ctx, 10*time.Millisecond); err != nil {
			return nil, err
		}
		top, err := inner.Execute(ctx, "top-level")
		if err != nil {
			return nil, err
		}
		return []string{fromOuter, top}, nil
	})
	RegisterWorkflow(app, wf)

	ctx := context.Background()
	if err := app.Start(ctx); err != nil {
		t.Fatalf("start app: %v", err)
	}
	id, err := StartWorkflow(ctx, app, wf, "")
	if err != nil {
		t.Fatalf("start workflow: %v", err)
	}
	if got := waitForOutput[[]string](t, app, id); !reflect.DeepEqual(got, []string{"nested", "top-level"}) {
		t.Errorf("output = %v, want [nested top-level]", got)
	}
	if got := recordedActivityIDs(t, app.Storage(), id); !reflect.DeepEqual(got, []string{"nested_outer:1", "sleep:1", "nested_inner:1"}) {
		t.Errorf("recorded activities = %v, want [nested_outer:1 sleep:1 nested_inner:1]", got)
	}
	started, _ := h.snapshot()
	for _, activityID := range started {
		if activityID != "nested_outer:1" && activityID != "nested_inner:1" {
			t.Errorf("OnActivityStart ran for %q: a nested call runs no hooks", activityID)
		}
	}
}

// A manual ID given to a nested call is ignored: it neither takes the ID
// from a later top-level call nor counts as a second use of it.
func TestNestedActivityIgnoresAManualID(t *testing.T) {
	logs := captureWarnings(t)
	app, cleanup := createTestApp(t)
	defer cleanup()

	inner := DefineActivity("nested_manual_inner", func(_ context.Context, s string) (string, error) {
		return s, nil
	})
	outer := DefineActivity("nested_manual_outer", func(ctx context.Context, _ string) (string, error) {
		return inner.Execute(GetWorkflowContext(ctx), "nested", WithActivityID("lookup:1"))
	})
	wf := DefineWorkflow("nested_manual", func(ctx *WorkflowContext, _ string) (string, error) {
		if _, err := outer.Execute(ctx, ""); err != nil {
			return "", err
		}
		return inner.Execute(ctx, "top-level", WithActivityID("lookup:1"))
	})
	RegisterWorkflow(app, wf)

	ctx := context.Background()
	if err := app.Start(ctx); err != nil {
		t.Fatalf("start app: %v", err)
	}
	id, err := StartWorkflow(ctx, app, wf, "")
	if err != nil {
		t.Fatalf("start workflow: %v", err)
	}
	if got := waitForOutput[string](t, app, id); got != "top-level" {
		t.Errorf("output = %q, want top-level", got)
	}
	if strings.Contains(logs.String(), "used twice") {
		t.Errorf("a nested call counted as a use of its manual ID: %s", logs.String())
	}
}

// newNestedTestStore opens an SQLite storage with the schema and a table for
// the activities' own writes, and creates a running instance.
func newNestedTestStore(t *testing.T, instanceID string) storage.Storage {
	t.Helper()
	s, err := storage.NewSQLiteStorage(filepath.Join(t.TempDir(), "nested.db"))
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	if err := storage.InitializeTestSchema(ctx, s); err != nil {
		t.Fatalf("init schema: %v", err)
	}
	if _, err := s.DB().ExecContext(ctx, "CREATE TABLE nested_rows (who TEXT)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	now := time.Now().UTC()
	if err := s.CreateInstance(ctx, &storage.WorkflowInstance{
		InstanceID:   instanceID,
		WorkflowName: "nested_tx",
		Status:       storage.StatusRunning,
		InputData:    []byte(`{}`),
		StartedAt:    now,
		UpdatedAt:    now,
	}); err != nil {
		t.Fatalf("create instance: %v", err)
	}
	return s
}

func nestedRows(t *testing.T, s storage.Storage) []string {
	t.Helper()
	rows, err := s.DB().QueryContext(context.Background(), "SELECT who FROM nested_rows ORDER BY who")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var who []string
	for rows.Next() {
		var w string
		if err := rows.Scan(&w); err != nil {
			t.Fatalf("scan: %v", err)
		}
		who = append(who, w)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return who
}

// A nested activity writes in the outer attempt's transaction: the writes
// commit with the outer activity and roll back with it.
func TestNestedActivityWritesInTheOuterTransaction(t *testing.T) {
	for _, tc := range []struct {
		name     string
		outerErr error
		wantRows []string
	}{
		{name: "outer commits", wantRows: []string{"inner", "outer"}},
		{name: "outer fails", outerErr: NewTerminalErrorf("outer fails after the inner call")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			instanceID := "nested-tx-" + strings.ReplaceAll(tc.name, " ", "-")
			s := newNestedTestStore(t, instanceID)
			insert := func(ctx context.Context, who string) error {
				_, err := GetWorkflowContext(ctx).Session().ExecContext(ctx, "INSERT INTO nested_rows (who) VALUES (?)", who)
				return err
			}
			inner := DefineActivity("nested_tx_inner", func(ctx context.Context, _ struct{}) (struct{}, error) {
				return struct{}{}, insert(ctx, "inner")
			})
			outer := DefineActivity("nested_tx_outer", func(ctx context.Context, _ struct{}) (struct{}, error) {
				// The inner call is the transaction's first write.
				if _, err := inner.Execute(GetWorkflowContext(ctx), struct{}{}); err != nil {
					return struct{}{}, err
				}
				if err := insert(ctx, "outer"); err != nil {
					return struct{}{}, err
				}
				return struct{}{}, tc.outerErr
			})

			engine := replay.NewEngine(s, &hooks.NoOpHooks{}, "worker-1")
			err := engine.StartWorkflow(context.Background(), instanceID, "nested_tx", nil, func(execCtx *replay.ExecutionContext) (any, error) {
				return outer.Execute(NewWorkflowContextFromExecution(execCtx), struct{}{})
			})
			if (err != nil) != (tc.outerErr != nil) {
				t.Fatalf("workflow error = %v, want error: %v", err, tc.outerErr != nil)
			}
			if got := nestedRows(t, s); !reflect.DeepEqual(got, tc.wantRows) {
				t.Errorf("rows = %v, want %v", got, tc.wantRows)
			}
			if got := recordedActivityIDs(t, s, instanceID); tc.outerErr == nil && !reflect.DeepEqual(got, []string{"nested_tx_outer:1"}) {
				t.Errorf("recorded activities = %v, want [nested_tx_outer:1]", got)
			}
		})
	}
}

// An error from a nested activity is the outer attempt's error: the outer
// retry policy runs the whole outer attempt again, and the inner activity's
// own retry policy does not apply.
func TestNestedActivityErrorIsTheOuterAttempts(t *testing.T) {
	const instanceID = "nested-retry"
	s := newNestedTestStore(t, instanceID)

	var innerRuns, outerRuns atomic.Int32
	inner := DefineActivity("nested_retry_inner", func(_ context.Context, _ struct{}) (int32, error) {
		if innerRuns.Add(1) == 1 {
			return 0, errors.New("transient")
		}
		return innerRuns.Load(), nil
	}, WithRetryPolicy[struct{}, int32](retry.Fixed(5, time.Millisecond)))
	outer := DefineActivity("nested_retry_outer", func(ctx context.Context, _ struct{}) (int32, error) {
		outerRuns.Add(1)
		return inner.Execute(GetWorkflowContext(ctx), struct{}{})
	}, WithRetryPolicy[struct{}, int32](retry.Fixed(2, time.Millisecond)))

	h := &activityHooks{}
	engine := replay.NewEngine(s, h, "worker-1")
	if err := engine.StartWorkflow(context.Background(), instanceID, "nested_tx", nil, func(execCtx *replay.ExecutionContext) (any, error) {
		return outer.Execute(NewWorkflowContextFromExecution(execCtx), struct{}{})
	}); err != nil {
		t.Fatalf("workflow failed: %v", err)
	}
	if innerRuns.Load() != 2 || outerRuns.Load() != 2 {
		t.Errorf("inner ran %d times and outer %d times, want 2 and 2", innerRuns.Load(), outerRuns.Load())
	}
	if _, retried := h.snapshot(); !reflect.DeepEqual(retried, []string{"nested_retry_outer:1"}) {
		t.Errorf("OnActivityRetry ran for %v, want only [nested_retry_outer:1]", retried)
	}
	if got := recordedActivityIDs(t, s, instanceID); !reflect.DeepEqual(got, []string{"nested_retry_outer:1"}) {
		t.Errorf("recorded activities = %v, want [nested_retry_outer:1]", got)
	}
}

// A nested activity's compensation is not registered: the outer activity is
// the unit that a saga compensates.
func TestNestedActivityRegistersNoCompensation(t *testing.T) {
	app, cleanup := createTestApp(t)
	defer cleanup()

	var compensated atomic.Bool
	inner := DefineActivity("nested_comp_inner", func(_ context.Context, s string) (string, error) {
		return s, nil
	}, WithCompensation[string, string](func(_ context.Context, _ string) error {
		compensated.Store(true)
		return nil
	}))
	outer := DefineActivity("nested_comp_outer", func(ctx context.Context, s string) (string, error) {
		return inner.Execute(GetWorkflowContext(ctx), s)
	})
	wf := DefineWorkflow("nested_comp", func(ctx *WorkflowContext, _ string) (string, error) {
		if _, err := outer.Execute(ctx, "x"); err != nil {
			return "", err
		}
		return "", errors.New("the workflow fails after the outer activity")
	})
	RegisterWorkflow(app, wf)

	ctx := context.Background()
	if err := app.Start(ctx); err != nil {
		t.Fatalf("start app: %v", err)
	}
	id, err := StartWorkflow(ctx, app, wf, "")
	if err != nil {
		t.Fatalf("start workflow: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		result, err := GetWorkflowResult[string](ctx, app, id)
		if err != nil {
			t.Fatalf("get result: %v", err)
		}
		if result.Status == "failed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("status = %s, want failed", result.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if compensated.Load() {
		t.Error("the nested activity's compensation ran")
	}
}
