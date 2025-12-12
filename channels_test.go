package romancy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/i2y/romancy/internal/storage"
)

func TestChannelMode_Constants(t *testing.T) {
	assert.Equal(t, storage.ChannelModeBroadcast, ModeBroadcast)
	assert.Equal(t, storage.ChannelModeCompeting, ModeCompeting)
}

func TestWithReceiveTimeout(t *testing.T) {
	tests := []struct {
		name    string
		timeout time.Duration
	}{
		{
			name:    "5 seconds",
			timeout: 5 * time.Second,
		},
		{
			name:    "1 minute",
			timeout: 1 * time.Minute,
		},
		{
			name:    "100 milliseconds",
			timeout: 100 * time.Millisecond,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := &receiveOptions{}
			WithReceiveTimeout(tt.timeout)(opts)
			require.NotNil(t, opts.timeout)
			assert.Equal(t, tt.timeout, *opts.timeout)
		})
	}
}

func TestWithMetadata(t *testing.T) {
	tests := []struct {
		name     string
		metadata map[string]any
	}{
		{
			name:     "simple metadata",
			metadata: map[string]any{"key": "value"},
		},
		{
			name:     "complex metadata",
			metadata: map[string]any{"string": "value", "int": 42, "bool": true},
		},
		{
			name:     "nested metadata",
			metadata: map[string]any{"nested": map[string]any{"key": "value"}},
		},
		{
			name:     "empty metadata",
			metadata: map[string]any{},
		},
		{
			name:     "nil metadata",
			metadata: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := &publishOptions{}
			WithMetadata(tt.metadata)(opts)
			assert.Equal(t, tt.metadata, opts.metadata)
		})
	}
}

func TestRecur(t *testing.T) {
	tests := []struct {
		name       string
		instanceID string
		input      any
	}{
		{
			name:       "int input",
			instanceID: "inst-1",
			input:      42,
		},
		{
			name:       "string input",
			instanceID: "inst-2",
			input:      "next-state",
		},
		{
			name:       "struct input",
			instanceID: "inst-3",
			input:      struct{ Count int }{Count: 10},
		},
		{
			name:       "map input",
			instanceID: "inst-4",
			input:      map[string]any{"iteration": 5},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			wfCtx := NewWorkflowContext(ctx, tt.instanceID, "worker-1", "workflow", nil, false)

			result, err := Recur(wfCtx, tt.input)

			require.Error(t, err)
			assert.True(t, IsSuspendSignal(err))

			signal := AsSuspendSignal(err)
			require.NotNil(t, signal)
			assert.Equal(t, SuspendForRecur, signal.Type)
			assert.Equal(t, tt.instanceID, signal.InstanceID)
			assert.Equal(t, tt.input, signal.NewInput)

			var zero any
			assert.Equal(t, zero, result)
		})
	}
}

func TestSubscribe_ReplayCacheHit(t *testing.T) {
	ctx := context.Background()
	historyCache := map[string]any{
		"subscribe_test-channel:1": true,
	}
	wfCtx := NewWorkflowContext(ctx, "inst-1", "worker-1", "workflow", historyCache, true)

	err := Subscribe(wfCtx, "test-channel", ModeBroadcast)
	require.NoError(t, err)
}

func TestSubscribe_NoStorage(t *testing.T) {
	ctx := context.Background()
	wfCtx := NewWorkflowContext(ctx, "inst-1", "worker-1", "workflow", nil, false)

	err := Subscribe(wfCtx, "test-channel", ModeBroadcast)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "storage not available")
}

func TestUnsubscribe_ReplayCacheHit(t *testing.T) {
	ctx := context.Background()
	historyCache := map[string]any{
		"unsubscribe_test-channel:1": true,
	}
	wfCtx := NewWorkflowContext(ctx, "inst-1", "worker-1", "workflow", historyCache, true)

	err := Unsubscribe(wfCtx, "test-channel")
	require.NoError(t, err)
}

func TestUnsubscribe_NoStorage(t *testing.T) {
	ctx := context.Background()
	wfCtx := NewWorkflowContext(ctx, "inst-1", "worker-1", "workflow", nil, false)

	err := Unsubscribe(wfCtx, "test-channel")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "storage not available")
}

