package romancy

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/i2y/romancy/retry"
)

func TestDefineActivity(t *testing.T) {
	tests := []struct {
		name              string
		activityName      string
		opts              []ActivityOption[string, string]
		wantTransactional bool
		wantHasRetry      bool
		wantHasTimeout    bool
		wantHasComp       bool
	}{
		{
			name:              "default options",
			activityName:      "test_activity",
			opts:              nil,
			wantTransactional: true,
			wantHasRetry:      false,
			wantHasTimeout:    false,
			wantHasComp:       false,
		},
		{
			name:         "transactional false",
			activityName: "non_tx_activity",
			opts: []ActivityOption[string, string]{
				WithTransactional[string, string](false),
			},
			wantTransactional: false,
			wantHasRetry:      false,
			wantHasTimeout:    false,
			wantHasComp:       false,
		},
		{
			name:         "with retry policy",
			activityName: "retry_activity",
			opts: []ActivityOption[string, string]{
				WithRetryPolicy[string, string](retry.DefaultPolicy()),
			},
			wantTransactional: true,
			wantHasRetry:      true,
			wantHasTimeout:    false,
			wantHasComp:       false,
		},
		{
			name:         "with timeout",
			activityName: "timeout_activity",
			opts: []ActivityOption[string, string]{
				WithTimeout[string, string](5 * time.Second),
			},
			wantTransactional: true,
			wantHasRetry:      false,
			wantHasTimeout:    true,
			wantHasComp:       false,
		},
		{
			name:         "with compensation",
			activityName: "comp_activity",
			opts: []ActivityOption[string, string]{
				WithCompensation[string, string](func(ctx context.Context, input string) error {
					return nil
				}),
			},
			wantTransactional: true,
			wantHasRetry:      false,
			wantHasTimeout:    false,
			wantHasComp:       true,
		},
		{
			name:         "all options combined",
			activityName: "full_activity",
			opts: []ActivityOption[string, string]{
				WithTransactional[string, string](false),
				WithRetryPolicy[string, string](retry.Fixed(3, 100*time.Millisecond)),
				WithTimeout[string, string](10 * time.Second),
				WithCompensation[string, string](func(ctx context.Context, input string) error {
					return nil
				}),
			},
			wantTransactional: false,
			wantHasRetry:      true,
			wantHasTimeout:    true,
			wantHasComp:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			activity := DefineActivity(tt.activityName,
				func(ctx context.Context, input string) (string, error) {
					return input, nil
				},
				tt.opts...,
			)

			assert.Equal(t, tt.activityName, activity.Name())
			assert.Equal(t, tt.wantTransactional, activity.transactional)
			assert.Equal(t, tt.wantHasRetry, activity.retryPolicy != nil)
			assert.Equal(t, tt.wantHasTimeout, activity.timeout > 0)
			assert.Equal(t, tt.wantHasComp, activity.HasCompensation())
		})
	}
}

func TestActivity_Name(t *testing.T) {
	tests := []struct {
		name         string
		activityName string
	}{
		{
			name:         "simple name",
			activityName: "send_email",
		},
		{
			name:         "namespaced name",
			activityName: "order.process",
		},
		{
			name:         "empty name",
			activityName: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			activity := DefineActivity(tt.activityName,
				func(ctx context.Context, input string) (string, error) {
					return input, nil
				},
			)
			assert.Equal(t, tt.activityName, activity.Name())
		})
	}
}

func TestActivity_Execute_CacheHit(t *testing.T) {
	tests := []struct {
		name         string
		cachedResult any
		wantResult   string
		wantErr      bool
	}{
		{
			name:         "cached success result",
			cachedResult: "cached_value",
			wantResult:   "cached_value",
			wantErr:      false,
		},
		{
			name:         "cached error result",
			cachedResult: errors.New("cached error"),
			wantResult:   "",
			wantErr:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var fnCalled bool
			activity := DefineActivity("test_activity",
				func(ctx context.Context, input string) (string, error) {
					fnCalled = true
					return "fresh_result", nil
				},
			)

			ctx := context.Background()
			historyCache := map[string]any{
				"test_activity:1": tt.cachedResult,
			}
			wfCtx := NewWorkflowContext(ctx, "inst-1", "worker-1", "workflow", historyCache, true)

			result, err := activity.Execute(wfCtx, "input")

			assert.False(t, fnCalled, "activity function should not be called on cache hit")

			if tt.wantErr {
				require.Error(t, err)
				assert.Equal(t, tt.wantResult, result)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.wantResult, result)
			}
		})
	}
}

