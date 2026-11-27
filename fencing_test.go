package romancy

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/i2y/romancy/internal/storage"
)

// A run that lost its lease leaves no effects behind (SEMANTICS §1): an
// effectful primitive commits its effect together with its record, in a
// transaction that first proves the lease. Here another holder takes the
// instance over mid-run; every effect must then fail and change nothing —
// no name binding, no message, no task, and no claim hiding a queued
// message from other consumers.
func TestFencing_LostLeaseLeavesNoEffects(t *testing.T) {
	app, cleanup := replayTestApp(t)
	defer cleanup()

	type outcome struct{ register, publish, post, receive error }
	results := make(chan outcome, 1)
	var once sync.Once
	wf := DefineWorkflow("fencing_lost_lease", func(ctx *WorkflowContext, _ struct{}) (string, error) {
		if err := Subscribe(ctx, "fence-queue", ModeCompeting); err != nil {
			return "", err
		}
		if err := Publish(ctx, "fence-queue", "job"); err != nil {
			return "", err
		}
		var stealErr error
		once.Do(func() {
			_, stealErr = app.Storage().DB().ExecContext(ctx.Context(),
				`UPDATE workflow_instances SET locked_by = 'thief#1' WHERE instance_id = ?`, ctx.InstanceID())
		})
		if stealErr != nil {
			return "", stealErr
		}
		var o outcome
		o.register = RegisterName(ctx, "fence-name")
		o.publish = Publish(ctx, "fence-pub", "late")
		_, o.post = PostTask(ctx, "fence-tasks", []byte(`{}`))
		_, o.receive = Receive[string](ctx, "fence-queue")
		results <- o
		return "done", nil
	})
	RegisterWorkflow(app, wf)

	ctx := context.Background()
	if _, err := StartWorkflow(ctx, app, wf, struct{}{}); err != nil {
		t.Fatal(err)
	}
	var o outcome
	select {
	case o = <-results:
	case <-time.After(10 * time.Second):
		t.Fatal("workflow did not run")
	}
	for name, err := range map[string]error{
		"RegisterName": o.register, "Publish": o.publish, "PostTask": o.post, "Receive": o.receive,
	} {
		if !errors.Is(err, storage.ErrLockNotHeld) {
			t.Errorf("%s after losing the lease = %v; want ErrLockNotHeld", name, err)
		}
	}

	s := app.Storage()
	if owner, err := s.WhereisActor(ctx, "fence-name"); err != nil || owner != "" {
		t.Errorf("name bound by a run without the lease: %q, %v", owner, err)
	}
	if n := countChannelMessages(t, app, "fence-pub"); n != 0 {
		t.Errorf("a run without the lease published %d messages", n)
	}
	if tasks, err := s.ListTasks(ctx, "fence-tasks", ""); err != nil || len(tasks) != 0 {
		t.Errorf("a run without the lease posted tasks: %d, %v", len(tasks), err)
	}
	var msgID int64
	if err := s.DB().QueryRowContext(ctx,
		`SELECT id FROM channel_messages WHERE channel = 'fence-queue'`).Scan(&msgID); err != nil {
		t.Fatalf("the queued message is gone: %v", err)
	}
	if claimed, err := s.ClaimChannelMessage(ctx, msgID, "other-consumer"); err != nil || !claimed {
		t.Errorf("another consumer cannot claim the queued message (claimed=%v, err=%v): the run without the lease left its claim", claimed, err)
	}
}
