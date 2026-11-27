package romancy

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/i2y/romancy/hooks"
	"github.com/i2y/romancy/internal/replay"
	"github.com/i2y/romancy/internal/storage"
	"github.com/i2y/romancy/retry"
)

func TestActivityTransactionDefault(t *testing.T) {
	// Create activity with default settings (transactional=true)
	activity := DefineActivity("test_activity", func(ctx context.Context, input string) (string, error) {
		return "result:" + input, nil
	})

	if !activity.transactional {
		t.Error("Expected transactional to be true by default")
	}
}

func TestActivityTransactionOptOut(t *testing.T) {
	// Create activity with transactional=false
	activity := DefineActivity("test_activity",
		func(ctx context.Context, input string) (string, error) {
			return "result:" + input, nil
		},
		WithTransactional[string, string](false),
	)

	if activity.transactional {
		t.Error("Expected transactional to be false after opt-out")
	}
}

func TestActivityExecutionInTransaction(t *testing.T) {
	// Create a real SQLite storage for testing
	store, err := storage.NewSQLiteStorage(":memory:")
	if err != nil {
		t.Fatalf("Failed to create storage: %v", err)
	}
	defer func() { _ = store.Close() }()

	if err := storage.InitializeTestSchema(context.Background(), store); err != nil {
		t.Fatalf("Failed to initialize storage: %v", err)
	}

	// Create the workflow instance first
	instance := &storage.WorkflowInstance{
		InstanceID:   "test-instance-tx-1",
		WorkflowName: "test_workflow",
		Status:       storage.StatusRunning,
	}
	if err := store.CreateInstance(context.Background(), instance); err != nil {
		t.Fatalf("Failed to create instance: %v", err)
	}

	// Track if transaction was active during execution
	var wasInTransaction bool

	activity := DefineActivity("check_transaction",
		func(ctx context.Context, input string) (string, error) {
			// Check if we're in a transaction
			wasInTransaction = store.InTransaction(ctx)
			return "done", nil
		},
	)

	// Create engine and run workflow
	engine := replay.NewEngine(store, &hooks.NoOpHooks{}, "test-worker")

	err = engine.StartWorkflow(
		context.Background(),
		"test-instance-tx-1",
		"test_workflow",
		[]byte(`"test-input"`),
		func(execCtx *replay.ExecutionContext) (any, error) {
			wfCtx := NewWorkflowContextFromExecution(execCtx)
			return activity.Execute(wfCtx, "test-input")
		},
	)

	if err != nil {
		t.Fatalf("Workflow execution failed: %v", err)
	}

	if !wasInTransaction {
		t.Error("Expected to be in transaction during activity execution")
	}
}

func TestActivityExecutionWithoutTransaction(t *testing.T) {
	// Create a real SQLite storage for testing
	store, err := storage.NewSQLiteStorage(":memory:")
	if err != nil {
		t.Fatalf("Failed to create storage: %v", err)
	}
	defer func() { _ = store.Close() }()

	if err := storage.InitializeTestSchema(context.Background(), store); err != nil {
		t.Fatalf("Failed to initialize storage: %v", err)
	}

	// Create the workflow instance first
	instance := &storage.WorkflowInstance{
		InstanceID:   "test-instance-no-tx-1",
		WorkflowName: "test_workflow",
		Status:       storage.StatusRunning,
	}
	if err := store.CreateInstance(context.Background(), instance); err != nil {
		t.Fatalf("Failed to create instance: %v", err)
	}

	// Track if transaction was active during execution
	var wasInTransaction bool

	activity := DefineActivity("check_no_transaction",
		func(ctx context.Context, input string) (string, error) {
			wasInTransaction = store.InTransaction(ctx)
			return "done", nil
		},
		WithTransactional[string, string](false), // Opt out of transactions
	)

	// Create engine and run workflow
	engine := replay.NewEngine(store, &hooks.NoOpHooks{}, "test-worker")

	err = engine.StartWorkflow(
		context.Background(),
		"test-instance-no-tx-1",
		"test_workflow",
		[]byte(`"test-input"`),
		func(execCtx *replay.ExecutionContext) (any, error) {
			wfCtx := NewWorkflowContextFromExecution(execCtx)
			return activity.Execute(wfCtx, "test-input")
		},
	)

	if err != nil {
		t.Fatalf("Workflow execution failed: %v", err)
	}

	if wasInTransaction {
		t.Error("Expected NOT to be in transaction when transactional=false")
	}
}

