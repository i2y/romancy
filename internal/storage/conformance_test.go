package storage

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// Cross-dialect behavior that the actor-OS kernel relies on (see
// schema/SEMANTICS.md). Every test here runs on SQLite, and on PostgreSQL
// and MySQL under -tags integration.

// SEMANTICS §3b: sandbox state is inherited from a spawn parent or a
// continuation source and can never be cleared.
func TestStorage_SandboxedInheritance(t *testing.T) {
	forEachBackend(t, func(t *testing.T, ctx context.Context, s *testDB) {
		mk := func(inst *WorkflowInstance) error {
			inst.WorkflowName = "wf"
			inst.Status = StatusRunning
			inst.InputData = []byte(`{}`)
			inst.StartedAt = time.Now()
			inst.UpdatedAt = inst.StartedAt
			return s.CreateInstance(ctx, inst)
		}
		sandboxed := func(id string) bool {
			t.Helper()
			inst, err := s.GetInstance(ctx, id)
			if err != nil || inst == nil {
				t.Fatalf("get %s: %v, %v", id, inst, err)
			}
			return inst.Sandboxed
		}

		if err := mk(&WorkflowInstance{InstanceID: "root"}); err != nil {
			t.Fatal(err)
		}
		if err := mk(&WorkflowInstance{InstanceID: "box", ParentInstanceID: "root", Sandboxed: true}); err != nil {
			t.Fatal(err)
		}
		if err := mk(&WorkflowInstance{InstanceID: "box-child", ParentInstanceID: "box"}); err != nil {
			t.Fatal(err)
		}
		if err := mk(&WorkflowInstance{InstanceID: "box-next", ContinuedFrom: "box"}); err != nil {
			t.Fatal(err)
		}
		if err := mk(&WorkflowInstance{InstanceID: "free-child", ParentInstanceID: "root"}); err != nil {
			t.Fatal(err)
		}
		if sandboxed("root") || !sandboxed("box") || !sandboxed("box-child") || !sandboxed("box-next") || sandboxed("free-child") {
			t.Fatalf("sandboxed: root=%v box=%v box-child=%v box-next=%v free-child=%v",
				sandboxed("root"), sandboxed("box"), sandboxed("box-child"), sandboxed("box-next"), sandboxed("free-child"))
		}
		if err := mk(&WorkflowInstance{InstanceID: "orphan", ParentInstanceID: "no-such-parent"}); err == nil {
			t.Fatal("creating a child of a missing parent should fail")
		}
	})
}

// SEMANTICS §1: fenced writes succeed for the holder — including writes that
// leave the row unchanged (MySQL reports 0 affected rows for those unless
// the connection uses clientFoundRows) — and fail for anyone else.
func TestStorage_FencedWrites(t *testing.T) {
	forEachBackend(t, func(t *testing.T, ctx context.Context, s *testDB) {
		createTestInstance(t, ctx, s, "inst-f", "wf")
		holder := NewLockIdentity("worker-a")
		other := NewLockIdentity("worker-a") // same worker, different epoch

		if ok, err := s.TryAcquireLock(ctx, "inst-f", holder, 300); err != nil || !ok {
			t.Fatalf("acquire = %v, %v", ok, err)
		}
		if ok, err := s.TryAcquireLock(ctx, "inst-f", other, 300); err != nil || ok {
			t.Fatalf("second epoch acquire = %v, %v; want false", ok, err)
		}
		// Same status twice in quick succession: the second write changes nothing.
		for i := range 2 {
			if err := s.UpdateInstanceStatusIfLocked(ctx, "inst-f", StatusRunning, "", holder); err != nil {
				t.Fatalf("holder status write %d: %v", i, err)
			}
		}
		if err := s.RefreshLock(ctx, "inst-f", holder, 300); err != nil {
			t.Fatalf("refresh: %v", err)
		}
		if err := s.RefreshLock(ctx, "inst-f", holder, 300); err != nil {
			t.Fatalf("immediate re-refresh: %v", err)
		}
		if err := s.UpdateInstanceStatusIfLocked(ctx, "inst-f", StatusFailed, "", other); !errors.Is(err, ErrLockNotHeld) {
			t.Fatalf("stale-epoch status write = %v; want ErrLockNotHeld", err)
		}
		if err := s.RefreshLock(ctx, "inst-f", other, 300); !errors.Is(err, ErrLockNotHeld) {
			t.Fatalf("stale-epoch refresh = %v; want ErrLockNotHeld", err)
		}
		ev := &HistoryEvent{InstanceID: "inst-f", ActivityID: "a:1", EventType: HistoryActivityCompleted,
			EventData: []byte(`{"r":1}`), DataType: "json"}
		if err := s.AppendHistoryIfLocked(ctx, ev, other); !errors.Is(err, ErrLockNotHeld) {
			t.Fatalf("stale-epoch append = %v; want ErrLockNotHeld", err)
		}
		if err := s.AppendHistoryIfLocked(ctx, ev, holder); err != nil {
			t.Fatalf("holder append: %v", err)
		}
		if err := s.AppendHistoryIfLocked(ctx, ev, holder); err != nil {
			t.Fatalf("duplicate holder append should be a no-op: %v", err)
		}
		if err := s.UpdateInstanceInputIfLocked(ctx, "inst-f", []byte(`{"n":2}`), holder); err != nil {
			t.Fatalf("holder input write: %v", err)
		}
		if err := s.UpdateInstanceInputIfLocked(ctx, "inst-f", []byte(`{"n":2}`), holder); err != nil {
			t.Fatalf("holder identical input write: %v", err)
		}
		if err := s.UpdateInstanceOutputIfLocked(ctx, "inst-f", []byte(`{"done":true}`), other); !errors.Is(err, ErrLockNotHeld) {
			t.Fatalf("stale-epoch output write = %v; want ErrLockNotHeld", err)
		}
		if err := s.UpdateInstanceOutputIfLocked(ctx, "inst-f", []byte(`{"done":true}`), holder); err != nil {
			t.Fatalf("holder output write: %v", err)
		}
		inst, err := s.GetInstance(ctx, "inst-f")
		if err != nil || inst.Status != StatusCompleted || string(inst.OutputData) != `{"done":true}` {
			t.Fatalf("instance = %+v, %v", inst, err)
		}
	})
}

