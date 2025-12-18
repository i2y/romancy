package replay

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/i2y/romancy/hooks"
	"github.com/i2y/romancy/internal/storage"
)

func setupTestStorage(t *testing.T) storage.Storage {
	store, err := storage.NewSQLiteStorage(":memory:")
	require.NoError(t, err)
	require.NoError(t, storage.InitializeTestSchema(context.Background(), store))
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestNewEngine(t *testing.T) {
	tests := []struct {
		name     string
		workerID string
	}{
		{
			name:     "with worker ID",
			workerID: "worker-1",
		},
		{
			name:     "with UUID worker ID",
			workerID: "550e8400-e29b-41d4-a716-446655440000",
		},
		{
			name:     "with empty worker ID",
			workerID: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := setupTestStorage(t)
			hooks := &hooks.NoOpHooks{}

			engine := NewEngine(store, hooks, tt.workerID)

			require.NotNil(t, engine)
			assert.Equal(t, store, engine.storage)
			assert.Equal(t, hooks, engine.hooks)
			assert.Equal(t, tt.workerID, engine.workerID)
		})
	}
}

func TestExecutionContext_Context(t *testing.T) {
	ctx := context.Background()
	execCtx := &ExecutionContext{
		ctx:             ctx,
		instanceID:      "inst-1",
		activityCounter: make(map[string]int),
		cachedResults:   make(map[string]*CachedResult),
		cachedErrors:    make(map[string]error),
	}

	assert.Equal(t, ctx, execCtx.Context())
}

func TestExecutionContext_InstanceID(t *testing.T) {
	tests := []struct {
		name       string
		instanceID string
	}{
		{
			name:       "simple ID",
			instanceID: "inst-123",
		},
		{
			name:       "UUID ID",
			instanceID: "550e8400-e29b-41d4-a716-446655440000",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			execCtx := &ExecutionContext{
				ctx:             context.Background(),
				instanceID:      tt.instanceID,
				activityCounter: make(map[string]int),
				cachedResults:   make(map[string]*CachedResult),
				cachedErrors:    make(map[string]error),
			}

			assert.Equal(t, tt.instanceID, execCtx.InstanceID())
		})
	}
}

func TestExecutionContext_IsReplaying(t *testing.T) {
	tests := []struct {
		name        string
		isReplaying bool
	}{
		{
			name:        "replaying true",
			isReplaying: true,
		},
		{
			name:        "replaying false",
			isReplaying: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			execCtx := &ExecutionContext{
				ctx:             context.Background(),
				instanceID:      "inst-1",
				isReplaying:     tt.isReplaying,
				activityCounter: make(map[string]int),
				cachedResults:   make(map[string]*CachedResult),
				cachedErrors:    make(map[string]error),
			}

			assert.Equal(t, tt.isReplaying, execCtx.IsReplaying())
		})
	}
}

func TestExecutionContext_GenerateActivityID(t *testing.T) {
	tests := []struct {
		name        string
		calls       []string
		wantResults []string
	}{
		{
			name:        "sequential same activity",
			calls:       []string{"activity_a", "activity_a", "activity_a"},
			wantResults: []string{"activity_a:1", "activity_a:2", "activity_a:3"},
		},
		{
			name:        "different activities",
			calls:       []string{"func_a", "func_b", "func_c"},
			wantResults: []string{"func_a:1", "func_b:1", "func_c:1"},
		},
		{
			name:        "mixed activities",
			calls:       []string{"a", "b", "a", "b", "a"},
			wantResults: []string{"a:1", "b:1", "a:2", "b:2", "a:3"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			execCtx := &ExecutionContext{
				ctx:             context.Background(),
				instanceID:      "inst-1",
				activityCounter: make(map[string]int),
				cachedResults:   make(map[string]*CachedResult),
				cachedErrors:    make(map[string]error),
			}

			var results []string
			for _, call := range tt.calls {
				results = append(results, execCtx.GenerateActivityID(call))
			}

			assert.Equal(t, tt.wantResults, results)
		})
	}
}

