package romancy

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTerminalError(t *testing.T) {
	tests := []struct {
		name       string
		innerErr   error
		wantMsg    string
		wantUnwrap error
	}{
		{
			name:       "with simple error",
			innerErr:   errors.New("some error"),
			wantMsg:    "terminal error: some error",
			wantUnwrap: errors.New("some error"),
		},
		{
			name:       "with formatted error",
			innerErr:   fmt.Errorf("error code: %d", 42),
			wantMsg:    "terminal error: error code: 42",
			wantUnwrap: fmt.Errorf("error code: %d", 42),
		},
		{
			name:       "with nil error",
			innerErr:   nil,
			wantMsg:    "terminal error: <nil>",
			wantUnwrap: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			termErr := NewTerminalError(tt.innerErr)

			assert.Equal(t, tt.wantMsg, termErr.Error())
			if tt.wantUnwrap != nil {
				assert.Equal(t, tt.wantUnwrap.Error(), termErr.Unwrap().Error())
			} else {
				assert.Nil(t, termErr.Unwrap())
			}
		})
	}
}

func TestNewTerminalErrorf(t *testing.T) {
	tests := []struct {
		name    string
		format  string
		args    []any
		wantMsg string
	}{
		{
			name:    "with string argument",
			format:  "failed: %s",
			args:    []any{"timeout"},
			wantMsg: "terminal error: failed: timeout",
		},
		{
			name:    "with multiple arguments",
			format:  "error at step %d: %s",
			args:    []any{3, "validation failed"},
			wantMsg: "terminal error: error at step 3: validation failed",
		},
		{
			name:    "no arguments",
			format:  "simple error",
			args:    nil,
			wantMsg: "terminal error: simple error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			termErr := NewTerminalErrorf(tt.format, tt.args...)
			assert.Equal(t, tt.wantMsg, termErr.Error())
		})
	}
}

func TestIsTerminalError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
		{
			name: "regular error",
			err:  errors.New("regular error"),
			want: false,
		},
		{
			name: "terminal error",
			err:  NewTerminalError(errors.New("inner")),
			want: true,
		},
		{
			name: "wrapped terminal error",
			err:  fmt.Errorf("wrapped: %w", NewTerminalError(errors.New("inner"))),
			want: true,
		},
		{
			name: "double wrapped terminal error",
			err:  fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", NewTerminalError(errors.New("core")))),
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsTerminalError(tt.err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestRetryExhaustedError(t *testing.T) {
	tests := []struct {
		name         string
		activityName string
		attempts     int
		lastErr      error
		wantContains []string
	}{
		{
			name:         "basic case",
			activityName: "send_email",
			attempts:     3,
			lastErr:      errors.New("connection refused"),
			wantContains: []string{"send_email", "3 attempts", "connection refused"},
		},
		{
			name:         "single attempt",
			activityName: "validate",
			attempts:     1,
			lastErr:      errors.New("invalid input"),
			wantContains: []string{"validate", "1 attempts", "invalid input"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := &RetryExhaustedError{
				ActivityName: tt.activityName,
				Attempts:     tt.attempts,
				LastErr:      tt.lastErr,
			}

			errMsg := err.Error()
			for _, substr := range tt.wantContains {
				assert.Contains(t, errMsg, substr)
			}

			assert.Equal(t, tt.lastErr, err.Unwrap())
		})
	}
}

func TestRetryExhaustedError_ErrorsIs(t *testing.T) {
	innerErr := errors.New("connection timeout")
	retryErr := &RetryExhaustedError{
		ActivityName: "fetch_data",
		Attempts:     5,
		LastErr:      innerErr,
	}

	assert.True(t, errors.Is(retryErr, innerErr))
}

func TestWorkflowCancelledError(t *testing.T) {
	tests := []struct {
		name       string
		instanceID string
		reason     string
		wantMsg    string
	}{
		{
			name:       "with reason",
			instanceID: "inst-123",
			reason:     "user requested",
			wantMsg:    "workflow inst-123 cancelled: user requested",
		},
		{
			name:       "without reason",
			instanceID: "inst-456",
			reason:     "",
			wantMsg:    "workflow inst-456 cancelled",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := &WorkflowCancelledError{
				InstanceID: tt.instanceID,
				Reason:     tt.reason,
			}
			assert.Equal(t, tt.wantMsg, err.Error())
		})
	}
}

func TestWorkflowNotFoundError(t *testing.T) {
	tests := []struct {
		name       string
		instanceID string
		wantMsg    string
	}{
		{
			name:       "simple ID",
			instanceID: "wf-001",
			wantMsg:    "workflow instance wf-001 not found",
		},
		{
			name:       "UUID style ID",
			instanceID: "550e8400-e29b-41d4-a716-446655440000",
			wantMsg:    "workflow instance 550e8400-e29b-41d4-a716-446655440000 not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := &WorkflowNotFoundError{InstanceID: tt.instanceID}
			assert.Equal(t, tt.wantMsg, err.Error())
		})
	}
}

