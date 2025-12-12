package romancy

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testContextKey is a custom type for context keys in tests.
type testContextKey string

func TestNewWorkflowContext(t *testing.T) {
	tests := []struct {
		name         string
		instanceID   string
		workerID     string
		workflowName string
		historyCache map[string]any
		isReplaying  bool
	}{
		{
			name:         "new execution without history",
			instanceID:   "inst-001",
			workerID:     "worker-1",
			workflowName: "test_workflow",
			historyCache: nil,
			isReplaying:  false,
		},
		{
			name:         "new execution with empty history",
			instanceID:   "inst-002",
			workerID:     "worker-2",
			workflowName: "test_workflow",
			historyCache: map[string]any{},
			isReplaying:  false,
		},
		{
			name:         "replay with history cache",
			instanceID:   "inst-003",
			workerID:     "worker-3",
			workflowName: "test_workflow",
			historyCache: map[string]any{
				"activity_a:1": "result1",
				"activity_b:1": 42,
			},
			isReplaying: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			wfCtx := NewWorkflowContext(ctx, tt.instanceID, tt.workerID, tt.workflowName, tt.historyCache, tt.isReplaying)

			require.NotNil(t, wfCtx)
			assert.Equal(t, tt.instanceID, wfCtx.InstanceID())
			assert.Equal(t, tt.workerID, wfCtx.WorkerID())
			assert.Equal(t, tt.workflowName, wfCtx.WorkflowName())
			assert.Equal(t, tt.isReplaying, wfCtx.IsReplaying())
			assert.NotNil(t, wfCtx.Context())
		})
	}
}

func TestWorkflowContext_Getters(t *testing.T) {
	ctx := context.Background()
	wfCtx := NewWorkflowContext(ctx, "test-instance", "test-worker", "test-workflow", nil, false)

	tests := []struct {
		name     string
		getter   func() any
		expected any
	}{
		{
			name:     "InstanceID",
			getter:   func() any { return wfCtx.InstanceID() },
			expected: "test-instance",
		},
		{
			name:     "WorkerID",
			getter:   func() any { return wfCtx.WorkerID() },
			expected: "test-worker",
		},
		{
			name:     "WorkflowName",
			getter:   func() any { return wfCtx.WorkflowName() },
			expected: "test-workflow",
		},
		{
			name:     "IsReplaying",
			getter:   func() any { return wfCtx.IsReplaying() },
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, tt.getter())
		})
	}
}

func TestWorkflowContext_GenerateActivityID(t *testing.T) {
	tests := []struct {
		name        string
		calls       []string
		wantResults []string
	}{
		{
			name:        "single function sequential calls",
			calls:       []string{"activity_a", "activity_a", "activity_a"},
			wantResults: []string{"activity_a:1", "activity_a:2", "activity_a:3"},
		},
		{
			name:        "different functions",
			calls:       []string{"func_a", "func_b", "func_c"},
			wantResults: []string{"func_a:1", "func_b:1", "func_c:1"},
		},
		{
			name:        "mixed functions",
			calls:       []string{"func_a", "func_b", "func_a", "func_b", "func_a"},
			wantResults: []string{"func_a:1", "func_b:1", "func_a:2", "func_b:2", "func_a:3"},
		},
		{
			name:        "single call",
			calls:       []string{"only_once"},
			wantResults: []string{"only_once:1"},
		},
		{
			name:        "many calls same function",
			calls:       []string{"repeat", "repeat", "repeat", "repeat", "repeat"},
			wantResults: []string{"repeat:1", "repeat:2", "repeat:3", "repeat:4", "repeat:5"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			wfCtx := NewWorkflowContext(ctx, "inst-1", "worker-1", "workflow", nil, false)

			for i, call := range tt.calls {
				result := wfCtx.GenerateActivityID(call)
				assert.Equal(t, tt.wantResults[i], result, "call %d", i)
			}
		})
	}
}

func TestWorkflowContext_GenerateActivityID_Deterministic(t *testing.T) {
	calls := []string{"a", "b", "a", "c", "b", "a"}

	ctx := context.Background()

	wfCtx1 := NewWorkflowContext(ctx, "inst-1", "worker-1", "workflow", nil, false)
	wfCtx2 := NewWorkflowContext(ctx, "inst-2", "worker-2", "workflow", nil, false)

	results1 := make([]string, 0, len(calls))
	results2 := make([]string, 0, len(calls))
	for _, call := range calls {
		results1 = append(results1, wfCtx1.GenerateActivityID(call))
		results2 = append(results2, wfCtx2.GenerateActivityID(call))
	}

	assert.Equal(t, results1, results2, "same call sequence should produce same IDs")
}

func TestWorkflowContext_GenerateActivityID_Concurrency(t *testing.T) {
	ctx := context.Background()
	wfCtx := NewWorkflowContext(ctx, "inst-1", "worker-1", "workflow", nil, false)

	const goroutines = 100
	const callsPerGoroutine = 10

	var wg sync.WaitGroup
	results := make(chan string, goroutines*callsPerGoroutine)

	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < callsPerGoroutine; j++ {
				id := wfCtx.GenerateActivityID("concurrent")
				results <- id
			}
		}()
	}

	wg.Wait()
	close(results)

	seen := make(map[string]bool)
	for id := range results {
		if seen[id] {
			t.Errorf("duplicate activity ID generated: %s", id)
		}
		seen[id] = true
	}

	assert.Len(t, seen, goroutines*callsPerGoroutine, "should have generated unique IDs for all calls")
}