func TestReceive_CachedResult(t *testing.T) {
	type TestMessage struct {
		Content string
	}

	ctx := context.Background()
	cachedMsg := &ReceivedMessage[TestMessage]{
		ID:          1,
		ChannelName: "test-channel",
		Data:        TestMessage{Content: "cached content"},
		CreatedAt:   time.Now(),
	}
	historyCache := map[string]any{
		"receive_test-channel:1": cachedMsg,
	}
	wfCtx := NewWorkflowContext(ctx, "inst-1", "worker-1", "workflow", historyCache, true)

	result, err := Receive[TestMessage](wfCtx, "test-channel")

	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, "cached content", result.Data.Content)
}

func TestReceive_CachedTimeoutError(t *testing.T) {
	type TestMessage struct {
		Content string
	}

	ctx := context.Background()
	cachedErr := &ChannelMessageTimeoutError{
		InstanceID:  "inst-1",
		ChannelName: "test-channel",
	}
	historyCache := map[string]any{
		"receive_test-channel:1": cachedErr,
	}
	wfCtx := NewWorkflowContext(ctx, "inst-1", "worker-1", "workflow", historyCache, true)

	result, err := Receive[TestMessage](wfCtx, "test-channel")

	require.Error(t, err)
	assert.Nil(t, result)

	var timeoutErr *ChannelMessageTimeoutError
	require.True(t, errors.As(err, &timeoutErr))
	assert.Equal(t, "inst-1", timeoutErr.InstanceID)
	assert.Equal(t, "test-channel", timeoutErr.ChannelName)
}

func TestReceive_NoStorage(t *testing.T) {
	type TestMessage struct {
		Content string
	}

	ctx := context.Background()
	wfCtx := NewWorkflowContext(ctx, "inst-1", "worker-1", "workflow", nil, false)

	result, err := Receive[TestMessage](wfCtx, "test-channel")

	require.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "storage not available")
}

func TestPublish_ReplayCacheHit(t *testing.T) {
	ctx := context.Background()
	historyCache := map[string]any{
		"publish_test-channel:1": true,
	}
	wfCtx := NewWorkflowContext(ctx, "inst-1", "worker-1", "workflow", historyCache, true)

	err := Publish(wfCtx, "test-channel", "test data")
	require.NoError(t, err)
}

func TestPublish_NoStorage(t *testing.T) {
	ctx := context.Background()
	wfCtx := NewWorkflowContext(ctx, "inst-1", "worker-1", "workflow", nil, false)

	err := Publish(wfCtx, "test-channel", "test data")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "storage not available")
}

func TestPublish_WithMetadata_NoStorage(t *testing.T) {
	ctx := context.Background()
	wfCtx := NewWorkflowContext(ctx, "inst-1", "worker-1", "workflow", nil, false)

	err := Publish(wfCtx, "test-channel", "test data", WithMetadata(map[string]any{"key": "value"}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "storage not available")
}

func TestSendTo_ReplayCacheHit(t *testing.T) {
	ctx := context.Background()
	historyCache := map[string]any{
		"send_to_target-inst_test-channel:1": true,
	}
	wfCtx := NewWorkflowContext(ctx, "inst-1", "worker-1", "workflow", historyCache, true)

	err := SendTo(wfCtx, "target-inst", "test-channel", "test data")
	require.NoError(t, err)
}

func TestSendTo_NoStorage(t *testing.T) {
	ctx := context.Background()
	wfCtx := NewWorkflowContext(ctx, "inst-1", "worker-1", "workflow", nil, false)

	err := SendTo(wfCtx, "target-inst", "test-channel", "test data")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "storage not available")
}

func TestJoinGroup_ReplayCacheHit(t *testing.T) {
	ctx := context.Background()
	historyCache := map[string]any{
		"join_group_test-group:1": true,
	}
	wfCtx := NewWorkflowContext(ctx, "inst-1", "worker-1", "workflow", historyCache, true)

	err := JoinGroup(wfCtx, "test-group")
	require.NoError(t, err)
}

func TestJoinGroup_NoStorage(t *testing.T) {
	ctx := context.Background()
	wfCtx := NewWorkflowContext(ctx, "inst-1", "worker-1", "workflow", nil, false)

	err := JoinGroup(wfCtx, "test-group")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "storage not available")
}

func TestLeaveGroup_ReplayCacheHit(t *testing.T) {
	ctx := context.Background()
	historyCache := map[string]any{
		"leave_group_test-group:1": true,
	}
	wfCtx := NewWorkflowContext(ctx, "inst-1", "worker-1", "workflow", historyCache, true)

	err := LeaveGroup(wfCtx, "test-group")
	require.NoError(t, err)
}