func TestActivityTransactionRollbackOnError(t *testing.T) {
	// Create a real SQLite storage for testing
	store, err := storage.NewSQLiteStorage(":memory:")
	if err != nil {
		t.Fatalf("Failed to create storage: %v", err)
	}
	defer func() { _ = store.Close() }()

	if err := storage.InitializeTestSchema(context.Background(), store); err != nil {
		t.Fatalf("Failed to initialize storage: %v", err)
	}

	// Create the workflow instance first
	instance := &storage.WorkflowInstance{
		InstanceID:   "test-instance-rollback-1",
		WorkflowName: "test_workflow",
		Status:       storage.StatusRunning,
	}
	if err := store.CreateInstance(context.Background(), instance); err != nil {
		t.Fatalf("Failed to create instance: %v", err)
	}

	// Create an activity that fails
	expectedErr := errors.New("activity failed")
	activity := DefineActivity("failing_activity",
		func(ctx context.Context, input string) (string, error) {
			return "", expectedErr
		},
	)

	// Create engine and run workflow
	engine := replay.NewEngine(store, &hooks.NoOpHooks{}, "test-worker")

	err = engine.StartWorkflow(
		context.Background(),
		"test-instance-rollback-1",
		"test_workflow",
		[]byte(`"test-input"`),
		func(execCtx *replay.ExecutionContext) (any, error) {
			wfCtx := NewWorkflowContextFromExecution(execCtx)
			return activity.Execute(wfCtx, "test-input")
		},
	)

	// Workflow should fail
	if err == nil {
		t.Fatal("Expected workflow to fail")
	}

	// The failed attempts were rolled back, so nothing of them is in
	// history; the activity's failure is recorded on its own after the
	// rollback (SEMANTICS §2), so a replay returns it instead of running the
	// body again.
	history, _, err := store.GetHistoryPaginated(context.Background(), "test-instance-rollback-1", 0, 100)
	if err != nil {
		t.Fatalf("Failed to get history: %v", err)
	}

	completed, failed := 0, 0
	for _, h := range history {
		switch h.EventType {
		case storage.HistoryActivityCompleted:
			completed++
		case storage.HistoryActivityFailed:
			failed++
		}
	}

	if completed != 0 || failed != 1 {
		t.Errorf("Expected exactly one ActivityFailed and no ActivityCompleted, got %d failed, %d completed", failed, completed)
	}
}

func TestStorageConnReturnsTransaction(t *testing.T) {
	// Create a real SQLite storage for testing
	store, err := storage.NewSQLiteStorage(":memory:")
	if err != nil {
		t.Fatalf("Failed to create storage: %v", err)
	}
	defer func() { _ = store.Close() }()

	if err := storage.InitializeTestSchema(context.Background(), store); err != nil {
		t.Fatalf("Failed to initialize storage: %v", err)
	}

	// Create the workflow instance first
	instance := &storage.WorkflowInstance{
		InstanceID:   "test-instance-conn-1",
		WorkflowName: "test_workflow",
		Status:       storage.StatusRunning,
	}
	if err := store.CreateInstance(context.Background(), instance); err != nil {
		t.Fatalf("Failed to create instance: %v", err)
	}

	var connNotNil bool
	var customQueryWorked bool

	activity := DefineActivity("conn_test",
		func(ctx context.Context, input string) (string, error) {
			// Get the executor (should be transaction)
			executor := store.Conn(ctx)
			connNotNil = (executor != nil)

			if executor != nil {
				// Try executing a custom query
				_, err := executor.ExecContext(ctx, "SELECT 1")
				customQueryWorked = (err == nil)
			}
			return "done", nil
		},
	)

	// Create engine and run workflow
	engine := replay.NewEngine(store, &hooks.NoOpHooks{}, "test-worker")

	err = engine.StartWorkflow(
		context.Background(),
		"test-instance-conn-1",
		"test_workflow",
		[]byte(`"test-input"`),
		func(execCtx *replay.ExecutionContext) (any, error) {
			wfCtx := NewWorkflowContextFromExecution(execCtx)
			return activity.Execute(wfCtx, "test-input")
		},
	)

	if err != nil {
		t.Fatalf("Workflow execution failed: %v", err)
	}

	if !connNotNil {
		t.Error("Expected Conn() to return non-nil executor")
	}

	if !customQueryWorked {
		t.Error("Expected custom query to work within transaction")
	}
}