func TestWorkflowContext_GetSetCachedResult_LocalCache(t *testing.T) {
	tests := []struct {
		name         string
		activityID   string
		setValue     any
		lookupID     string
		wantValue    any
		wantFound    bool
		initialCache map[string]any
	}{
		{
			name:         "set and get string",
			activityID:   "act:1",
			setValue:     "result string",
			lookupID:     "act:1",
			wantValue:    "result string",
			wantFound:    true,
			initialCache: nil,
		},
		{
			name:         "set and get int",
			activityID:   "act:2",
			setValue:     42,
			lookupID:     "act:2",
			wantValue:    42,
			wantFound:    true,
			initialCache: nil,
		},
		{
			name:         "set and get struct",
			activityID:   "act:3",
			setValue:     struct{ Name string }{Name: "test"},
			lookupID:     "act:3",
			wantValue:    struct{ Name string }{Name: "test"},
			wantFound:    true,
			initialCache: nil,
		},
		{
			name:         "set and get error",
			activityID:   "act:4",
			setValue:     errors.New("test error"),
			lookupID:     "act:4",
			wantValue:    errors.New("test error"),
			wantFound:    true,
			initialCache: nil,
		},
		{
			name:       "lookup non-existent",
			activityID: "",
			setValue:   nil,
			lookupID:   "missing:1",
			wantValue:  nil,
			wantFound:  false,
		},
		{
			name:         "get from initial cache",
			activityID:   "",
			setValue:     nil,
			lookupID:     "preset:1",
			wantValue:    "preset value",
			wantFound:    true,
			initialCache: map[string]any{"preset:1": "preset value"},
		},
		{
			name:         "set and get nil value",
			activityID:   "act:nil",
			setValue:     nil,
			lookupID:     "act:nil",
			wantValue:    nil,
			wantFound:    true,
			initialCache: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			wfCtx := NewWorkflowContext(ctx, "inst-1", "worker-1", "workflow", tt.initialCache, false)

			if tt.activityID != "" || tt.setValue != nil {
				wfCtx.SetCachedResult(tt.activityID, tt.setValue)
			}

			result, found := wfCtx.GetCachedResult(tt.lookupID)
			assert.Equal(t, tt.wantFound, found)

			if tt.wantFound {
				if wantErr, ok := tt.wantValue.(error); ok {
					gotErr, ok := result.(error)
					require.True(t, ok, "expected error type")
					assert.Equal(t, wantErr.Error(), gotErr.Error())
				} else {
					assert.Equal(t, tt.wantValue, result)
				}
			}
		})
	}
}

func TestWorkflowContext_GetCachedResult_NilCache(t *testing.T) {
	ctx := context.Background()
	wfCtx := NewWorkflowContext(ctx, "inst-1", "worker-1", "workflow", nil, false)

	result, found := wfCtx.GetCachedResult("any:1")
	assert.False(t, found)
	assert.Nil(t, result)
}

func TestWorkflowContext_SetCachedResult_InitializesCache(t *testing.T) {
	ctx := context.Background()
	wfCtx := NewWorkflowContext(ctx, "inst-1", "worker-1", "workflow", nil, false)

	result, found := wfCtx.GetCachedResult("test:1")
	assert.False(t, found)
	assert.Nil(t, result)

	wfCtx.SetCachedResult("test:1", "value")

	result, found = wfCtx.GetCachedResult("test:1")
	assert.True(t, found)
	assert.Equal(t, "value", result)
}

func TestWorkflowContext_WithContext(t *testing.T) {
	parentCtx := context.Background()
	wfCtx := NewWorkflowContext(parentCtx, "inst-1", "worker-1", "workflow", map[string]any{"a:1": "value"}, true)

	wfCtx.GenerateActivityID("counter")
	wfCtx.GenerateActivityID("counter")

	newCtx := context.WithValue(context.Background(), testContextKey("key"), "new-value")
	newWfCtx := wfCtx.WithContext(newCtx)

	assert.NotEqual(t, wfCtx.Context(), newWfCtx.Context())
	assert.Equal(t, newCtx, newWfCtx.Context())

	assert.Equal(t, wfCtx.InstanceID(), newWfCtx.InstanceID())
	assert.Equal(t, wfCtx.WorkerID(), newWfCtx.WorkerID())
	assert.Equal(t, wfCtx.WorkflowName(), newWfCtx.WorkflowName())
	assert.Equal(t, wfCtx.IsReplaying(), newWfCtx.IsReplaying())

	result, found := newWfCtx.GetCachedResult("a:1")
	assert.True(t, found)
	assert.Equal(t, "value", result)

	id := newWfCtx.GenerateActivityID("counter")
	assert.Equal(t, "counter:3", id)
}