func TestLockAcquisitionError(t *testing.T) {
	tests := []struct {
		name       string
		instanceID string
		workerID   string
		reason     string
		wantMsg    string
	}{
		{
			name:       "lock held by another worker",
			instanceID: "inst-1",
			workerID:   "worker-a",
			reason:     "lock held by worker-b",
			wantMsg:    "failed to acquire lock for instance inst-1 (worker: worker-a): lock held by worker-b",
		},
		{
			name:       "timeout",
			instanceID: "inst-2",
			workerID:   "worker-c",
			reason:     "timeout after 5s",
			wantMsg:    "failed to acquire lock for instance inst-2 (worker: worker-c): timeout after 5s",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := &LockAcquisitionError{
				InstanceID: tt.instanceID,
				WorkerID:   tt.workerID,
				Reason:     tt.reason,
			}
			assert.Equal(t, tt.wantMsg, err.Error())
		})
	}
}

func TestEventTimeoutError(t *testing.T) {
	tests := []struct {
		name       string
		instanceID string
		eventType  string
		wantMsg    string
	}{
		{
			name:       "order event",
			instanceID: "order-123",
			eventType:  "order.confirmed",
			wantMsg:    `timeout waiting for event "order.confirmed" on instance order-123`,
		},
		{
			name:       "payment event",
			instanceID: "payment-456",
			eventType:  "payment.completed",
			wantMsg:    `timeout waiting for event "payment.completed" on instance payment-456`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := &EventTimeoutError{
				InstanceID: tt.instanceID,
				EventType:  tt.eventType,
			}
			assert.Equal(t, tt.wantMsg, err.Error())
		})
	}
}

func TestChannelMessageTimeoutError(t *testing.T) {
	tests := []struct {
		name        string
		instanceID  string
		channelName string
		wantMsg     string
	}{
		{
			name:        "notification channel",
			instanceID:  "inst-001",
			channelName: "notifications",
			wantMsg:     `timeout waiting for channel message on "notifications" (instance: inst-001)`,
		},
		{
			name:        "events channel",
			instanceID:  "inst-002",
			channelName: "user.events",
			wantMsg:     `timeout waiting for channel message on "user.events" (instance: inst-002)`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := &ChannelMessageTimeoutError{
				InstanceID:  tt.instanceID,
				ChannelName: tt.channelName,
			}
			assert.Equal(t, tt.wantMsg, err.Error())
		})
	}
}

func TestChannelMessageTimeoutError_ErrorsAs(t *testing.T) {
	originalErr := &ChannelMessageTimeoutError{
		InstanceID:  "inst-test",
		ChannelName: "test-channel",
	}
	wrappedErr := fmt.Errorf("operation failed: %w", originalErr)

	var targetErr *ChannelMessageTimeoutError
	require.True(t, errors.As(wrappedErr, &targetErr))
	assert.Equal(t, "inst-test", targetErr.InstanceID)
	assert.Equal(t, "test-channel", targetErr.ChannelName)
}

func TestSentinelErrors(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		wantMsg string
	}{
		{
			name:    "ErrWorkflowAlreadyCompleted",
			err:     ErrWorkflowAlreadyCompleted,
			wantMsg: "workflow already completed",
		},
		{
			name:    "ErrWorkflowAlreadyCancelled",
			err:     ErrWorkflowAlreadyCancelled,
			wantMsg: "workflow already cancelled",
		},
		{
			name:    "ErrWorkflowAlreadyFailed",
			err:     ErrWorkflowAlreadyFailed,
			wantMsg: "workflow already failed",
		},
		{
			name:    "ErrInvalidWorkflowState",
			err:     ErrInvalidWorkflowState,
			wantMsg: "invalid workflow state",
		},
		{
			name:    "ErrActivityIDConflict",
			err:     ErrActivityIDConflict,
			wantMsg: "activity ID conflict: duplicate activity ID in workflow",
		},
		{
			name:    "ErrDeterminismViolation",
			err:     ErrDeterminismViolation,
			wantMsg: "determinism violation during replay",
		},
		{
			name:    "ErrChannelNotSubscribed",
			err:     ErrChannelNotSubscribed,
			wantMsg: "not subscribed to channel",
		},
		{
			name:    "ErrGroupNotJoined",
			err:     ErrGroupNotJoined,
			wantMsg: "not a member of group",
		},
		{
			name:    "ErrWorkflowNotCancellable",
			err:     ErrWorkflowNotCancellable,
			wantMsg: "workflow cannot be cancelled",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.wantMsg, tt.err.Error())
		})
	}
}

