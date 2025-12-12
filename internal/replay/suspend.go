package replay

import (
	"errors"
	"fmt"
	"time"
)

// ========================================
// SuspendSignal - Unified Workflow Suspension
// ========================================

// SuspendType represents the type of workflow suspension.
type SuspendType int

const (
	// SuspendForTimer indicates the workflow is waiting for a timer to expire.
	SuspendForTimer SuspendType = iota
	// SuspendForChannelMessage indicates the workflow is waiting for a channel message (via Receive or WaitEvent).
	SuspendForChannelMessage
	// SuspendForRecur indicates the workflow is recursing with new input.
	SuspendForRecur
)

func (t SuspendType) String() string {
	switch t {
	case SuspendForTimer:
		return "timer"
	case SuspendForChannelMessage:
		return "channel_message"
	case SuspendForRecur:
		return "recur"
	default:
		return "unknown"
	}
}

// SuspendSignal is returned when a workflow needs to suspend execution.
// It implements the error interface for compatibility with Go's error handling,
// but it is NOT an error - it's a control flow signal.
//
// When a workflow function returns a SuspendSignal, callers should propagate it:
//
//	if err := romancy.Sleep(ctx, time.Hour); err != nil {
//	    return result, err  // Propagate the suspend signal
//	}
//
// The replay engine will detect SuspendSignal and handle the suspension appropriately.
type SuspendSignal struct {
	// Type indicates what the workflow is waiting for
	Type SuspendType

	// InstanceID is the workflow instance being suspended
	InstanceID string

	// For SuspendForTimer
	TimerID   string
	ExpiresAt time.Time
	Step      int // Activity step for replay (optional)

	// For SuspendForChannelMessage (channel/event waiting)
	Channel    string         // Channel or event type name
	Timeout    *time.Duration // Optional timeout
	ActivityID string         // Activity ID for replay matching

	// For SuspendForRecur
	NewInput any // New input for the next iteration
}

func (s *SuspendSignal) Error() string {
	switch s.Type {
	case SuspendForTimer:
		return fmt.Sprintf("workflow suspended: waiting for timer %q until %v", s.TimerID, s.ExpiresAt)
	case SuspendForChannelMessage:
		if s.Timeout != nil {
			return fmt.Sprintf("workflow suspended: waiting for channel message on %q (timeout: %v)", s.Channel, *s.Timeout)
		}
		return fmt.Sprintf("workflow suspended: waiting for channel message on %q", s.Channel)
	case SuspendForRecur:
		return fmt.Sprintf("workflow suspended: recurring instance %s", s.InstanceID)
	default:
		return "workflow suspended"
	}
}

// IsSuspendSignal returns true if the error is a SuspendSignal.
func IsSuspendSignal(err error) bool {
	var sig *SuspendSignal
	return errors.As(err, &sig)
}

// AsSuspendSignal extracts the SuspendSignal from an error if present.
// Returns nil if the error is not a SuspendSignal.
func AsSuspendSignal(err error) *SuspendSignal {
	var sig *SuspendSignal
	if errors.As(err, &sig) {
		return sig
	}
	return nil
}

// NewTimerSuspend creates a SuspendSignal for timer waiting.
func NewTimerSuspend(instanceID, timerID string, expiresAt time.Time, step int) *SuspendSignal {
	return &SuspendSignal{
		Type:       SuspendForTimer,
		InstanceID: instanceID,
		TimerID:    timerID,
		ExpiresAt:  expiresAt,
		Step:       step,
	}
}

// NewChannelMessageSuspend creates a SuspendSignal for channel message waiting (via Receive or WaitEvent).
func NewChannelMessageSuspend(instanceID, channel, activityID string, timeout *time.Duration) *SuspendSignal {
	return &SuspendSignal{
		Type:       SuspendForChannelMessage,
		InstanceID: instanceID,
		Channel:    channel,
		ActivityID: activityID,
		Timeout:    timeout,
	}
}

// NewRecurSuspend creates a SuspendSignal for workflow recursion.
func NewRecurSuspend(instanceID string, newInput any) *SuspendSignal {
	return &SuspendSignal{
		Type:       SuspendForRecur,
		InstanceID: instanceID,
		NewInput:   newInput,
	}
}
