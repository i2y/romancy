package romancy

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

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

// TasksChannel returns the task-event stream name for a channel. All
// task lifecycle notices (posted/claimed/done/failed) are published on
// this broadcast channel — workers and waiters subscribe here.
func TasksChannel(channel string) string {
	return channel + ":tasks"
}

// PostTask creates a task on channel and publishes a "task_posted"
// notice on TasksChannel(channel). Returns the task ID.
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

// WaitTask blocks until the task reaches a terminal state (done or
// failed) and returns it. Subscribes to the channel's task-event stream
// (broadcast) and re-checks the task row on every wake — durable: the
// workflow releases its lock while suspended. timeout <= 0 waits
// indefinitely; on timeout returns the last known task and the timeout
// error.
func WaitTask(ctx *WorkflowContext, taskID string, timeout time.Duration) (*storage.ActorTask, error) {
	s := ctx.Storage()
	if s == nil {
		return nil, fmt.Errorf("storage not available")
	}
	task, err := s.GetTask(ctx.Context(), taskID)
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, fmt.Errorf("%w: %s", storage.ErrTaskNotFound, taskID)
	}
	return waitForTerminal(ctx, TasksChannel(task.Channel), timeout,
		func() (*storage.ActorTask, error) {
			t, err := s.GetTask(ctx.Context(), taskID)
			if err != nil {
				return nil, err
			}
			if t == nil {
				return nil, fmt.Errorf("%w: %s", storage.ErrTaskNotFound, taskID)
			}
			return t, nil
		}, task)
}

// WaitAnyTask blocks until any task on channel reaches a terminal state
// and returns it (earliest done, else earliest failed).
func WaitAnyTask(ctx *WorkflowContext, channel string, timeout time.Duration) (*storage.ActorTask, error) {
	s := ctx.Storage()
	if s == nil {
		return nil, fmt.Errorf("storage not available")
	}
	return waitForTerminal(ctx, TasksChannel(channel), timeout,
		func() (*storage.ActorTask, error) {
			for _, state := range []string{"done", "failed"} {
				tasks, err := s.ListTasks(ctx.Context(), channel, state)
				if err != nil {
					return nil, err
				}
				if len(tasks) > 0 {
					return tasks[0], nil
				}
			}
			return nil, nil
		}, nil)
}

// waitForTerminal subscribes broadcast on notifyCh, then loops:
// re-check → receive until check returns a terminal task or deadline.
func waitForTerminal(ctx *WorkflowContext, notifyCh string, timeout time.Duration,
	check func() (*storage.ActorTask, error), last *storage.ActorTask) (*storage.ActorTask, error) {
	var deadline time.Time
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	for {
		task, err := check()
		if err != nil {
			return nil, err
		}
		if task != nil {
			last = task
			if task.State == "done" || task.State == "failed" {
				return task, nil
			}
		}
		if err := Subscribe(ctx, notifyCh, ModeBroadcast); err != nil {
			return nil, err
		}
		opts := []ReceiveOption{}
		if timeout > 0 {
			rem := time.Until(deadline)
			if rem <= 0 {
				return last, &ChannelMessageTimeoutError{
					InstanceID:  ctx.InstanceID(),
					ChannelName: notifyCh,
				}
			}
			opts = append(opts, WithReceiveTimeout(rem))
		}
		if _, err := Receive[json.RawMessage](ctx, notifyCh, opts...); err != nil {
			var timeoutErr *ChannelMessageTimeoutError
			if errors.As(err, &timeoutErr) {
				if timeout <= 0 || time.Now().Before(deadline) {
					continue // spurious wake timeout — keep waiting
				}
				return last, err
			}
			return nil, err
		}
	}
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