func TestExecutionContext_GenerateActivityID_Concurrency(t *testing.T) {
	execCtx := &ExecutionContext{
		ctx:             context.Background(),
		instanceID:      "inst-1",
		activityCounter: make(map[string]int),
		cachedResults:   make(map[string]*CachedResult),
		cachedErrors:    make(map[string]error),
	}

	const goroutines = 50
	const callsPerGoroutine = 20

	var wg sync.WaitGroup
	results := make(chan string, goroutines*callsPerGoroutine)

	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < callsPerGoroutine; j++ {
				id := execCtx.GenerateActivityID("concurrent")
				results <- id
			}
		}()
	}

	wg.Wait()
	close(results)

	seen := make(map[string]bool)
	for id := range results {
		if seen[id] {
			t.Errorf("duplicate activity ID: %s", id)
		}
		seen[id] = true
	}

	assert.Len(t, seen, goroutines*callsPerGoroutine)
}

func TestExecutionContext_GetCachedResult(t *testing.T) {
	tests := []struct {
		name         string
		cachedValues map[string]*CachedResult
		cachedErrors map[string]error
		activityID   string
		wantResult   any
		wantFound    bool
	}{
		{
			name: "found in cachedResults",
			cachedValues: map[string]*CachedResult{
				"act:1": {Value: "result"},
			},
			cachedErrors: make(map[string]error),
			activityID:   "act:1",
			wantResult:   "result",
			wantFound:    true,
		},
		{
			name:         "found in cachedErrors",
			cachedValues: make(map[string]*CachedResult),
			cachedErrors: map[string]error{
				"act:2": errors.New("cached error"),
			},
			activityID: "act:2",
			wantResult: errors.New("cached error"),
			wantFound:  true,
		},
		{
			name:         "not found",
			cachedValues: make(map[string]*CachedResult),
			cachedErrors: make(map[string]error),
			activityID:   "act:3",
			wantResult:   nil,
			wantFound:    false,
		},
		{
			name: "result with int value",
			cachedValues: map[string]*CachedResult{
				"act:4": {Value: 42},
			},
			cachedErrors: make(map[string]error),
			activityID:   "act:4",
			wantResult:   42,
			wantFound:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			execCtx := &ExecutionContext{
				ctx:             context.Background(),
				instanceID:      "inst-1",
				activityCounter: make(map[string]int),
				cachedResults:   tt.cachedValues,
				cachedErrors:    tt.cachedErrors,
			}

			result, found := execCtx.GetCachedResult(tt.activityID)

			assert.Equal(t, tt.wantFound, found)
			if tt.wantFound {
				if wantErr, ok := tt.wantResult.(error); ok {
					gotErr, ok := result.(error)
					require.True(t, ok)
					assert.Equal(t, wantErr.Error(), gotErr.Error())
				} else {
					assert.Equal(t, tt.wantResult, result)
				}
			}
		})
	}
}

func TestExecutionContext_GetCachedResultRaw(t *testing.T) {
	tests := []struct {
		name         string
		cachedValues map[string]*CachedResult
		activityID   string
		wantRaw      []byte
		wantFound    bool
	}{
		{
			name: "found with raw JSON",
			cachedValues: map[string]*CachedResult{
				"act:1": {RawJSON: []byte(`{"key":"value"}`), Value: map[string]any{"key": "value"}},
			},
			activityID: "act:1",
			wantRaw:    []byte(`{"key":"value"}`),
			wantFound:  true,
		},
		{
			name: "found without raw JSON",
			cachedValues: map[string]*CachedResult{
				"act:2": {Value: "result"},
			},
			activityID: "act:2",
			wantRaw:    nil,
			wantFound:  true,
		},
		{
			name:         "not found",
			cachedValues: make(map[string]*CachedResult),
			activityID:   "act:3",
			wantRaw:      nil,
			wantFound:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			execCtx := &ExecutionContext{
				ctx:             context.Background(),
				instanceID:      "inst-1",
				activityCounter: make(map[string]int),
				cachedResults:   tt.cachedValues,
				cachedErrors:    make(map[string]error),
			}

			cached, found := execCtx.GetCachedResultRaw(tt.activityID)

			assert.Equal(t, tt.wantFound, found)
			if tt.wantFound {
				require.NotNil(t, cached)
				assert.Equal(t, tt.wantRaw, cached.RawJSON)
			} else {
				assert.Nil(t, cached)
			}
		})
	}
}