func TestWorkflowContextSession(t *testing.T) {
	// Create a real SQLite storage for testing
	store, err := storage.NewSQLiteStorage(":memory:")
	if err != nil {
		t.Fatalf("Failed to create storage: %v", err)
	}
	defer func() { _ = store.Close() }()

	if err := storage.InitializeTestSchema(context.Background(), store); err != nil {
		t.Fatalf("Failed to initialize storage: %v", err)
	}

	// Create the workflow instance first
	instance := &storage.WorkflowInstance{
		InstanceID:   "test-instance-session-1",
		WorkflowName: "test_workflow",
		Status:       storage.StatusRunning,
	}
	if err := store.CreateInstance(context.Background(), instance); err != nil {
		t.Fatalf("Failed to create instance: %v", err)
	}

	var sessionNotNil bool
	var customQueryWorked bool

	activity := DefineActivity("session_test",
		func(ctx context.Context, input string) (string, error) {
			// In a real activity, the workflow context would be passed
			// Here we test that Conn works within transaction
			executor := store.Conn(ctx)
			sessionNotNil = (executor != nil)

			if executor != nil {
				_, err := executor.ExecContext(ctx, "SELECT 1")
				customQueryWorked = (err == nil)
			}
			return "done", nil
		},
	)

	// Create engine and run workflow
	engine := replay.NewEngine(store, &hooks.NoOpHooks{}, "test-worker")

	err = engine.StartWorkflow(
		context.Background(),
		"test-instance-session-1",
		"test_workflow",
		[]byte(`"test-input"`),
		func(execCtx *replay.ExecutionContext) (any, error) {
			wfCtx := NewWorkflowContextFromExecution(execCtx)

			// Test Session() method on workflow context before activity
			if wfCtx.Session() == nil {
				t.Error("Expected Session() to return non-nil before activity")
			}

			return activity.Execute(wfCtx, "test-input")
		},
	)

	if err != nil {
		t.Fatalf("Workflow execution failed: %v", err)
	}

	if !sessionNotNil {
		t.Error("Expected session to be available")
	}

	if !customQueryWorked {
		t.Error("Expected custom query to work with session")
	}
}

// On SQLite (WAL), a transaction whose first statement is a read cannot
// write once another connection has committed after that read. An activity
// in that position is rolled back and run again in a new transaction
// instead of failing the workflow, whether the failed write is the result
// record or a write in the body.
func TestActivityRunsAgainAfterSQLiteWriteConflict(t *testing.T) {
	for _, writeInBody := range []bool{false, true} {
		t.Run(fmt.Sprintf("writeInBody=%v", writeInBody), func(t *testing.T) {
			app, cleanup := replayTestApp(t)
			defer cleanup()
			ctx := context.Background()

			var runs atomic.Int32
			act := DefineActivity(fmt.Sprintf("write_conflict_%v", writeInBody),
				func(actx context.Context, _ struct{}) (int32, error) {
					wctx := GetWorkflowContext(actx)
					path := "/actor/" + wctx.InstanceID() + "/state"
					// The transaction's first statement is this read.
					if _, _, err := FSRead(wctx, path); err != nil {
						return 0, err
					}
					n := runs.Add(1)
					if n == 1 {
						// Another connection commits after the read.
						if err := app.Storage().FSPut(ctx, "/shared/bump", []byte("x"), "test", 0); err != nil {
							return 0, err
						}
					}
					if writeInBody {
						if err := FSWrite(wctx, path, []byte("v"), 0, ""); err != nil {
							return 0, err
						}
					}
					return n, nil
				})
			wf := DefineWorkflow(fmt.Sprintf("write_conflict_wf_%v", writeInBody),
				func(wctx *WorkflowContext, _ struct{}) (int32, error) {
					return act.Execute(wctx, struct{}{})
				})
			RegisterWorkflow(app, wf)

			id, err := StartWorkflow(ctx, app, wf, struct{}{})
			if err != nil {
				t.Fatal(err)
			}
			if inst := waitForTerminalInstance(t, app, id); inst.Status != storage.StatusCompleted {
				t.Fatalf("instance = %s", inst.Status)
			}
			result, err := GetWorkflowResult[int32](ctx, app, id)
			if err != nil {
				t.Fatal(err)
			}
			if result.Output != 2 || runs.Load() != 2 {
				t.Fatalf("output %d after %d runs; want the second run's result", result.Output, runs.Load())
			}
			if writeInBody {
				if obj, err := app.Storage().FSGet(ctx, "/actor/"+id+"/state"); err != nil || obj == nil {
					t.Fatalf("body write not committed: %v", err)
				}
			}
		})
	}
}

