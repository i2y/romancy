package romancy

import (
	"context"
	"errors"
	"testing"

	"github.com/i2y/romancy/hooks"
	"github.com/i2y/romancy/internal/replay"
	"github.com/i2y/romancy/internal/storage"
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

	if err := store.Initialize(context.Background()); err != nil {
		t.Fatalf("Failed to initialize storage: %v", err)
	}

	// Create the workflow instance first
	instance := &storage.WorkflowInstance{
		InstanceID:   "test-instance-tx-1",
		WorkflowName: "test_workflow",
		Status:       storage.StatusPending,
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

	if err := store.Initialize(context.Background()); err != nil {
		t.Fatalf("Failed to initialize storage: %v", err)
	}

	// Create the workflow instance first
	instance := &storage.WorkflowInstance{
		InstanceID:   "test-instance-no-tx-1",
		WorkflowName: "test_workflow",
		Status:       storage.StatusPending,
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

	if err := store.Initialize(context.Background()); err != nil {
		t.Fatalf("Failed to initialize storage: %v", err)
	}

	// Create the workflow instance first
	instance := &storage.WorkflowInstance{
		InstanceID:   "test-instance-rollback-1",
		WorkflowName: "test_workflow",
		Status:       storage.StatusPending,
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

	// Verify no history was recorded (transaction was rolled back)
	history, _, err := store.GetHistoryPaginated(context.Background(), "test-instance-rollback-1", 0, 100)
	if err != nil {
		t.Fatalf("Failed to get history: %v", err)
	}

	if len(history) != 0 {
		t.Errorf("Expected no history events after rollback, got %d", len(history))
	}
}

func TestStorageConnReturnsTransaction(t *testing.T) {
	// Create a real SQLite storage for testing
	store, err := storage.NewSQLiteStorage(":memory:")
	if err != nil {
		t.Fatalf("Failed to create storage: %v", err)
	}
	defer func() { _ = store.Close() }()

	if err := store.Initialize(context.Background()); err != nil {
		t.Fatalf("Failed to initialize storage: %v", err)
	}

	// Create the workflow instance first
	instance := &storage.WorkflowInstance{
		InstanceID:   "test-instance-conn-1",
		WorkflowName: "test_workflow",
		Status:       storage.StatusPending,
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

	if err := store.Initialize(context.Background()); err != nil {
		t.Fatalf("Failed to initialize storage: %v", err)
	}

	// Create the workflow instance first
	instance := &storage.WorkflowInstance{
		InstanceID:   "test-instance-session-1",
		WorkflowName: "test_workflow",
		Status:       storage.StatusPending,
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