func TestLeaveGroup_NoStorage(t *testing.T) {
	ctx := context.Background()
	wfCtx := NewWorkflowContext(ctx, "inst-1", "worker-1", "workflow", nil, false)

	err := LeaveGroup(wfCtx, "test-group")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "storage not available")
}

func TestReceivedMessage_Structure(t *testing.T) {
	type CustomData struct {
		Field1 string
		Field2 int
	}

	msg := ReceivedMessage[CustomData]{
		ID:               123,
		ChannelName:      "test-channel",
		Data:             CustomData{Field1: "test", Field2: 42},
		Metadata:         map[string]any{"key": "value"},
		SenderInstanceID: "sender-1",
		CreatedAt:        time.Now(),
	}

	assert.Equal(t, int64(123), msg.ID)
	assert.Equal(t, "test-channel", msg.ChannelName)
	assert.Equal(t, "test", msg.Data.Field1)
	assert.Equal(t, 42, msg.Data.Field2)
	assert.Equal(t, "value", msg.Metadata["key"])
	assert.Equal(t, "sender-1", msg.SenderInstanceID)
}

func TestRecur_TypedInput(t *testing.T) {
	type IterationState struct {
		Count     int
		Processed []string
	}

	ctx := context.Background()
	wfCtx := NewWorkflowContext(ctx, "inst-1", "worker-1", "workflow", nil, false)

	input := IterationState{Count: 5, Processed: []string{"a", "b"}}
	result, err := Recur(wfCtx, input)

	require.Error(t, err)
	assert.True(t, IsSuspendSignal(err))

	signal := AsSuspendSignal(err)
	require.NotNil(t, signal)
	assert.Equal(t, SuspendForRecur, signal.Type)

	typedInput, ok := signal.NewInput.(IterationState)
	require.True(t, ok)
	assert.Equal(t, 5, typedInput.Count)
	assert.Equal(t, []string{"a", "b"}, typedInput.Processed)

	assert.Equal(t, IterationState{}, result)
}

func TestReceive_WithTimeout_Option(t *testing.T) {
	timeout := 30 * time.Second
	opt := WithReceiveTimeout(timeout)

	opts := &receiveOptions{}
	opt(opts)

	require.NotNil(t, opts.timeout)
	assert.Equal(t, timeout, *opts.timeout)
}

func TestReceive_NoTimeout_Option(t *testing.T) {
	opts := &receiveOptions{}

	assert.Nil(t, opts.timeout)
}

func TestActivityID_Generation(t *testing.T) {
	tests := []struct {
		name         string
		operation    string
		channelName  string
		calls        int
		wantPatterns []string
	}{
		{
			name:        "subscribe generates unique IDs",
			operation:   "subscribe",
			channelName: "orders",
			calls:       3,
			wantPatterns: []string{
				"subscribe_orders:1",
				"subscribe_orders:2",
				"subscribe_orders:3",
			},
		},
		{
			name:        "unsubscribe generates unique IDs",
			operation:   "unsubscribe",
			channelName: "events",
			calls:       2,
			wantPatterns: []string{
				"unsubscribe_events:1",
				"unsubscribe_events:2",
			},
		},
		{
			name:        "receive generates unique IDs",
			operation:   "receive",
			channelName: "notifications",
			calls:       2,
			wantPatterns: []string{
				"receive_notifications:1",
				"receive_notifications:2",
			},
		},
		{
			name:        "publish generates unique IDs",
			operation:   "publish",
			channelName: "updates",
			calls:       2,
			wantPatterns: []string{
				"publish_updates:1",
				"publish_updates:2",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			wfCtx := NewWorkflowContext(ctx, "inst-1", "worker-1", "workflow", nil, false)

			var generatedIDs []string
			for i := 0; i < tt.calls; i++ {
				id := wfCtx.GenerateActivityID(tt.operation + "_" + tt.channelName)
				generatedIDs = append(generatedIDs, id)
			}

			assert.Equal(t, tt.wantPatterns, generatedIDs)
		})
	}
}

func TestReceive_CachedGenericError(t *testing.T) {
	type TestMessage struct {
		Content string
	}

	ctx := context.Background()
	cachedErr := errors.New("some other error")
	historyCache := map[string]any{
		"receive_test-channel:1": cachedErr,
	}
	wfCtx := NewWorkflowContext(ctx, "inst-1", "worker-1", "workflow", historyCache, true)

	result, err := Receive[TestMessage](wfCtx, "test-channel")

	require.Error(t, err)
	assert.Nil(t, result)
	assert.Equal(t, cachedErr, err)
}