func TestSentinelErrors_ErrorsIs(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		wrappedIn error
	}{
		{
			name:      "ErrWorkflowAlreadyCompleted wrapped",
			err:       ErrWorkflowAlreadyCompleted,
			wrappedIn: fmt.Errorf("failed: %w", ErrWorkflowAlreadyCompleted),
		},
		{
			name:      "ErrWorkflowAlreadyCancelled wrapped",
			err:       ErrWorkflowAlreadyCancelled,
			wrappedIn: fmt.Errorf("failed: %w", ErrWorkflowAlreadyCancelled),
		},
		{
			name:      "ErrChannelNotSubscribed wrapped",
			err:       ErrChannelNotSubscribed,
			wrappedIn: fmt.Errorf("operation failed: %w", ErrChannelNotSubscribed),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.True(t, errors.Is(tt.wrappedIn, tt.err))
		})
	}
}

func TestSuspendType_Constants(t *testing.T) {
	assert.Equal(t, SuspendType(0), SuspendForTimer)
	assert.Equal(t, SuspendType(1), SuspendForChannelMessage)
	assert.Equal(t, SuspendType(2), SuspendForRecur)
}

func TestIsSuspendSignal(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
		{
			name: "regular error",
			err:  errors.New("some error"),
			want: false,
		},
		{
			name: "timer suspend signal",
			err:  NewTimerSuspend("inst-1", "timer-1", "timer-1", time.Now().Add(time.Hour)),
			want: true,
		},
		{
			name: "channel message suspend signal",
			err:  NewChannelMessageSuspend("inst-2", "events", "act-1", nil),
			want: true,
		},
		{
			name: "recur suspend signal",
			err:  NewRecurSuspend("inst-3", map[string]int{"count": 5}),
			want: true,
		},
		{
			name: "wrapped suspend signal",
			err:  fmt.Errorf("wrapped: %w", NewTimerSuspend("inst-4", "timer-2", "timer-2", time.Now())),
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsSuspendSignal(tt.err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestAsSuspendSignal(t *testing.T) {
	timerSignal := NewTimerSuspend("inst-1", "timer-1", "timer-1", time.Now().Add(time.Hour))
	channelSignal := NewChannelMessageSuspend("inst-2", "events", "act-1", nil)

	tests := []struct {
		name     string
		err      error
		wantNil  bool
		wantType SuspendType
	}{
		{
			name:    "nil error",
			err:     nil,
			wantNil: true,
		},
		{
			name:    "regular error",
			err:     errors.New("some error"),
			wantNil: true,
		},
		{
			name:     "timer suspend signal",
			err:      timerSignal,
			wantNil:  false,
			wantType: SuspendForTimer,
		},
		{
			name:     "channel message suspend signal",
			err:      channelSignal,
			wantNil:  false,
			wantType: SuspendForChannelMessage,
		},
		{
			name:     "wrapped suspend signal",
			err:      fmt.Errorf("wrapped: %w", timerSignal),
			wantNil:  false,
			wantType: SuspendForTimer,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AsSuspendSignal(tt.err)
			if tt.wantNil {
				assert.Nil(t, got)
			} else {
				require.NotNil(t, got)
				assert.Equal(t, tt.wantType, got.Type)
			}
		})
	}
}

func TestNewTimerSuspend(t *testing.T) {
	tests := []struct {
		name       string
		instanceID string
		timerID    string
		activityID string
		expiresAt  time.Time
	}{
		{
			name:       "basic timer",
			instanceID: "inst-1",
			timerID:    "sleep-timer",
			activityID: "sleep-timer",
			expiresAt:  time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC),
		},
		{
			name:       "delayed timer",
			instanceID: "inst-2",
			timerID:    "deadline-timer",
			activityID: "deadline-timer",
			expiresAt:  time.Date(2025, 6, 15, 0, 0, 0, 0, time.UTC),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			signal := NewTimerSuspend(tt.instanceID, tt.timerID, tt.activityID, tt.expiresAt)

			require.NotNil(t, signal)
			assert.Equal(t, SuspendForTimer, signal.Type)
			assert.Equal(t, tt.instanceID, signal.InstanceID)
			assert.Equal(t, tt.timerID, signal.TimerID)
			assert.Equal(t, tt.expiresAt, signal.ExpiresAt)
			assert.Equal(t, tt.activityID, signal.ActivityID)

			assert.Contains(t, signal.Error(), "waiting for timer")
			assert.Contains(t, signal.Error(), tt.timerID)
		})
	}
}