func TestActivity_Execute_WithActivityID(t *testing.T) {
	tests := []struct {
		name         string
		providedID   string
		expectID     string
		useAutoGenID bool
	}{
		{
			name:         "auto-generated ID",
			providedID:   "",
			expectID:     "my_activity:1",
			useAutoGenID: true,
		},
		{
			name:         "manual ID",
			providedID:   "custom_id_123",
			expectID:     "custom_id_123",
			useAutoGenID: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			activity := DefineActivity("my_activity",
				func(ctx context.Context, input string) (string, error) {
					return "result", nil
				},
			)

			ctx := context.Background()
			wfCtx := NewWorkflowContext(ctx, "inst-1", "worker-1", "workflow", nil, false)

			var opts []ExecuteOption
			if tt.providedID != "" {
				opts = append(opts, WithActivityID(tt.providedID))
			}

			result, err := activity.Execute(wfCtx, "input", opts...)
			require.NoError(t, err)
			assert.Equal(t, "result", result)

			cachedResult, found := wfCtx.GetCachedResult(tt.expectID)
			assert.True(t, found, "result should be cached with expected ID")
			assert.Equal(t, "result", cachedResult)
		})
	}
}

func TestActivity_executeWithRetry_SuccessOnFirstTry(t *testing.T) {
	var attempts int32
	activity := DefineActivity("test",
		func(ctx context.Context, input string) (string, error) {
			atomic.AddInt32(&attempts, 1)
			return "success", nil
		},
		WithRetryPolicy[string, string](retry.Fixed(3, time.Millisecond)),
	)

	ctx := context.Background()
	wfCtx := NewWorkflowContext(ctx, "inst-1", "worker-1", "workflow", nil, false)

	result, err := activity.executeWithRetry(wfCtx, "input", "test:1")

	require.NoError(t, err)
	assert.Equal(t, "success", result)
	assert.Equal(t, int32(1), atomic.LoadInt32(&attempts))
}

func TestActivity_executeWithRetry_SuccessAfterRetries(t *testing.T) {
	var attempts int32
	activity := DefineActivity("test",
		func(ctx context.Context, input string) (string, error) {
			count := atomic.AddInt32(&attempts, 1)
			if count < 3 {
				return "", errors.New("transient error")
			}
			return "success", nil
		},
		WithRetryPolicy[string, string](retry.Fixed(5, time.Millisecond)),
	)

	ctx := context.Background()
	wfCtx := NewWorkflowContext(ctx, "inst-1", "worker-1", "workflow", nil, false)

	result, err := activity.executeWithRetry(wfCtx, "input", "test:1")

	require.NoError(t, err)
	assert.Equal(t, "success", result)
	assert.Equal(t, int32(3), atomic.LoadInt32(&attempts))
}

func TestActivity_executeWithRetry_ExhaustedRetries(t *testing.T) {
	var attempts int32
	activity := DefineActivity("test",
		func(ctx context.Context, input string) (string, error) {
			atomic.AddInt32(&attempts, 1)
			return "", errors.New("persistent error")
		},
		WithRetryPolicy[string, string](retry.Fixed(3, time.Millisecond)),
	)

	ctx := context.Background()
	wfCtx := NewWorkflowContext(ctx, "inst-1", "worker-1", "workflow", nil, false)

	result, err := activity.executeWithRetry(wfCtx, "input", "test:1")

	require.Error(t, err)
	assert.Empty(t, result)
	assert.Equal(t, int32(3), atomic.LoadInt32(&attempts))

	var retryErr *RetryExhaustedError
	require.True(t, errors.As(err, &retryErr))
	assert.Equal(t, "test", retryErr.ActivityName)
	assert.Equal(t, 3, retryErr.Attempts)
}

func TestActivity_executeWithRetry_TerminalErrorNoRetry(t *testing.T) {
	var attempts int32
	activity := DefineActivity("test",
		func(ctx context.Context, input string) (string, error) {
			atomic.AddInt32(&attempts, 1)
			return "", NewTerminalError(errors.New("terminal"))
		},
		WithRetryPolicy[string, string](retry.Fixed(5, time.Millisecond)),
	)

	ctx := context.Background()
	wfCtx := NewWorkflowContext(ctx, "inst-1", "worker-1", "workflow", nil, false)

	result, err := activity.executeWithRetry(wfCtx, "input", "test:1")

	require.Error(t, err)
	assert.Empty(t, result)
	assert.Equal(t, int32(1), atomic.LoadInt32(&attempts), "should not retry terminal error")
	assert.True(t, IsTerminalError(err))
}

