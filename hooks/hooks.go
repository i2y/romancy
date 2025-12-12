// Package hooks provides lifecycle hooks for workflow observability.
package hooks

import (
	"context"
	"time"
)

// WorkflowHooks defines callbacks for workflow lifecycle events.
// Implement this interface to add observability (logging, tracing, metrics).
type WorkflowHooks interface {
	// Workflow lifecycle
	OnWorkflowStart(ctx context.Context, info WorkflowStartInfo)
	OnWorkflowComplete(ctx context.Context, info WorkflowCompleteInfo)
	OnWorkflowFailed(ctx context.Context, info WorkflowFailedInfo)
	OnWorkflowCancelled(ctx context.Context, info WorkflowCancelledInfo)

	// Activity lifecycle
	OnActivityStart(ctx context.Context, info ActivityStartInfo)
	OnActivityComplete(ctx context.Context, info ActivityCompleteInfo)
	OnActivityFailed(ctx context.Context, info ActivityFailedInfo)
	OnActivityRetry(ctx context.Context, info ActivityRetryInfo)

	// Event handling
	OnEventReceived(ctx context.Context, info EventReceivedInfo)
	OnEventWaitStart(ctx context.Context, info EventWaitStartInfo)
	OnEventWaitComplete(ctx context.Context, info EventWaitCompleteInfo)
	OnEventTimeout(ctx context.Context, info EventTimeoutInfo)

	// Timer handling
	OnTimerStart(ctx context.Context, info TimerStartInfo)
	OnTimerFired(ctx context.Context, info TimerFiredInfo)

	// Replay
	OnReplayStart(ctx context.Context, info ReplayStartInfo)
	OnReplayComplete(ctx context.Context, info ReplayCompleteInfo)
	OnActivityCacheHit(ctx context.Context, info ActivityCacheHitInfo)
}

// WorkflowStartInfo contains information about a workflow start.
type WorkflowStartInfo struct {
	InstanceID   string
	WorkflowName string
	InputData    any
	StartTime    time.Time
}

// WorkflowCompleteInfo contains information about a workflow completion.
type WorkflowCompleteInfo struct {
	InstanceID   string
	WorkflowName string
	OutputData   any
	Duration     time.Duration
}

// WorkflowFailedInfo contains information about a workflow failure.
type WorkflowFailedInfo struct {
	InstanceID   string
	WorkflowName string
	Error        error
	Duration     time.Duration
}

// WorkflowCancelledInfo contains information about a workflow cancellation.
type WorkflowCancelledInfo struct {
	InstanceID   string
	WorkflowName string
	Reason       string
	Duration     time.Duration
}

// ActivityStartInfo contains information about an activity start.
type ActivityStartInfo struct {
	InstanceID   string
	WorkflowName string
	ActivityID   string
	ActivityName string
	InputData    any
	IsReplay     bool
}

// ActivityCompleteInfo contains information about an activity completion.
type ActivityCompleteInfo struct {
	InstanceID   string
	WorkflowName string
	ActivityID   string
	ActivityName string
	OutputData   any
	Duration     time.Duration
	IsReplay     bool
}

// ActivityFailedInfo contains information about an activity failure.
type ActivityFailedInfo struct {
	InstanceID   string
	WorkflowName string
	ActivityID   string
	ActivityName string
	Error        error
	Duration     time.Duration
	Attempt      int
}

// ActivityRetryInfo contains information about an activity retry.
type ActivityRetryInfo struct {
	InstanceID   string
	WorkflowName string
	ActivityID   string
	ActivityName string
	Attempt      int
	MaxAttempts  int
	NextDelay    time.Duration
	Error        error
}

// ActivityCacheHitInfo contains information about a cache hit during replay.
type ActivityCacheHitInfo struct {
	InstanceID   string
	WorkflowName string
	ActivityID   string
	ActivityName string
	CachedResult any
}

// EventReceivedInfo contains information about a received event.
type EventReceivedInfo struct {
	EventType   string
	EventID     string
	EventSource string
	InstanceID  string
}

// EventWaitStartInfo contains information about starting to wait for an event.
type EventWaitStartInfo struct {
	InstanceID   string
	WorkflowName string
	EventType    string
	Timeout      *time.Duration
}

// EventWaitCompleteInfo contains information about completing an event wait.
type EventWaitCompleteInfo struct {
	InstanceID   string
	WorkflowName string
	EventType    string
	Duration     time.Duration
}

// EventTimeoutInfo contains information about an event timeout.
type EventTimeoutInfo struct {
	InstanceID   string
	WorkflowName string
	EventType    string
}

// TimerStartInfo contains information about a timer start.
type TimerStartInfo struct {
	InstanceID   string
	WorkflowName string
	TimerID      string
	ExpiresAt    time.Time
}

// TimerFiredInfo contains information about a fired timer.
type TimerFiredInfo struct {
	InstanceID   string
	WorkflowName string
	TimerID      string
	FiredAt      time.Time
}

// ReplayStartInfo contains information about replay starting.
type ReplayStartInfo struct {
	InstanceID     string
	WorkflowName   string
	HistoryEvents  int
	ResumeActivity string
}

// ReplayCompleteInfo contains information about replay completion.
type ReplayCompleteInfo struct {
	InstanceID    string
	WorkflowName  string
	CacheHits     int
	NewActivities int
	Duration      time.Duration
}

// NoOpHooks is a no-operation implementation of WorkflowHooks.
// Use this as a base for partial implementations.
type NoOpHooks struct{}

func (n *NoOpHooks) OnWorkflowStart(ctx context.Context, info WorkflowStartInfo)         {}
func (n *NoOpHooks) OnWorkflowComplete(ctx context.Context, info WorkflowCompleteInfo)   {}
func (n *NoOpHooks) OnWorkflowFailed(ctx context.Context, info WorkflowFailedInfo)       {}
func (n *NoOpHooks) OnWorkflowCancelled(ctx context.Context, info WorkflowCancelledInfo) {}
func (n *NoOpHooks) OnActivityStart(ctx context.Context, info ActivityStartInfo)         {}
func (n *NoOpHooks) OnActivityComplete(ctx context.Context, info ActivityCompleteInfo)   {}
func (n *NoOpHooks) OnActivityFailed(ctx context.Context, info ActivityFailedInfo)       {}
func (n *NoOpHooks) OnActivityRetry(ctx context.Context, info ActivityRetryInfo)         {}
func (n *NoOpHooks) OnEventReceived(ctx context.Context, info EventReceivedInfo)         {}
func (n *NoOpHooks) OnEventWaitStart(ctx context.Context, info EventWaitStartInfo)       {}
func (n *NoOpHooks) OnEventWaitComplete(ctx context.Context, info EventWaitCompleteInfo) {}
func (n *NoOpHooks) OnEventTimeout(ctx context.Context, info EventTimeoutInfo)           {}
func (n *NoOpHooks) OnTimerStart(ctx context.Context, info TimerStartInfo)               {}
func (n *NoOpHooks) OnTimerFired(ctx context.Context, info TimerFiredInfo)               {}
func (n *NoOpHooks) OnReplayStart(ctx context.Context, info ReplayStartInfo)             {}
func (n *NoOpHooks) OnReplayComplete(ctx context.Context, info ReplayCompleteInfo)       {}
func (n *NoOpHooks) OnActivityCacheHit(ctx context.Context, info ActivityCacheHitInfo)   {}