func TestNewChannelMessageSuspend(t *testing.T) {
	timeout := 5 * time.Minute

	tests := []struct {
		name       string
		instanceID string
		channel    string
		activityID string
		timeout    *time.Duration
	}{
		{
			name:       "without timeout",
			instanceID: "inst-1",
			channel:    "notifications",
			activityID: "receive:1",
			timeout:    nil,
		},
		{
			name:       "with timeout",
			instanceID: "inst-2",
			channel:    "events",
			activityID: "receive:2",
			timeout:    &timeout,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			signal := NewChannelMessageSuspend(tt.instanceID, tt.channel, tt.activityID, tt.timeout)

			require.NotNil(t, signal)
			assert.Equal(t, SuspendForChannelMessage, signal.Type)
			assert.Equal(t, tt.instanceID, signal.InstanceID)
			assert.Equal(t, tt.channel, signal.Channel)
			assert.Equal(t, tt.activityID, signal.ActivityID)
			assert.Equal(t, tt.timeout, signal.Timeout)

			assert.Contains(t, signal.Error(), "waiting for channel message")
			assert.Contains(t, signal.Error(), tt.channel)
		})
	}
}

func TestNewRecurSuspend(t *testing.T) {
	tests := []struct {
		name       string
		instanceID string
		newInput   any
	}{
		{
			name:       "with int input",
			instanceID: "inst-1",
			newInput:   42,
		},
		{
			name:       "with string input",
			instanceID: "inst-2",
			newInput:   "next-state",
		},
		{
			name:       "with struct input",
			instanceID: "inst-3",
			newInput:   struct{ Count int }{Count: 10},
		},
		{
			name:       "with map input",
			instanceID: "inst-4",
			newInput:   map[string]any{"iteration": 5, "data": "test"},
		},
		{
			name:       "with nil input",
			instanceID: "inst-5",
			newInput:   nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			signal := NewRecurSuspend(tt.instanceID, tt.newInput)

			require.NotNil(t, signal)
			assert.Equal(t, SuspendForRecur, signal.Type)
			assert.Equal(t, tt.instanceID, signal.InstanceID)
			assert.Equal(t, tt.newInput, signal.NewInput)

			assert.Contains(t, signal.Error(), "recurring instance")
			assert.Contains(t, signal.Error(), tt.instanceID)
		})
	}
}

func TestSuspendSignal_Error(t *testing.T) {
	timeout := 10 * time.Second

	tests := []struct {
		name         string
		signal       *SuspendSignal
		wantContains []string
	}{
		{
			name:         "timer suspend",
			signal:       NewTimerSuspend("inst-1", "timer-1", "timer-1", time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)),
			wantContains: []string{"workflow suspended", "waiting for timer", "timer-1"},
		},
		{
			name:         "channel message suspend without timeout",
			signal:       NewChannelMessageSuspend("inst-2", "events", "act-1", nil),
			wantContains: []string{"workflow suspended", "waiting for channel message", "events"},
		},
		{
			name:         "channel message suspend with timeout",
			signal:       NewChannelMessageSuspend("inst-3", "events", "act-2", &timeout),
			wantContains: []string{"workflow suspended", "waiting for channel message", "events", "timeout"},
		},
		{
			name:         "recur suspend",
			signal:       NewRecurSuspend("inst-4", nil),
			wantContains: []string{"workflow suspended", "recurring instance", "inst-4"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errMsg := tt.signal.Error()
			for _, substr := range tt.wantContains {
				assert.Contains(t, errMsg, substr)
			}
		})
	}
}