func TestExecutionContext_SetCachedResult(t *testing.T) {
	tests := []struct {
		name          string
		activityID    string
		result        any
		wantInResults bool
		wantInErrors  bool
	}{
		{
			name:          "set string result",
			activityID:    "act:1",
			result:        "success",
			wantInResults: true,
			wantInErrors:  false,
		},
		{
			name:          "set int result",
			activityID:    "act:2",
			result:        42,
			wantInResults: true,
			wantInErrors:  false,
		},
		{
			name:          "set error result",
			activityID:    "act:3",
			result:        errors.New("test error"),
			wantInResults: false,
			wantInErrors:  true,
		},
		{
			name:          "set struct result",
			activityID:    "act:4",
			result:        struct{ Name string }{Name: "test"},
			wantInResults: true,
			wantInErrors:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			execCtx := &ExecutionContext{
				ctx:             context.Background(),
				instanceID:      "inst-1",
				activityCounter: make(map[string]int),
				cachedResults:   make(map[string]*CachedResult),
				cachedErrors:    make(map[string]error),
			}

			execCtx.SetCachedResult(tt.activityID, tt.result)

			_, inResults := execCtx.cachedResults[tt.activityID]
			_, inErrors := execCtx.cachedErrors[tt.activityID]

			assert.Equal(t, tt.wantInResults, inResults)
			assert.Equal(t, tt.wantInErrors, inErrors)

			result, found := execCtx.GetCachedResult(tt.activityID)
			assert.True(t, found)

			if err, ok := tt.result.(error); ok {
				gotErr, ok := result.(error)
				require.True(t, ok)
				assert.Equal(t, err.Error(), gotErr.Error())
			} else {
				assert.Equal(t, tt.result, result)
			}
		})
	}
}

func TestExecutionContext_SetCachedResultWithRaw(t *testing.T) {
	execCtx := &ExecutionContext{
		ctx:             context.Background(),
		instanceID:      "inst-1",
		activityCounter: make(map[string]int),
		cachedResults:   make(map[string]*CachedResult),
		cachedErrors:    make(map[string]error),
	}

	rawJSON := []byte(`{"name":"test","value":42}`)
	value := map[string]any{"name": "test", "value": float64(42)}

	execCtx.SetCachedResultWithRaw("act:1", rawJSON, value)

	cached, found := execCtx.GetCachedResultRaw("act:1")
	require.True(t, found)
	require.NotNil(t, cached)
	assert.Equal(t, rawJSON, cached.RawJSON)
	assert.Equal(t, value, cached.Value)
}

func TestExecutionContext_Storage(t *testing.T) {
	store := setupTestStorage(t)
	engine := NewEngine(store, &hooks.NoOpHooks{}, "worker-1")

	execCtx := &ExecutionContext{
		ctx:             context.Background(),
		instanceID:      "inst-1",
		engine:          engine,
		activityCounter: make(map[string]int),
		cachedResults:   make(map[string]*CachedResult),
		cachedErrors:    make(map[string]error),
	}

	assert.Equal(t, store, execCtx.Storage())
}

func TestEngine_SetCompensationRunner(t *testing.T) {
	store := setupTestStorage(t)
	engine := NewEngine(store, &hooks.NoOpHooks{}, "worker-1")

	assert.Nil(t, engine.compensationRunner)

	runner := func(ctx context.Context, funcName string, arg []byte) error {
		return nil
	}

	engine.SetCompensationRunner(runner)

	assert.NotNil(t, engine.compensationRunner)
}