// runRetryScenario runs, through the replay engine on s, an activity whose
// first attempt inserts a row and then fails on a SQL error (which, on
// PostgreSQL, also aborts the transaction), and whose second attempt inserts
// a row and succeeds. It returns the attempts whose rows were committed and
// the number of attempts made.
func runRetryScenario(t *testing.T, s storage.Storage, name string) ([]int, int32) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.DB().ExecContext(ctx, "CREATE TABLE retry_rows (attempt INTEGER)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	instanceID := "retry-tx-" + name
	now := time.Now().UTC()
	if err := s.CreateInstance(ctx, &storage.WorkflowInstance{
		InstanceID:   instanceID,
		WorkflowName: "retry_tx",
		Status:       storage.StatusRunning,
		InputData:    []byte(`{}`),
		StartedAt:    now,
		UpdatedAt:    now,
	}); err != nil {
		t.Fatalf("create instance: %v", err)
	}

	var attempts atomic.Int32
	act := DefineActivity("retry_tx_step_"+name,
		func(actx context.Context, _ struct{}) (int32, error) {
			db := GetWorkflowContext(actx).Session()
			n := attempts.Add(1)
			if _, err := db.ExecContext(actx, fmt.Sprintf("INSERT INTO retry_rows (attempt) VALUES (%d)", n)); err != nil {
				return 0, err
			}
			if n == 1 {
				if _, err := db.ExecContext(actx, "SELECT * FROM retry_no_such_table"); err != nil {
					return 0, err
				}
				return 0, errors.New("the statement was expected to fail")
			}
			return n, nil
		},
		WithRetryPolicy[struct{}, int32](retry.Fixed(3, time.Millisecond)),
	)

	engine := replay.NewEngine(s, &hooks.NoOpHooks{}, "worker-1")
	if err := engine.StartWorkflow(ctx, instanceID, "retry_tx", nil, func(execCtx *replay.ExecutionContext) (any, error) {
		return act.Execute(NewWorkflowContextFromExecution(execCtx), struct{}{})
	}); err != nil {
		t.Fatalf("workflow failed after %d attempts: %v", attempts.Load(), err)
	}

	rows, err := s.DB().QueryContext(ctx, "SELECT attempt FROM retry_rows ORDER BY attempt")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var committed []int
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		committed = append(committed, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return committed, attempts.Load()
}

// Each attempt runs in its own transaction: a retry starts from a clean
// state, and only the successful attempt's writes are committed.
func TestActivityRetryRunsInNewTransaction(t *testing.T) {
	store, err := storage.NewSQLiteStorage(filepath.Join(t.TempDir(), "retry.db"))
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}
	defer func() { _ = store.Close() }()
	if err := storage.InitializeTestSchema(context.Background(), store); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	committed, attempts := runRetryScenario(t, store, "sqlite")
	if attempts != 2 || !reflect.DeepEqual(committed, []int{2}) {
		t.Fatalf("committed rows %v after %d attempts; want [2] after 2", committed, attempts)
	}
}