func TestWorkflowContext_Cancel(t *testing.T) {
	ctx := context.Background()
	wfCtx := NewWorkflowContext(ctx, "inst-1", "worker-1", "workflow", nil, false)

	assert.Nil(t, wfCtx.Err())

	select {
	case <-wfCtx.Done():
		t.Fatal("context should not be done before cancel")
	default:
	}

	wfCtx.Cancel()

	<-wfCtx.Done()
	assert.Equal(t, context.Canceled, wfCtx.Err())
}

func TestWorkflowContext_Done(t *testing.T) {
	ctx := context.Background()
	wfCtx := NewWorkflowContext(ctx, "inst-1", "worker-1", "workflow", nil, false)

	doneChan := wfCtx.Done()
	require.NotNil(t, doneChan)

	select {
	case <-doneChan:
		t.Fatal("done channel should not be closed before cancel")
	default:
	}

	wfCtx.Cancel()

	select {
	case <-doneChan:
	default:
		t.Fatal("done channel should be closed after cancel")
	}
}

func TestWorkflowContext_Err(t *testing.T) {
	tests := []struct {
		name         string
		setupContext func() (*WorkflowContext, func())
		wantErr      error
	}{
		{
			name: "no error initially",
			setupContext: func() (*WorkflowContext, func()) {
				ctx := context.Background()
				wfCtx := NewWorkflowContext(ctx, "inst-1", "worker-1", "workflow", nil, false)
				return wfCtx, func() {}
			},
			wantErr: nil,
		},
		{
			name: "context canceled",
			setupContext: func() (*WorkflowContext, func()) {
				ctx := context.Background()
				wfCtx := NewWorkflowContext(ctx, "inst-1", "worker-1", "workflow", nil, false)
				return wfCtx, func() { wfCtx.Cancel() }
			},
			wantErr: context.Canceled,
		},
		{
			name: "parent context canceled",
			setupContext: func() (*WorkflowContext, func()) {
				parentCtx, parentCancel := context.WithCancel(context.Background())
				wfCtx := NewWorkflowContext(parentCtx, "inst-1", "worker-1", "workflow", nil, false)
				return wfCtx, parentCancel
			},
			wantErr: context.Canceled,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wfCtx, trigger := tt.setupContext()

			if tt.wantErr == nil {
				assert.Nil(t, wfCtx.Err())
			} else {
				trigger()
				assert.Equal(t, tt.wantErr, wfCtx.Err())
			}
		})
	}
}

func TestWorkflowContext_Storage_NilWithoutExecCtx(t *testing.T) {
	ctx := context.Background()
	wfCtx := NewWorkflowContext(ctx, "inst-1", "worker-1", "workflow", nil, false)

	assert.Nil(t, wfCtx.Storage())
}

func TestWorkflowContext_Session_NilWithoutStorage(t *testing.T) {
	ctx := context.Background()
	wfCtx := NewWorkflowContext(ctx, "inst-1", "worker-1", "workflow", nil, false)

	assert.Nil(t, wfCtx.Session())
}

func TestWorkflowContext_IsReplaying_Values(t *testing.T) {
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
			ctx := context.Background()
			wfCtx := NewWorkflowContext(ctx, "inst-1", "worker-1", "workflow", nil, tt.isReplaying)
			assert.Equal(t, tt.isReplaying, wfCtx.IsReplaying())
		})
	}
}

func TestWorkflowContext_RecordActivityID_WithoutExecCtx(t *testing.T) {
	ctx := context.Background()
	wfCtx := NewWorkflowContext(ctx, "inst-1", "worker-1", "workflow", nil, false)

	assert.NotPanics(t, func() {
		wfCtx.RecordActivityID("test:1")
	})
}

func TestWorkflowContext_RecordActivityResult_WithoutExecCtx(t *testing.T) {
	ctx := context.Background()
	wfCtx := NewWorkflowContext(ctx, "inst-1", "worker-1", "workflow", nil, false)

	err := wfCtx.RecordActivityResult("test:1", "result", nil)
	assert.NoError(t, err)

	result, found := wfCtx.GetCachedResult("test:1")
	assert.True(t, found)
	assert.Equal(t, "result", result)
}

func TestWorkflowContext_RecordActivityResult_CachesError(t *testing.T) {
	ctx := context.Background()
	wfCtx := NewWorkflowContext(ctx, "inst-1", "worker-1", "workflow", nil, false)

	testErr := errors.New("activity failed")
	err := wfCtx.RecordActivityResult("test:1", nil, testErr)
	assert.NoError(t, err)

	result, found := wfCtx.GetCachedResult("test:1")
	assert.True(t, found)
	assert.Equal(t, testErr, result)
}

func TestWorkflowContext_GetCachedResultRaw_WithoutExecCtx(t *testing.T) {
	ctx := context.Background()
	wfCtx := NewWorkflowContext(ctx, "inst-1", "worker-1", "workflow", nil, false)

	raw, found := wfCtx.GetCachedResultRaw("test:1")
	assert.False(t, found)
	assert.Nil(t, raw)
}
