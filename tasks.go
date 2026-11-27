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

// Every primitive below is a recorded step when called from workflow code
// (see runStep): a replay observes the recorded outcome — the same posted
// id, the same claimed task, the same poll result — instead of touching the
// queue again.

// PostTask creates a task on channel and publishes a "task_posted"
// notice on TasksChannel(channel). Returns the task ID.
func PostTask(ctx *WorkflowContext, channel string, data []byte) (string, error) {
	return runEffect(ctx, "romancy.post_task", func(ctx *WorkflowContext) (string, error) {
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
	})
}

// ClaimTask atomically claims the oldest open task on channel for this
// instance. Returns nil, nil when no open task exists.
func ClaimTask(ctx *WorkflowContext, channel string) (*storage.ActorTask, error) {
	return runEffect(ctx, "romancy.claim_task", func(ctx *WorkflowContext) (*storage.ActorTask, error) {
		s := ctx.Storage()
		if s == nil {
			return nil, fmt.Errorf("storage not available")
		}
		return s.ClaimTask(ctx.Context(), channel, ctx.InstanceID())
	})
}

// CompleteTask marks a claimed task done (result JSON). Only the claimant
// may complete; otherwise ErrTaskConflict.
func CompleteTask(ctx *WorkflowContext, taskID string, result []byte) error {
	return runEffectErr(ctx, "romancy.complete_task", func(ctx *WorkflowContext) error {
		s := ctx.Storage()
		if s == nil {
			return fmt.Errorf("storage not available")
		}
		return s.CompleteTask(ctx.Context(), taskID, ctx.InstanceID(), result, false)
	})
}

// FailTask marks a claimed task failed. Only the claimant may fail it.
func FailTask(ctx *WorkflowContext, taskID string, result []byte) error {
	return runEffectErr(ctx, "romancy.fail_task", func(ctx *WorkflowContext) error {
		s := ctx.Storage()
		if s == nil {
			return fmt.Errorf("storage not available")
		}
		return s.CompleteTask(ctx.Context(), taskID, ctx.InstanceID(), result, true)
	})
}

// ListTasks returns tasks on channel filtered by state ("" = all).
func ListTasks(ctx *WorkflowContext, channel, state string) ([]*storage.ActorTask, error) {
	return runStep(ctx, "romancy.list_tasks", func(ctx *WorkflowContext) ([]*storage.ActorTask, error) {
		s := ctx.Storage()
		if s == nil {
			return nil, fmt.Errorf("storage not available")
		}
		return s.ListTasks(ctx.Context(), channel, state)
	})
}

// WaitTask blocks until the task reaches a terminal state (done or
// failed) and returns it. Subscribes to the channel's task-event stream
// (broadcast) and re-checks the task row on every wake — durable: the
// workflow releases its lock while suspended. timeout <= 0 waits
// indefinitely; on timeout returns the last known task and the timeout
// error.
func WaitTask(ctx *WorkflowContext, taskID string, timeout time.Duration) (*storage.ActorTask, error) {
	poll := func() (*storage.ActorTask, error) {
		return runStep(ctx, "romancy.get_task", func(ctx *WorkflowContext) (*storage.ActorTask, error) {
			s := ctx.Storage()
			if s == nil {
				return nil, fmt.Errorf("storage not available")
			}
			t, err := s.GetTask(ctx.Context(), taskID)
			if err != nil {
				return nil, err
			}
			if t == nil {
				return nil, fmt.Errorf("%w: %s", storage.ErrTaskNotFound, taskID)
			}
			return t, nil
		})
	}
	task, err := poll() // learns the channel to watch
	if err != nil {
		return nil, err
	}
	return waitForTerminal(ctx, TasksChannel(task.Channel), timeout, poll)
}

// WaitAnyTask blocks until any task on channel reaches a terminal state
// and returns it (earliest done, else earliest failed).
func WaitAnyTask(ctx *WorkflowContext, channel string, timeout time.Duration) (*storage.ActorTask, error) {
	return waitForTerminal(ctx, TasksChannel(channel), timeout, func() (*storage.ActorTask, error) {
		return runStep(ctx, "romancy.find_settled_task", func(ctx *WorkflowContext) (*storage.ActorTask, error) {
			s := ctx.Storage()
			if s == nil {
				return nil, fmt.Errorf("storage not available")
			}
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
		})
	})
}

