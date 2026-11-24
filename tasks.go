package romancy

import (
	"fmt"

	"github.com/google/uuid"
	"github.com/i2y/romancy/internal/storage"
)

// Task primitive (Vercel Board-style): a durable, claimable unit of work
// posted to a channel. Workers compete for tasks via ClaimTask — the
// competing-claim machinery (stale takeover, orphan recovery) applies.
//
// PostTask also publishes a lightweight notice on the channel so waiting
// workers wake; the task ROW is the source of truth, the message is only
// a wake-up signal. A typical worker loop:
//
//	for {
//	    task, _ := romancy.ClaimTask(ctx, "tasks:build")
//	    if task == nil { receive on channel / sleep; continue }
//	    result := doWork(task.Data)
//	    romancy.CompleteTask(ctx, task.ID, result, nil)
//	}

// PostTask creates a task on channel and publishes a wake-up notice.
// Returns the task ID.
func PostTask(ctx *WorkflowContext, channel string, data []byte) (string, error) {
	s := ctx.Storage()
	if s == nil {
		return "", fmt.Errorf("storage not available")
	}
	task := &storage.ActorTask{
		ID:      fmt.Sprintf("task-%s", uuid.NewString()),
		Channel: channel,
		Data:    data,
		State:   "open",
	}
	if err := s.PostTask(ctx.Context(), task); err != nil {
		return "", err
	}
	// Wake workers subscribed on this channel. Notice is best-effort —
	// the task row is the source of truth.
	_, _ = s.PublishToChannel(ctx.Context(), channel, []byte(fmt.Sprintf(
		`{"type":"task_posted","task_id":%q}`, task.ID)), nil)
	return task.ID, nil
}

// ClaimTask atomically claims the oldest open task on channel for this
// instance. Returns nil, nil when no open task exists.
func ClaimTask(ctx *WorkflowContext, channel string) (*storage.ActorTask, error) {
	s := ctx.Storage()
	if s == nil {
		return nil, fmt.Errorf("storage not available")
	}
	return s.ClaimTask(ctx.Context(), channel, ctx.InstanceID())
}

// CompleteTask marks a claimed task done (result JSON). Only the claimant
// may complete; otherwise ErrTaskConflict.
func CompleteTask(ctx *WorkflowContext, taskID string, result []byte) error {
	s := ctx.Storage()
	if s == nil {
		return fmt.Errorf("storage not available")
	}
	return s.CompleteTask(ctx.Context(), taskID, ctx.InstanceID(), result, false)
}

// FailTask marks a claimed task failed. Only the claimant may fail it.
func FailTask(ctx *WorkflowContext, taskID string, result []byte) error {
	s := ctx.Storage()
	if s == nil {
		return fmt.Errorf("storage not available")
	}
	return s.CompleteTask(ctx.Context(), taskID, ctx.InstanceID(), result, true)
}

// ListTasks returns tasks on channel filtered by state ("" = all).
func ListTasks(ctx *WorkflowContext, channel, state string) ([]*storage.ActorTask, error) {
	s := ctx.Storage()
	if s == nil {
		return nil, fmt.Errorf("storage not available")
	}
	return s.ListTasks(ctx.Context(), channel, state)
}

// ReadChannelSince returns messages on channel published after the given
// message_id cursor ("" reads all, oldest first). Use the last message's
// MessageID as the next cursor.
func ReadChannelSince(ctx *WorkflowContext, channel, messageID string) ([]*storage.ChannelMessage, error) {
	s := ctx.Storage()
	if s == nil {
		return nil, fmt.Errorf("storage not available")
	}
	return s.ChannelMessagesSince(ctx.Context(), channel, messageID)
}