// SEMANTICS §5: archiving history (Recur) must work against the real
// schema and keep the events, in order.
func TestStorage_ArchiveHistory(t *testing.T) {
	forEachBackend(t, func(t *testing.T, ctx context.Context, s *testDB) {
		createTestInstance(t, ctx, s, "inst-r", "wf")
		for _, id := range []string{"step:1", "step:2", "step:3"} {
			if err := s.AppendHistory(ctx, &HistoryEvent{InstanceID: "inst-r", ActivityID: id,
				EventType: HistoryActivityCompleted, EventData: []byte(`"` + id + `"`), DataType: "json"}); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.ArchiveHistory(ctx, "inst-r"); err != nil {
			t.Fatalf("archive: %v", err)
		}
		if live, _, err := s.GetHistoryPaginated(ctx, "inst-r", 0, 100); err != nil || len(live) != 0 {
			t.Fatalf("live history after archive = %d, %v", len(live), err)
		}
		arch, err := s.GetArchivedHistory(ctx, "inst-r")
		if err != nil || len(arch) != 3 {
			t.Fatalf("archived = %d, %v", len(arch), err)
		}
		for i, want := range []string{"step:1", "step:2", "step:3"} {
			if arch[i].ActivityID != want || string(arch[i].EventData) != `"`+want+`"` {
				t.Fatalf("archived[%d] = %+v", i, arch[i])
			}
		}
		// a second generation archives alongside the first
		if err := s.AppendHistory(ctx, &HistoryEvent{InstanceID: "inst-r", ActivityID: "step:1",
			EventType: HistoryActivityCompleted, EventData: []byte(`"gen2"`), DataType: "json"}); err != nil {
			t.Fatal(err)
		}
		if err := s.ArchiveHistory(ctx, "inst-r"); err != nil {
			t.Fatalf("archive gen2: %v", err)
		}
		if arch, err = s.GetArchivedHistory(ctx, "inst-r"); err != nil || len(arch) != 4 {
			t.Fatalf("archived gen2 = %d, %v", len(arch), err)
		}
	})
}

// SEMANTICS §6: registration is idempotent for the holder; the name stays
// exclusive against everyone else.
func TestStorage_RegisterActorNameIdempotent(t *testing.T) {
	forEachBackend(t, func(t *testing.T, ctx context.Context, s *testDB) {
		createTestInstance(t, ctx, s, "wf-n1", "wf")
		createTestInstance(t, ctx, s, "wf-n2", "wf")
		for i := range 2 {
			if err := s.RegisterActorName(ctx, "svc", "wf-n1", "go"); err != nil {
				t.Fatalf("register #%d by holder: %v", i, err)
			}
		}
		if err := s.RegisterActorName(ctx, "svc", "wf-n2", "go"); !errors.Is(err, ErrActorNameTaken) {
			t.Fatalf("register by other = %v; want ErrActorNameTaken", err)
		}
		if id, err := s.WhereisActor(ctx, "svc"); err != nil || id != "wf-n1" {
			t.Fatalf("whereis = %q, %v", id, err)
		}
	})
}

// SEMANTICS §5/§6: a recurred instance is alive — no DOWN, name kept — and a
// terminated actor's name is freed even when nobody monitored it.
func TestStorage_DownSweepLiveness(t *testing.T) {
	forEachBackend(t, func(t *testing.T, ctx context.Context, s *testDB) {
		for _, id := range []string{"wf-watch", "wf-loop", "wf-gone"} {
			createTestInstance(t, ctx, s, id, "wf")
		}
		if err := s.RegisterActorName(ctx, "looper", "wf-loop", "go"); err != nil {
			t.Fatal(err)
		}
		if err := s.RegisterActorName(ctx, "goner", "wf-gone", "go"); err != nil {
			t.Fatal(err)
		}
		if err := s.MonitorActor(ctx, "wf-watch", "wf-loop", "mon:wf-watch"); err != nil {
			t.Fatal(err)
		}
		s.exec(t, ctx, `UPDATE workflow_instances SET status = 'recurred' WHERE instance_id = ?`, "wf-loop")
		s.exec(t, ctx, `UPDATE workflow_instances SET status = 'completed' WHERE instance_id = ?`, "wf-gone")

		if n, err := s.EmitActorDownForTerminated(ctx); err != nil || n != 0 {
			t.Fatalf("emit = %d, %v; want 0 (recurred is alive)", n, err)
		}
		if id, _ := s.WhereisActor(ctx, "looper"); id != "wf-loop" {
			t.Fatalf("recurred actor lost its name: whereis = %q", id)
		}
		if id, _ := s.WhereisActor(ctx, "goner"); id != "" {
			t.Fatalf("unmonitored terminated actor keeps its name: whereis = %q", id)
		}
		// the real termination is still reported
		s.exec(t, ctx, `UPDATE workflow_instances SET status = 'failed' WHERE instance_id = ?`, "wf-loop")
		if n, err := s.EmitActorDownForTerminated(ctx); err != nil || n != 1 {
			t.Fatalf("emit after failure = %d, %v; want 1", n, err)
		}
		data := s.queryString(t, ctx, `SELECT data FROM channel_messages WHERE channel = ?`, "mon:wf-watch")
		if want := `{"type":"DOWN","actor":"wf-loop","status":"failed"}`; data != want {
			t.Fatalf("down payload = %q, want %q", data, want)
		}
	})
}

// SEMANTICS §4: settling a task is idempotent for its claimant (a step
// re-executed after a crash) and a conflict for anyone else.
func TestStorage_CompleteTaskIdempotent(t *testing.T) {
	forEachBackend(t, func(t *testing.T, ctx context.Context, s *testDB) {
		createTestInstance(t, ctx, s, "wf-c1", "wf")
		if err := s.PostTask(ctx, &ActorTask{ID: "tc1", Channel: `q"\x`, Data: []byte(`{}`), State: "open"}); err != nil {
			t.Fatal(err)
		}
		if task, err := s.ClaimTask(ctx, `q"\x`, "wf-c1"); err != nil || task == nil || task.ID != "tc1" {
			t.Fatalf("claim = %+v, %v", task, err)
		}
		for i := range 2 {
			if err := s.CompleteTask(ctx, "tc1", "wf-c1", []byte(`{"ok":true}`), false); err != nil {
				t.Fatalf("complete #%d: %v", i, err)
			}
		}
		if err := s.CompleteTask(ctx, "tc1", "wf-c1", nil, true); !errors.Is(err, ErrTaskConflict) {
			t.Fatalf("fail after done = %v; want ErrTaskConflict", err)
		}
		if err := s.CompleteTask(ctx, "tc1", "wf-other", nil, false); !errors.Is(err, ErrTaskConflict) {
			t.Fatalf("complete by other = %v; want ErrTaskConflict", err)
		}
		if err := s.CompleteTask(ctx, "no-such-task", "wf-c1", nil, false); !errors.Is(err, ErrTaskNotFound) {
			t.Fatalf("complete missing = %v; want ErrTaskNotFound", err)
		}
		// lifecycle notices are valid JSON even for awkward channel names
		rows, err := s.db.QueryContext(ctx, s.rebind(`SELECT data FROM channel_messages WHERE channel = ?`), `q"\x:tasks`)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		count := 0
		for rows.Next() {
			var data string
			if err := rows.Scan(&data); err != nil {
				t.Fatal(err)
			}
			var v map[string]any
			if err := json.Unmarshal([]byte(data), &v); err != nil {
				t.Fatalf("notice %q is not JSON: %v", data, err)
			}
			count++
		}
		if count != 3 { // posted, claimed, state
			t.Fatalf("notices = %d; want 3", count)
		}
	})
}

func mustAcquire(t *testing.T, ctx context.Context, s Storage, instanceID, holder string) {
	t.Helper()
	if ok, err := s.TryAcquireLock(ctx, instanceID, holder, 300); err != nil || !ok {
		t.Fatalf("acquire %s as %s = %v, %v", instanceID, holder, ok, err)
	}
}

// waitOn suspends instanceID in Receive on channel as the engine does and
// returns with the lock released.
func waitOn(t *testing.T, ctx context.Context, s Storage, instanceID, channel string, mode ChannelMode, deadline *time.Time) {
	t.Helper()
	if err := s.SubscribeToChannel(ctx, instanceID, channel, mode); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	holder := NewLockIdentity("w")
	mustAcquire(t, ctx, s, instanceID, holder)
	registered, err := s.RegisterChannelReceiveAndReleaseLock(ctx, instanceID, channel, holder, "receive_"+channel+":1", deadline)
	if err != nil || !registered {
		t.Fatalf("register wait = %v, %v", registered, err)
	}
}

// SEMANTICS §3: a message is delivered only into a registered wait. A
// subscriber that is not waiting keeps the message pending for its next
// Receive: it is neither recorded under a history id no Receive reads nor,
// in competing mode, consumed.
func TestStorage_DeliverOnlyIntoWaits(t *testing.T) {
	forEachBackend(t, func(t *testing.T, ctx context.Context, s *testDB) {
		for _, mode := range []ChannelMode{ChannelModeCompeting, ChannelModeBroadcast} {
			idle, waiting, ch := "idle-"+string(mode), "waiting-"+string(mode), "dw-"+string(mode)
			createTestInstance(t, ctx, s, idle, "wf")
			createTestInstance(t, ctx, s, waiting, "wf")
			if err := s.SubscribeToChannel(ctx, idle, ch, mode); err != nil {
				t.Fatal(err)
			}
			waitOn(t, ctx, s, waiting, ch, mode, nil)
			msgID, err := s.PublishToChannel(ctx, ch, []byte(`{"x":1}`), nil)
			if err != nil {
				t.Fatal(err)
			}
			msg := &ChannelMessage{ID: msgID, Channel: ch, Data: []byte(`{"x":1}`)}

			res, err := s.DeliverChannelMessageWithLock(ctx, idle, ch, msg, NewLockIdentity("w"), 300)
			if err != nil || res != nil {
				t.Fatalf("%s: delivery to a non-waiting subscriber = %+v, %v; want skipped", mode, res, err)
			}
			if n := s.queryString(t, ctx, `SELECT COUNT(*) FROM workflow_history WHERE instance_id = ?`, idle); n != "0" {
				t.Fatalf("%s: non-waiting subscriber got %s history rows", mode, n)
			}
			if holder := s.queryString(t, ctx, `SELECT COALESCE(locked_by, '') FROM workflow_instances WHERE instance_id = ?`, idle); holder != "" {
				t.Fatalf("%s: skipped delivery left the lock held by %q", mode, holder)
			}
			if pending, err := s.GetPendingChannelMessagesForInstance(ctx, idle, ch); err != nil || len(pending) != 1 {
				t.Fatalf("%s: pending for the idle subscriber = %d, %v; want the message still there", mode, len(pending), err)
			}

			res, err = s.DeliverChannelMessageWithLock(ctx, waiting, ch, msg, NewLockIdentity("w"), 300)
			if err != nil || res == nil {
				t.Fatalf("%s: delivery into the wait = %+v, %v", mode, res, err)
			}
			if n := s.queryString(t, ctx, `SELECT COUNT(*) FROM workflow_history WHERE instance_id = ? AND activity_id = ?`, waiting, "receive_"+ch+":1"); n != "1" {
				t.Fatalf("%s: wait got %s history rows; want 1", mode, n)
			}
			if inst, _ := s.GetInstance(ctx, waiting); inst.Status != StatusRunning {
				t.Fatalf("%s: delivered instance is %s; want running", mode, inst.Status)
			}
		}
	})
}

// SEMANTICS §1/§3: wait registration is a fenced write. A run that lost its
// lease can neither suspend the instance nor overwrite the live run's wait.
func TestStorage_WaitRegistrationFenced(t *testing.T) {
	forEachBackend(t, func(t *testing.T, ctx context.Context, s *testDB) {
		createTestInstance(t, ctx, s, "inst-wr", "wf")
		if err := s.SubscribeToChannel(ctx, "inst-wr", "wr", ChannelModeBroadcast); err != nil {
			t.Fatal(err)
		}
		stale, live := NewLockIdentity("w"), NewLockIdentity("w")
		mustAcquire(t, ctx, s, "inst-wr", stale)
		if err := s.ReleaseLock(ctx, "inst-wr", stale); err != nil {
			t.Fatal(err)
		}
		mustAcquire(t, ctx, s, "inst-wr", live)

		if _, err := s.RegisterChannelReceiveAndReleaseLock(ctx, "inst-wr", "wr", stale, "receive_wr:9", nil); !errors.Is(err, ErrLockNotHeld) {
			t.Fatalf("stale registration = %v; want ErrLockNotHeld", err)
		}
		if sub, err := s.GetChannelSubscription(ctx, "inst-wr", "wr"); err != nil || sub.ActivityID != "" {
			t.Fatalf("stale registration wrote the wait: %+v, %v", sub, err)
		}
		if inst, _ := s.GetInstance(ctx, "inst-wr"); inst.Status != StatusRunning || inst.LockedBy != live {
			t.Fatalf("instance = %s locked by %q; want running, held by the live run", inst.Status, inst.LockedBy)
		}
		if registered, err := s.RegisterChannelReceiveAndReleaseLock(ctx, "inst-wr", "wr", live, "receive_wr:1", nil); err != nil || !registered {
			t.Fatalf("live registration = %v, %v", registered, err)
		}
	})
}

// Terminal statuses are absorbing. A cancel that lands while a run holds the
// lock (the supervisor cancels without taking it) is not undone by that
// run's later status writes, and cancelling drops the instance's waits so
// no timer or receive timeout fires for it afterwards.
func TestStorage_TerminalStatusAbsorbing(t *testing.T) {
	forEachBackend(t, func(t *testing.T, ctx context.Context, s *testDB) {
		createTestInstance(t, ctx, s, "inst-ab", "wf")
		deadline := time.Now().Add(time.Hour)
		waitOn(t, ctx, s, "inst-ab", "ab", ChannelModeBroadcast, &deadline)
		if err := s.RegisterTimerSubscription(ctx, &TimerSubscription{InstanceID: "inst-ab", TimerID: "sleep:1",
			ExpiresAt: time.Now().Add(time.Hour), ActivityID: "sleep:1"}); err != nil {
			t.Fatal(err)
		}
		run := NewLockIdentity("w")
		mustAcquire(t, ctx, s, "inst-ab", run)

		if err := s.CancelInstance(ctx, "inst-ab", "test"); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		if n := s.queryString(t, ctx, `SELECT COUNT(*) FROM workflow_timer_subscriptions WHERE instance_id = ?`, "inst-ab"); n != "0" {
			t.Fatalf("cancel left %s timers", n)
		}
		if sub, err := s.GetChannelSubscription(ctx, "inst-ab", "ab"); err != nil || sub.ActivityID != "" || sub.TimeoutAt != nil {
			t.Fatalf("cancel left the channel wait: %+v, %v", sub, err)
		}
		for _, st := range []WorkflowStatus{StatusRunning, StatusWaitingForTimer, StatusCompleted} {
			if err := s.UpdateInstanceStatusIfLocked(ctx, "inst-ab", st, "", run); !errors.Is(err, ErrInstanceTerminated) {
				t.Fatalf("holder write %s over cancelled = %v; want ErrInstanceTerminated", st, err)
			}
		}
		// Rewriting the status it already has is a no-op, not a refusal (the
		// engine writes the output, which completes, then the status).
		if err := s.UpdateInstanceStatusIfLocked(ctx, "inst-ab", StatusCancelled, "", run); err != nil {
			t.Fatalf("holder rewrite of the same terminal status = %v; want nil", err)
		}
		// Completing writes the output and the status in one statement: over
		// a cancelled instance it is refused, and no output is written.
		if err := s.UpdateInstanceOutputIfLocked(ctx, "inst-ab", []byte(`"late"`), run); !errors.Is(err, ErrInstanceTerminated) {
			t.Fatalf("holder output write over cancelled = %v; want ErrInstanceTerminated", err)
		}
		if inst, _ := s.GetInstance(ctx, "inst-ab"); len(inst.OutputData) != 0 {
			t.Fatalf("output over cancelled = %s; want none", inst.OutputData)
		}
		if _, err := s.RegisterChannelReceiveAndReleaseLock(ctx, "inst-ab", "ab", run, "receive_ab:2", nil); !errors.Is(err, ErrInstanceTerminated) {
			t.Fatalf("wait registration over cancelled = %v; want ErrInstanceTerminated", err)
		}
		if err := s.UpdateInstanceStatusIfLocked(ctx, "inst-ab", StatusRunning, "", NewLockIdentity("w")); !errors.Is(err, ErrLockNotHeld) {
			t.Fatalf("non-holder write = %v; want ErrLockNotHeld", err)
		}
		if inst, _ := s.GetInstance(ctx, "inst-ab"); inst.Status != StatusCancelled {
			t.Fatalf("status = %s; want cancelled", inst.Status)
		}
	})
}

// The requeue backstop flips a stranded wait back to running and clears it,
// so its stale deadline cannot fire a timeout into the resumed instance. The
// competing-mode check reads workflow_instances, which MySQL refuses inside
// an UPDATE of that table (error 1093).
func TestStorage_RequeueClearsWait(t *testing.T) {
	forEachBackend(t, func(t *testing.T, ctx context.Context, s *testDB) {
		modes := []ChannelMode{ChannelModeCompeting, ChannelModeBroadcast}
		for _, mode := range modes {
			inst, ch := "inst-rq-"+string(mode), "rq-"+string(mode)
			createTestInstance(t, ctx, s, inst, "wf")
			deadline := time.Now().Add(time.Hour)
			waitOn(t, ctx, s, inst, ch, mode, &deadline)
			// Published after the registration, and the delivery was missed.
			if _, err := s.PublishToChannel(ctx, ch, []byte(`{"x":1}`), nil); err != nil {
				t.Fatal(err)
			}
		}
		n, err := s.RequeueChannelWaitsWithPending(ctx)
		if err != nil || n != len(modes) {
			t.Fatalf("requeue = %d, %v; want %d", n, err, len(modes))
		}
		for _, mode := range modes {
			inst, ch := "inst-rq-"+string(mode), "rq-"+string(mode)
			if got, _ := s.GetInstance(ctx, inst); got.Status != StatusRunning {
				t.Fatalf("%s: status = %s; want running", mode, got.Status)
			}
			if sub, err := s.GetChannelSubscription(ctx, inst, ch); err != nil || sub.ActivityID != "" || sub.TimeoutAt != nil {
				t.Fatalf("%s: requeue left the wait: %+v, %v", mode, sub, err)
			}
		}
		if n, err := s.RequeueChannelWaitsWithPending(ctx); err != nil || n != 0 {
			t.Fatalf("second requeue = %d, %v; want 0", n, err)
		}
	})
}

// A fenced history append is atomic with a lease steal (SEMANTICS §1): while
// the old holder's append is uncommitted the steal waits, so the new holder
// loads a history that already contains the record — or the append, having
// waited for a steal, is refused. A lock check that merely reads a snapshot
// lets the steal through, and the old run's record lands afterwards.
func TestStorage_FencedAppendOrdersSteal(t *testing.T) {
	forEachBackend(t, func(t *testing.T, ctx context.Context, d *testDB) {
		s := d.Storage
		createTestInstance(t, ctx, s, "fa", "wf")
		mustAcquire(t, ctx, s, "fa", "old#1")
		d.exec(t, ctx, `UPDATE workflow_instances SET lock_expires_at = '2000-01-01 00:00:00' WHERE instance_id = ?`, "fa")

		txCtx, err := s.BeginTransaction(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.AppendHistoryIfLocked(txCtx, &HistoryEvent{
			InstanceID: "fa", ActivityID: "act:1", EventType: HistoryActivityCompleted,
			EventData: []byte(`1`), DataType: "json",
		}, "old#1"); err != nil {
			_ = s.RollbackTransaction(txCtx)
			t.Fatalf("append as the holder: %v", err)
		}
		stolen := make(chan bool, 1)
		go func() {
			ok, err := s.TryAcquireLock(ctx, "fa", "new#1", 300)
			if err != nil {
				t.Errorf("steal: %v", err)
			}
			stolen <- ok
		}()
		select {
		case <-stolen:
			_ = s.RollbackTransaction(txCtx)
			t.Fatal("the lease was stolen while the old holder's fenced append was uncommitted")
		case <-time.After(300 * time.Millisecond):
		}
		if err := s.CommitTransaction(txCtx); err != nil {
			t.Fatal(err)
		}
		if !<-stolen {
			t.Fatal("the steal did not go through once the append committed")
		}
		if n, err := s.GetHistoryCount(ctx, "fa"); err != nil || n != 1 {
			t.Fatalf("history count = %d, %v; want the old holder's record", n, err)
		}
	})
}

// SEMANTICS §3: a message_id cursor that names no message on the channel
// is an error, not "read from the beginning"; the numeric cursor keeps
// working after the cursor message is gone.
func TestStorage_CursorReads(t *testing.T) {
	forEachBackend(t, func(t *testing.T, ctx context.Context, s *testDB) {
		var ids []int64
		for _, v := range []string{`"a"`, `"b"`, `"c"`} {
			id, err := s.PublishToChannel(ctx, "cur", []byte(v), nil)
			if err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		if _, err := s.PublishToChannel(ctx, "cur-other", []byte(`"x"`), nil); err != nil {
			t.Fatal(err)
		}
		all, err := s.ChannelMessagesSince(ctx, "cur", "")
		if err != nil || len(all) != 3 {
			t.Fatalf("since \"\" = %d messages, %v; want 3", len(all), err)
		}
		since, err := s.ChannelMessagesSince(ctx, "cur", all[0].MessageID)
		if err != nil || len(since) != 2 || since[0].ID != ids[1] {
			t.Fatalf("since first = %+v, %v; want the last two", since, err)
		}
		if _, err := s.ChannelMessagesSince(ctx, "cur", "no-such-message"); !errors.Is(err, ErrCursorNotFound) {
			t.Fatalf("unknown cursor = %v; want ErrCursorNotFound", err)
		}
		other, err := s.ChannelMessagesSince(ctx, "cur-other", "")
		if err != nil || len(other) != 1 {
			t.Fatalf("other channel = %d, %v", len(other), err)
		}
		if _, err := s.ChannelMessagesSince(ctx, "cur", other[0].MessageID); !errors.Is(err, ErrCursorNotFound) {
			t.Fatalf("a cursor from another channel = %v; want ErrCursorNotFound", err)
		}

		if err := s.DeleteChannelMessage(ctx, ids[0]); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ChannelMessagesSince(ctx, "cur", all[0].MessageID); !errors.Is(err, ErrCursorNotFound) {
			t.Fatalf("deleted cursor = %v; want ErrCursorNotFound", err)
		}
		after, err := s.ChannelMessagesAfter(ctx, "cur", ids[0])
		if err != nil || len(after) != 2 || after[0].ID != ids[1] || after[1].ID != ids[2] {
			t.Fatalf("after the deleted cursor = %+v, %v; want the last two", after, err)
		}
		if after[0].MessageID == "" {
			t.Fatal("messages read by id carry no message_id")
		}
		if rest, err := s.ChannelMessagesAfter(ctx, "cur", 0); err != nil || len(rest) != 2 {
			t.Fatalf("after 0 = %d, %v; want 2", len(rest), err)
		}
	})
}

// SEMANTICS §1: a recur drops the instance's compensations.
func TestStorage_ClearCompensations(t *testing.T) {
	forEachBackend(t, func(t *testing.T, ctx context.Context, s *testDB) {
		createTestInstance(t, ctx, s, "comp-a", "wf")
		createTestInstance(t, ctx, s, "comp-b", "wf")
		for _, id := range []string{"comp-a", "comp-a", "comp-b"} {
			if err := s.AddCompensation(ctx, &CompensationEntry{
				InstanceID: id, ActivityID: "act:1", ActivityName: "act", Args: []byte(`{}`),
			}); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.ClearCompensations(ctx, "comp-a"); err != nil {
			t.Fatal(err)
		}
		a, _ := s.GetCompensations(ctx, "comp-a")
		b, _ := s.GetCompensations(ctx, "comp-b")
		if len(a) != 0 || len(b) != 1 {
			t.Fatalf("compensations after clearing comp-a: a=%d b=%d; want 0 and 1", len(a), len(b))
		}
	})
}

// A claim takes only instances of the code version the worker runs.
func TestStorage_ClaimByVersion(t *testing.T) {
	forEachBackend(t, func(t *testing.T, ctx context.Context, s *testDB) {
		for id, hash := range map[string]string{"cv-1": "hash-1", "cv-2": "hash-2", "cv-o": "hash-o"} {
			name := "wf-v"
			if id == "cv-o" {
				name = "wf-o"
			}
			now := time.Now()
			if err := s.CreateInstance(ctx, &WorkflowInstance{
				InstanceID: id, WorkflowName: name, SourceHash: hash, Status: StatusRunning,
				InputData: []byte(`{}`), StartedAt: now, UpdatedAt: now,
			}); err != nil {
				t.Fatal(err)
			}
		}
		got, err := s.ClaimResumableWorkflows(ctx, "w1#1",
			[]WorkflowVersion{{Name: "wf-v", SourceHash: "hash-1"}, {Name: "wf-o"}}, 300, 10)
		if err != nil {
			t.Fatal(err)
		}
		claimed := map[string]bool{}
		for _, wf := range got {
			claimed[wf.InstanceID] = true
		}
		if len(got) != 2 || !claimed["cv-1"] || !claimed["cv-o"] {
			t.Fatalf("claimed %v; want cv-1 (its version) and cv-o (any version), not cv-2", claimed)
		}
	})
}

// SEMANTICS §3a: the expiry sweep keeps each channel sealed — it deletes an
// expired grant only when an unexpired or a newer grant of the same
// (kind, target) remains (newer: created_at, then id).
func TestStorage_ExpirySweepKeepsSeal(t *testing.T) {
	forEachBackend(t, func(t *testing.T, ctx context.Context, s *testDB) {
		past := time.Now().Add(-time.Hour)
		future := time.Now().Add(time.Hour)
		grant := func(id, target string, expires *time.Time) {
			t.Helper()
			if err := s.GrantCapability(ctx, &ActorCapability{
				ID: id, Kind: CapabilityKindChannelPub, Target: target, SecretHash: "h-" + id, ExpiresAt: expires,
			}); err != nil {
				t.Fatal(err)
			}
		}
		grant("cap-old", "t1", &past)
		grant("cap-new", "t1", &past)
		grant("cap-expired", "t2", &past)
		grant("cap-live", "t2", &future)
		grant("cap-tie-a", "t3", &past)
		grant("cap-tie-b", "t3", &past)
		grant("cap-only", "t4", &past)
		older := canonicalTS(time.Now().Add(-2 * time.Hour))
		s.exec(t, ctx, `UPDATE actor_capabilities SET created_at = ? WHERE id = ?`, older, "cap-old")
		tie := canonicalTS(time.Now().Add(-3 * time.Hour))
		s.exec(t, ctx, `UPDATE actor_capabilities SET created_at = ? WHERE id IN (?, ?)`, tie, "cap-tie-a", "cap-tie-b")

		n, err := s.DeleteExpiredCapabilities(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if n != 3 {
			t.Fatalf("deleted %d grants; want 3 (cap-old, cap-expired, cap-tie-a)", n)
		}
		rows, err := s.db.QueryContext(ctx, `SELECT id FROM actor_capabilities ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		var left []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			left = append(left, id)
		}
		want := []string{"cap-live", "cap-new", "cap-only", "cap-tie-b"}
		if strings.Join(left, ",") != strings.Join(want, ",") {
			t.Fatalf("grants left = %v; want %v", left, want)
		}
	})
}

// Columns that hold an instance ID take any ID workflow_instances accepts
// (up to 255 characters): the owner and claimant checks compare the whole
// ID, so a truncated one would refuse the instance that owns the row.
func TestStorage_LongInstanceIDsInActorColumns(t *testing.T) {
	forEachBackend(t, func(t *testing.T, ctx context.Context, s *testDB) {
		long := "inst-" + strings.Repeat("x", 245) // 250 characters
		if err := s.GrantCapability(ctx, &ActorCapability{
			ID: "cap-long", Kind: CapabilityKindChannelPub, Target: "long-ch", SecretHash: "h-long", GrantedBy: long,
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.RevokeOwnCapability(ctx, "cap-long", long); err != nil {
			t.Fatalf("revoke by the granting instance: %v", err)
		}
		if err := s.FSPut(ctx, "/actor/"+long+"/state", []byte("v"), long, 0); err != nil {
			t.Fatal(err)
		}
		obj, err := s.FSGet(ctx, "/actor/"+long+"/state")
		if err != nil || obj == nil || obj.Owner != long {
			t.Fatalf("fs owner = %+v, %v", obj, err)
		}
		if err := s.PostTask(ctx, &ActorTask{ID: "task-long", Channel: "long-tasks", Data: []byte(`{}`)}); err != nil {
			t.Fatal(err)
		}
		task, err := s.ClaimTask(ctx, "long-tasks", long)
		if err != nil || task == nil || task.ClaimedBy != long {
			t.Fatalf("claimed = %+v, %v", task, err)
		}
		if err := s.CompleteTask(ctx, "task-long", long, []byte(`{}`), false); err != nil {
			t.Fatalf("complete by the claimant: %v", err)
		}
		if err := s.PutBlob(ctx, strings.Repeat("a", 64), []byte("b"), long); err != nil {
			t.Fatal(err)
		}
	})
}