func TestEngine_StartWorkflow(t *testing.T) {
	tests := []struct {
		name         string
		runnerResult any
		runnerErr    error
		wantStatus   storage.WorkflowStatus
		wantErr      bool
	}{
		{
			name:         "success",
			runnerResult: "completed result",
			runnerErr:    nil,
			wantStatus:   storage.StatusCompleted,
			wantErr:      false,
		},
		{
			name:         "failure",
			runnerResult: nil,
			runnerErr:    errors.New("workflow error"),
			wantStatus:   storage.StatusFailed,
			wantErr:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := setupTestStorage(t)
			engine := NewEngine(store, &hooks.NoOpHooks{}, "worker-1")

			instance := &storage.WorkflowInstance{
				InstanceID:   "inst-" + tt.name,
				WorkflowName: "test_workflow",
				Status:       storage.StatusRunning,
			}
			require.NoError(t, store.CreateInstance(context.Background(), instance))

			runner := func(execCtx *ExecutionContext) (any, error) {
				return tt.runnerResult, tt.runnerErr
			}

			err := engine.StartWorkflow(context.Background(), instance.InstanceID, "test_workflow", nil, runner)

			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}

			gotInstance, err := store.GetInstance(context.Background(), instance.InstanceID)
			require.NoError(t, err)
			assert.Equal(t, tt.wantStatus, gotInstance.Status)
		})
	}
}

func TestEngine_StartWorkflow_TimerSuspend(t *testing.T) {
	store := setupTestStorage(t)
	engine := NewEngine(store, &hooks.NoOpHooks{}, "worker-1")

	instance := &storage.WorkflowInstance{
		InstanceID:   "inst-timer",
		WorkflowName: "test_workflow",
		Status:       storage.StatusRunning,
	}
	require.NoError(t, store.CreateInstance(context.Background(), instance))

	expiresAt := time.Now().Add(1 * time.Hour)
	runner := func(execCtx *ExecutionContext) (any, error) {
		return nil, NewTimerSuspend(execCtx.InstanceID(), "timer-1", "timer-1", expiresAt)
	}

	err := engine.StartWorkflow(context.Background(), instance.InstanceID, "test_workflow", nil, runner)
	require.NoError(t, err)

	gotInstance, err := store.GetInstance(context.Background(), instance.InstanceID)
	require.NoError(t, err)
	assert.Equal(t, storage.StatusWaitingForTimer, gotInstance.Status)
}

func TestEngine_StartWorkflow_RecurSuspend(t *testing.T) {
	store := setupTestStorage(t)
	engine := NewEngine(store, &hooks.NoOpHooks{}, "worker-1")

	instance := &storage.WorkflowInstance{
		InstanceID:   "inst-recur",
		WorkflowName: "test_workflow",
		Status:       storage.StatusRunning,
	}
	require.NoError(t, store.CreateInstance(context.Background(), instance))

	runner := func(execCtx *ExecutionContext) (any, error) {
		return nil, NewRecurSuspend(execCtx.InstanceID(), map[string]int{"count": 5})
	}

	err := engine.StartWorkflow(context.Background(), instance.InstanceID, "test_workflow", nil, runner)
	require.NoError(t, err)

	gotInstance, err := store.GetInstance(context.Background(), instance.InstanceID)
	require.NoError(t, err)
	assert.Equal(t, storage.StatusRecurred, gotInstance.Status)
}

