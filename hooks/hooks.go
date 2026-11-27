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
	// InputData is the input StartWorkflow was given, in the first run when
	// the process that called StartWorkflow runs it. It is nil in later runs,
	// and in a first run that a resumption poll began: on a worker other than
	// the one that started the workflow (which does not register it), or for
	// a workflow started inside an activity's transaction.
	InputData any
	StartTime time.Time
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

// WorkflowSuspendHooks is implemented by hooks that want to know when a run
// ends without finishing its workflow, for example to end what they opened
// in OnWorkflowStart: the workflow continues in a later run, possibly on
// another worker.
//
// After OnWorkflowStart, a run calls exactly one of OnWorkflowComplete,
// OnWorkflowFailed and OnWorkflowSuspended, in the same process.
// (OnWorkflowCancelled is called by the cancellation, not by a run; a run
// that finds its instance cancelled calls OnWorkflowSuspended.) The
// interface is separate from WorkflowHooks so that existing implementations
// keep compiling: the engine calls it on hooks that implement it. NoOpHooks
// does, so hooks that embed it can just define the method.
type WorkflowSuspendHooks interface {
	OnWorkflowSuspended(ctx context.Context, info WorkflowSuspendedInfo)
}

// SuspendReason tells why a run ended without finishing its workflow.
type SuspendReason string

const (
	// SuspendWaiting: the workflow waits for a timer, a channel message or
	// an event.
	SuspendWaiting SuspendReason = "waiting"
	// SuspendRecur: the workflow recurred and continues with new input.
	SuspendRecur SuspendReason = "recur"
	// SuspendAborted: the run stopped without an outcome: it lost its lock,
	// hit a storage error, the worker shut down, or the instance was
	// cancelled meanwhile. Unless the instance ended, a later run replays it.
	SuspendAborted SuspendReason = "aborted"
)

// WorkflowSuspendedInfo contains information about a run that ended without
// finishing its workflow.
type WorkflowSuspendedInfo struct {
	InstanceID   string
	WorkflowName string
	Reason       SuspendReason
	// Error is the cause of an aborted run, or the error that kept a wait
	// or a recur from being registered. It is nil when the run suspended as
	// planned.
	Error    error
	Duration time.Duration
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

// OnWorkflowSuspended implements WorkflowSuspendHooks.
func (n *NoOpHooks) OnWorkflowSuspended(ctx context.Context, info WorkflowSuspendedInfo) {}

var _ WorkflowSuspendHooks = (*NoOpHooks)(nil)