// waitForTerminal subscribes broadcast on notifyCh, then loops: poll →
// receive until the poll answers a terminal task or the deadline passes.
//
// Everything a decision depends on is recorded, so a replay takes exactly
// the recorded path: polls are steps, and the deadline is fixed once as a
// step — every run waits toward the same instant, and a receive that times
// out has reached it.
func waitForTerminal(ctx *WorkflowContext, notifyCh string, timeout time.Duration,
	poll func() (*storage.ActorTask, error)) (*storage.ActorTask, error) {
	var deadline time.Time
	if timeout > 0 {
		var err error
		deadline, err = runStep(ctx, "romancy.wait_deadline", func(*WorkflowContext) (time.Time, error) {
			return time.Now().UTC().Add(timeout), nil
		})
		if err != nil {
			return nil, err
		}
	}
	// Subscribe before the first poll: a notice published after the poll is
	// then behind the subscription's cursor and wakes the receive.
	if err := Subscribe(ctx, notifyCh, ModeBroadcast); err != nil {
		return nil, err
	}
	var last *storage.ActorTask
	for {
		task, err := poll()
		if err != nil {
			return nil, err
		}
		if task != nil {
			last = task
			if task.State == "done" || task.State == "failed" {
				return task, nil
			}
		}
		var opts []ReceiveOption
		if timeout > 0 {
			// A deadline already behind us still goes through a (minimal)
			// timed receive, so the timeout is a recorded outcome too.
			opts = append(opts, WithReceiveTimeout(max(time.Until(deadline), time.Millisecond)))
		}
		if _, err := Receive[json.RawMessage](ctx, notifyCh, opts...); err != nil {
			var timeoutErr *ChannelMessageTimeoutError
			if errors.As(err, &timeoutErr) {
				return last, err
			}
			return nil, err
		}
	}
}

// ReadChannelSince returns messages on channel published after the given
// message_id cursor ("" reads all, oldest first). Use the last message's
// MessageID as the next cursor. A cursor that names no message on the
// channel any more (consumed or cleaned up) fails with ErrCursorNotFound
// instead of reading from the beginning again; ReadChannelAfter, which
// takes the numeric ID, keeps working then. A channel sealed for
// channel_sub requires WithReceiveCapability, as Receive does.
func ReadChannelSince(ctx *WorkflowContext, channel, messageID string, opts ...ReceiveOption) ([]*storage.ChannelMessage, error) {
	options := &receiveOptions{}
	for _, opt := range opts {
		opt(options)
	}
	return runStep(ctx, "romancy.read_channel_since", func(ctx *WorkflowContext) ([]*storage.ChannelMessage, error) {
		s := ctx.Storage()
		if s == nil {
			return nil, fmt.Errorf("storage not available")
		}
		if err := enforceChannelCapability(ctx, storage.CapabilityKindChannelSub, channel, options.capabilityToken); err != nil {
			return nil, err
		}
		if err := enforceDirectPrivacy(ctx, channel); err != nil {
			return nil, err
		}
		return s.ChannelMessagesSince(ctx.Context(), channel, messageID)
	})
}

// ReadChannelAfter returns the messages on channel whose ID (the numeric
// ChannelMessage.ID, the row sequence) is larger than afterID, in ID order;
// 0 reads from the beginning. Unlike ReadChannelSince it keeps working after
// the cursor message was consumed or cleaned up: pass the ID of the last
// message read. Sealing and recording are those of ReadChannelSince.
func ReadChannelAfter(ctx *WorkflowContext, channel string, afterID int64, opts ...ReceiveOption) ([]*storage.ChannelMessage, error) {
	options := &receiveOptions{}
	for _, opt := range opts {
		opt(options)
	}
	return runStep(ctx, "romancy.read_channel_after", func(ctx *WorkflowContext) ([]*storage.ChannelMessage, error) {
		s := ctx.Storage()
		if s == nil {
			return nil, fmt.Errorf("storage not available")
		}
		if afterID < 0 {
			return nil, fmt.Errorf("%w: negative cursor %d", ErrInvalidArgument, afterID)
		}
		if err := enforceChannelCapability(ctx, storage.CapabilityKindChannelSub, channel, options.capabilityToken); err != nil {
			return nil, err
		}
		if err := enforceDirectPrivacy(ctx, channel); err != nil {
			return nil, err
		}
		return s.ChannelMessagesAfter(ctx.Context(), channel, afterID)
	})
}