func TestEngine_RecordActivityResult(t *testing.T) {
	tests := []struct {
		name        string
		activityID  string
		result      any
		activityErr error
		wantEvent   storage.HistoryEventType
	}{
		{
			name:        "success result",
			activityID:  "act:1",
			result:      "success",
			activityErr: nil,
			wantEvent:   storage.HistoryActivityCompleted,
		},
		{
			name:        "error result",
			activityID:  "act:2",
			result:      nil,
			activityErr: errors.New("activity failed"),
			wantEvent:   storage.HistoryActivityFailed,
		},
		{
			name:        "struct result",
			activityID:  "act:3",
			result:      struct{ Name string }{Name: "test"},
			activityErr: nil,
			wantEvent:   storage.HistoryActivityCompleted,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := setupTestStorage(t)
			engine := NewEngine(store, &hooks.NoOpHooks{}, "worker-1")

			instance := &storage.WorkflowInstance{
				InstanceID:   "inst-" + tt.name,
				WorkflowName: "test_workflow",
				Status:       storage.StatusRunning,
			}
			require.NoError(t, store.CreateInstance(context.Background(), instance))

			err := engine.RecordActivityResult(context.Background(), instance.InstanceID, tt.activityID, tt.result, tt.activityErr)
			require.NoError(t, err)

			history, _, err := store.GetHistoryPaginated(context.Background(), instance.InstanceID, 0, 10)
			require.NoError(t, err)
			require.Len(t, history, 1)
			assert.Equal(t, tt.wantEvent, history[0].EventType)
			assert.Equal(t, tt.activityID, history[0].ActivityID)
		})
	}
}

func TestEngine_executeCompensations(t *testing.T) {
	tests := []struct {
		name          string
		compensations []string
		runnerError   error
		wantErr       bool
		wantExecuted  int
	}{
		{
			name:          "no compensations",
			compensations: nil,
			runnerError:   nil,
			wantErr:       false,
			wantExecuted:  0,
		},
		{
			name:          "all success",
			compensations: []string{"comp1", "comp2"},
			runnerError:   nil,
			wantErr:       false,
			wantExecuted:  2,
		},
		{
			name:          "runner fails",
			compensations: []string{"comp1"},
			runnerError:   errors.New("compensation failed"),
			wantErr:       true,
			wantExecuted:  1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := setupTestStorage(t)
			engine := NewEngine(store, &hooks.NoOpHooks{}, "worker-1")

			instance := &storage.WorkflowInstance{
				InstanceID:   "inst-" + tt.name,
				WorkflowName: "test_workflow",
				Status:       storage.StatusRunning,
			}
			require.NoError(t, store.CreateInstance(context.Background(), instance))

			for _, compName := range tt.compensations {
				entry := &storage.CompensationEntry{
					InstanceID:   instance.InstanceID,
					ActivityID:   compName,
					ActivityName: compName,
					Args:         []byte(`"arg"`),
				}
				require.NoError(t, store.AddCompensation(context.Background(), entry))
				// Small delay to ensure different millisecond timestamps for ordering
				time.Sleep(2 * time.Millisecond)
			}

			var executedCount int
			engine.SetCompensationRunner(func(ctx context.Context, funcName string, arg []byte) error {
				executedCount++
				return tt.runnerError
			})

			err := engine.executeCompensations(context.Background(), instance.InstanceID)

			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}

			assert.Equal(t, tt.wantExecuted, executedCount)
		})
	}
}

func TestEngine_executeCompensations_NoRunner(t *testing.T) {
	store := setupTestStorage(t)
	engine := NewEngine(store, &hooks.NoOpHooks{}, "worker-1")

	instance := &storage.WorkflowInstance{
		InstanceID:   "inst-no-runner",
		WorkflowName: "test_workflow",
		Status:       storage.StatusRunning,
	}
	require.NoError(t, store.CreateInstance(context.Background(), instance))

	entry := &storage.CompensationEntry{
		InstanceID:   instance.InstanceID,
		ActivityID:   "comp1",
		ActivityName: "comp1",
		Args:         []byte(`"arg"`),
	}
	require.NoError(t, store.AddCompensation(context.Background(), entry))

	err := engine.executeCompensations(context.Background(), instance.InstanceID)
	require.NoError(t, err)
}

func TestCachedResult_Structure(t *testing.T) {
	cached := &CachedResult{
		RawJSON: []byte(`{"key":"value"}`),
		Value:   map[string]any{"key": "value"},
	}

	assert.Equal(t, []byte(`{"key":"value"}`), cached.RawJSON)
	assert.Equal(t, map[string]any{"key": "value"}, cached.Value)
}