func TestActivity_executeWithRetry_ContextCancellation(t *testing.T) {
	activity := DefineActivity("test",
		func(ctx context.Context, input string) (string, error) {
			return "", errors.New("error")
		},
		WithRetryPolicy[string, string](retry.Fixed(100, 100*time.Millisecond)),
	)

	ctx, cancel := context.WithCancel(context.Background())
	wfCtx := NewWorkflowContext(ctx, "inst-1", "worker-1", "workflow", nil, false)

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err := activity.executeWithRetry(wfCtx, "input", "test:1")

	require.Error(t, err)
	assert.Equal(t, context.Canceled, err)
}

func TestActivity_executeWithRetry_Timeout(t *testing.T) {
	activity := DefineActivity("test",
		func(ctx context.Context, input string) (string, error) {
			select {
			case <-time.After(1 * time.Second):
				return "done", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		},
		WithTimeout[string, string](10*time.Millisecond),
		WithRetryPolicy[string, string](retry.Fixed(1, time.Millisecond)),
	)

	ctx := context.Background()
	wfCtx := NewWorkflowContext(ctx, "inst-1", "worker-1", "workflow", nil, false)

	_, err := activity.executeWithRetry(wfCtx, "input", "test:1")

	require.Error(t, err)
}

func TestActivity_Compensate(t *testing.T) {
	tests := []struct {
		name      string
		hasCompFn bool
		compErr   error
		wantErr   bool
	}{
		{
			name:      "no compensation function",
			hasCompFn: false,
			compErr:   nil,
			wantErr:   false,
		},
		{
			name:      "compensation success",
			hasCompFn: true,
			compErr:   nil,
			wantErr:   false,
		},
		{
			name:      "compensation fails",
			hasCompFn: true,
			compErr:   errors.New("compensation failed"),
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var compensationCalled bool
			var opts []ActivityOption[string, string]

			if tt.hasCompFn {
				opts = append(opts, WithCompensation[string, string](func(ctx context.Context, input string) error {
					compensationCalled = true
					return tt.compErr
				}))
			}

			activity := DefineActivity("test",
				func(ctx context.Context, input string) (string, error) {
					return "result", nil
				},
				opts...,
			)

			err := activity.Compensate(context.Background(), "input")

			if tt.hasCompFn {
				assert.True(t, compensationCalled)
			} else {
				assert.False(t, compensationCalled)
			}

			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestActivity_HasCompensation(t *testing.T) {
	tests := []struct {
		name    string
		hasComp bool
	}{
		{
			name:    "with compensation",
			hasComp: true,
		},
		{
			name:    "without compensation",
			hasComp: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var opts []ActivityOption[string, string]
			if tt.hasComp {
				opts = append(opts, WithCompensation[string, string](func(ctx context.Context, input string) error {
					return nil
				}))
			}

			activity := DefineActivity("test",
				func(ctx context.Context, input string) (string, error) {
					return "result", nil
				},
				opts...,
			)

			assert.Equal(t, tt.hasComp, activity.HasCompensation())
		})
	}
}

func TestRegisterActivity(t *testing.T) {
	tests := []struct {
		name           string
		activityName   string
		hasComp        bool
		wantRegistered bool
	}{
		{
			name:           "with compensation",
			activityName:   "reg_test_with_comp",
			hasComp:        true,
			wantRegistered: true,
		},
		{
			name:           "without compensation",
			activityName:   "reg_test_no_comp",
			hasComp:        false,
			wantRegistered: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var opts []ActivityOption[string, string]
			if tt.hasComp {
				opts = append(opts, WithCompensation[string, string](func(ctx context.Context, input string) error {
					return nil
				}))
			}

			activity := DefineActivity(tt.activityName,
				func(ctx context.Context, input string) (string, error) {
					return "result", nil
				},
				opts...,
			)

			RegisterActivity(activity)

			executor, found := GetCompensationExecutor(tt.activityName)
			assert.Equal(t, tt.wantRegistered, found)
			if tt.wantRegistered {
				assert.NotNil(t, executor)
			}
		})
	}
}

func TestGetCompensationExecutor(t *testing.T) {
	activityName := "executor_test_activity"

	activity := DefineActivity(activityName,
		func(ctx context.Context, input string) (string, error) {
			return "result", nil
		},
		WithCompensation[string, string](func(ctx context.Context, input string) error {
			if input == "error" {
				return errors.New("compensation error")
			}
			return nil
		}),
	)
	RegisterActivity(activity)

	tests := []struct {
		name         string
		activityName string
		input        string
		wantFound    bool
		wantErr      bool
	}{
		{
			name:         "registered activity - success",
			activityName: activityName,
			input:        `"success"`,
			wantFound:    true,
			wantErr:      false,
		},
		{
			name:         "registered activity - error",
			activityName: activityName,
			input:        `"error"`,
			wantFound:    true,
			wantErr:      true,
		},
		{
			name:         "unregistered activity",
			activityName: "nonexistent_activity",
			input:        `"input"`,
			wantFound:    false,
			wantErr:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			executor, found := GetCompensationExecutor(tt.activityName)
			assert.Equal(t, tt.wantFound, found)

			if found {
				err := executor(context.Background(), []byte(tt.input))
				if tt.wantErr {
					require.Error(t, err)
				} else {
					require.NoError(t, err)
				}
			}
		})
	}
}

func TestGetCompensationExecutor_InvalidJSON(t *testing.T) {
	activityName := "json_test_activity"

	activity := DefineActivity(activityName,
		func(ctx context.Context, input string) (string, error) {
			return "result", nil
		},
		WithCompensation[string, string](func(ctx context.Context, input string) error {
			return nil
		}),
	)
	RegisterActivity(activity)

	executor, found := GetCompensationExecutor(activityName)
	require.True(t, found)

	err := executor(context.Background(), []byte(`invalid json`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unmarshal")
}

func TestWithActivityID_Option(t *testing.T) {
	tests := []struct {
		name string
		id   string
	}{
		{
			name: "simple ID",
			id:   "my-activity-1",
		},
		{
			name: "UUID style ID",
			id:   "550e8400-e29b-41d4-a716-446655440000",
		},
		{
			name: "empty ID",
			id:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := &executeOptions{}
			WithActivityID(tt.id)(opts)
			assert.Equal(t, tt.id, opts.activityID)
		})
	}
}

func TestActivity_Execute_NonTransactional_NoStorage(t *testing.T) {
	var fnCalled bool
	activity := DefineActivity("test",
		func(ctx context.Context, input string) (string, error) {
			fnCalled = true
			return "result", nil
		},
	)

	ctx := context.Background()
	wfCtx := NewWorkflowContext(ctx, "inst-1", "worker-1", "workflow", nil, false)

	result, err := activity.Execute(wfCtx, "input")

	require.NoError(t, err)
	assert.Equal(t, "result", result)
	assert.True(t, fnCalled)

	cachedResult, found := wfCtx.GetCachedResult("test:1")
	assert.True(t, found)
	assert.Equal(t, "result", cachedResult)
}

func TestActivity_Execute_Error_NoStorage(t *testing.T) {
	expectedErr := NewTerminalError(errors.New("activity error"))
	activity := DefineActivity("test",
		func(ctx context.Context, input string) (string, error) {
			return "", expectedErr
		},
	)

	ctx := context.Background()
	wfCtx := NewWorkflowContext(ctx, "inst-1", "worker-1", "workflow", nil, false)

	result, err := activity.Execute(wfCtx, "input")

	require.Error(t, err)
	assert.Empty(t, result)
	assert.True(t, IsTerminalError(err))

	cachedResult, found := wfCtx.GetCachedResult("test:1")
	assert.True(t, found, "error should be cached")
	assert.True(t, IsTerminalError(cachedResult.(error)))
}

func TestActivity_TypedInputOutput(t *testing.T) {
	type Input struct {
		Name  string
		Value int
	}
	type Output struct {
		Result string
		Sum    int
	}

	activity := DefineActivity("typed_activity",
		func(ctx context.Context, input Input) (Output, error) {
			return Output{
				Result: "processed: " + input.Name,
				Sum:    input.Value * 2,
			}, nil
		},
	)

	ctx := context.Background()
	wfCtx := NewWorkflowContext(ctx, "inst-1", "worker-1", "workflow", nil, false)

	result, err := activity.Execute(wfCtx, Input{Name: "test", Value: 21})

	require.NoError(t, err)
	assert.Equal(t, "processed: test", result.Result)
	assert.Equal(t, 42, result.Sum)
}
