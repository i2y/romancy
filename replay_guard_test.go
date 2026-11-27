package romancy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/i2y/romancy/internal/storage"
)

// replayTestApp starts an app whose background loops run fast enough for a
// workflow to suspend on a short Sleep and be resumed (replayed) quickly.
func replayTestApp(t *testing.T) (*App, func()) {
	t.Helper()
	app, cleanup := createTestApp(t,
		WithWorkflowResumptionInterval(50*time.Millisecond),
		WithTimerCheckInterval(50*time.Millisecond),
		WithEventTimeoutInterval(50*time.Millisecond),
	)
	if err := app.Start(context.Background()); err != nil {
		cleanup()
		t.Fatalf("start app: %v", err)
	}
	return app, cleanup
}

func countChannelMessages(t *testing.T, app *App, channel string) int {
	t.Helper()
	var n int
	if err := app.Storage().DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM channel_messages WHERE channel = ?`, channel).Scan(&n); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	return n
}

// Publish and SendTo called from workflow code must not run again when the
// workflow is replayed after a suspension (SEMANTICS §2).
func TestReplay_PublishAndSendToRunOnce(t *testing.T) {
	app, cleanup := replayTestApp(t)
	defer cleanup()

	wf := DefineWorkflow("replay_publish_once", func(ctx *WorkflowContext, _ struct{}) (string, error) {
		if err := Publish(ctx, "rg-pub", map[string]int{"n": 1}); err != nil {
			return "", err
		}
		if err := SendTo(ctx, "rg-target", "rg-direct", map[string]int{"n": 2}); err != nil {
			return "", err
		}
		if err := Sleep(ctx, 100*time.Millisecond); err != nil {
			return "", err
		}
		return "ok", nil
	})
	RegisterWorkflow(app, wf)

	ctx := context.Background()
	id, err := StartWorkflow(ctx, app, wf, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if !waitForStatus(t, ctx, app, id, storage.StatusCompleted, 10*time.Second) {
		inst, _ := app.GetInstance(ctx, id)
		t.Fatalf("workflow did not complete: %+v", inst)
	}
	if n := countChannelMessages(t, app, "rg-pub"); n != 1 {
		t.Errorf("Publish ran %d times; want 1", n)
	}
	if n := countChannelMessages(t, app, "rg-direct:rg-target"); n != 1 {
		t.Errorf("SendTo ran %d times; want 1", n)
	}
}

type replayOut struct {
	N int    `json:"n"`
	S string `json:"s"`
}

// Activity results loaded from history (a resumed run) come back as the
// activity's own output type, not as generic JSON values.
func TestReplay_TypedActivityResults(t *testing.T) {
	app, cleanup := replayTestApp(t)
	defer cleanup()
	structAct := DefineActivity("replay_struct_act", func(ctx context.Context, in int) (replayOut, error) {
		return replayOut{N: in, S: "x"}, nil
	})
	intAct := DefineActivity("replay_int_act", func(ctx context.Context, in int) (int, error) { return in * 2, nil })
	sliceAct := DefineActivity("replay_slice_act", func(ctx context.Context, in int) ([]int, error) { return []int{in, in}, nil })
	wf := DefineWorkflow("replay_typed_results", func(ctx *WorkflowContext, in int) (int, error) {
		out, err := structAct.Execute(ctx, in)
		if err != nil {
			return 0, err
		}
		n, err := intAct.Execute(ctx, in)
		if err != nil {
			return 0, err
		}
		xs, err := sliceAct.Execute(ctx, in)
		if err != nil {
			return 0, err
		}
		if err := Sleep(ctx, 50*time.Millisecond); err != nil {
			return 0, err
		}
		return out.N + n + len(xs), nil
	})
	RegisterWorkflow(app, wf)

	ctx := context.Background()
	id, err := StartWorkflow(ctx, app, wf, 7)
	if err != nil {
		t.Fatal(err)
	}
	inst := waitForTerminalInstance(t, app, id)
	if inst.Status != storage.StatusCompleted || string(inst.OutputData) != "23" {
		t.Fatalf("instance = %s %s", inst.Status, inst.OutputData)
	}
}

// Registry, monitor and capability primitives called from workflow code are
// recorded steps: replays neither fail on them nor repeat their effects.
func TestReplay_ActorPrimitivesRunOnce(t *testing.T) {
	app, cleanup := replayTestApp(t)
	defer cleanup()
	type result struct {
		Token  string `json:"token"`
		CapID  string `json:"cap_id"`
		Holder string `json:"holder"`
	}
	wf := DefineWorkflow("replay_actor_primitives", func(ctx *WorkflowContext, _ struct{}) (result, error) {
		if err := RegisterName(ctx, "replay-svc-"+ctx.InstanceID()); err != nil {
			return result{}, err
		}
		token, capID, err := IssueCapability(ctx, storage.CapabilityKindChannelPub, "replay-sealed-"+ctx.InstanceID())
		if err != nil {
			return result{}, err
		}
		if err := Monitor(ctx, "replay-nobody", "replay-mon"); err != nil {
			return result{}, err
		}
		if err := Sleep(ctx, 50*time.Millisecond); err != nil {
			return result{}, err
		}
		holder, err := Whereis(ctx, "replay-svc-"+ctx.InstanceID())
		if err != nil {
			return result{}, err
		}
		if err := RevokeCapability(ctx, capID); err != nil {
			return result{}, err
		}
		if err := Sleep(ctx, 50*time.Millisecond); err != nil {
			return result{}, err
		}
		return result{Token: token, CapID: capID, Holder: holder}, nil
	})
	RegisterWorkflow(app, wf)

	ctx := context.Background()
	id, err := StartWorkflow(ctx, app, wf, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	inst := waitForTerminalInstance(t, app, id)
	if inst.Status != storage.StatusCompleted {
		t.Fatalf("instance = %s %s", inst.Status, inst.OutputData)
	}
	var got result
	if err := json.Unmarshal(inst.OutputData, &got); err != nil {
		t.Fatal(err)
	}
	if got.Holder != id {
		t.Errorf("whereis = %q; want %q", got.Holder, id)
	}
	var grants, monitors int
	db := app.Storage().DB()
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM actor_capabilities WHERE target = ?`, "replay-sealed-"+id).Scan(&grants)
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM actor_monitors WHERE watcher_instance_id = ?`, id).Scan(&monitors)
	if grants != 0 {
		t.Errorf("capability grants left after revoke = %d; want 0 (replays minted extra grants)", grants)
	}
	if monitors != 1 {
		t.Errorf("monitors = %d; want 1", monitors)
	}
	secret := got.Token[strings.LastIndex(got.Token, ":")+1:]
	if ok, _ := app.Storage().CheckCapability(ctx, storage.CapabilityKindChannelPub, "replay-sealed-"+id, hashCapabilitySecret(secret)); ok {
		t.Error("the issued token still validates after RevokeCapability")
	}
}

// Task primitives called from workflow code are recorded steps: the task a
// replay completes is the task the first run claimed.
func TestReplay_TaskPrimitivesRunOnce(t *testing.T) {
	app, cleanup := replayTestApp(t)
	defer cleanup()
	wf := DefineWorkflow("replay_task_primitives", func(ctx *WorkflowContext, channel string) (string, error) {
		if _, err := PostTask(ctx, channel, []byte(`{"n":1}`)); err != nil {
			return "", err
		}
		if _, err := PostTask(ctx, channel, []byte(`{"n":2}`)); err != nil {
			return "", err
		}
		task, err := ClaimTask(ctx, channel)
		if err != nil || task == nil {
			return "", fmt.Errorf("claim = %v, %v", task, err)
		}
		if err := Sleep(ctx, 50*time.Millisecond); err != nil {
			return "", err
		}
		if err := CompleteTask(ctx, task.ID, []byte(`"ok"`)); err != nil {
			return "", err
		}
		if err := Sleep(ctx, 50*time.Millisecond); err != nil {
			return "", err
		}
		return task.ID, nil
	})
	RegisterWorkflow(app, wf)

	ctx := context.Background()
	id, err := StartWorkflow(ctx, app, wf, "replay-q")
	if err != nil {
		t.Fatal(err)
	}
	inst := waitForTerminalInstance(t, app, id)
	if inst.Status != storage.StatusCompleted {
		t.Fatalf("instance = %s %s", inst.Status, inst.OutputData)
	}
	var claimedID string
	_ = json.Unmarshal(inst.OutputData, &claimedID)
	tasks, err := app.Storage().ListTasks(ctx, "replay-q", "")
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]string{}
	for _, task := range tasks {
		states[task.ID] = task.State
	}
	if len(tasks) != 2 {
		t.Fatalf("tasks = %v; want exactly the 2 posted", states)
	}
	if states[claimedID] != "done" {
		t.Errorf("claimed task %s is %q; want done (all: %v)", claimedID, states[claimedID], states)
	}
	for tid, st := range states {
		if tid != claimedID && st != "open" {
			t.Errorf("unclaimed task %s is %q; want open", tid, st)
		}
	}
}

// FS calls inside an activity work in every run, including activities that
// execute for the first time after the run resumed from history.
func TestReplay_FSAfterSuspend(t *testing.T) {
	app, cleanup := replayTestApp(t)
	defer cleanup()
	write := DefineActivity("replay_fs_write", func(ctx context.Context, p string) (bool, error) {
		return true, FSWrite(GetWorkflowContext(ctx), p, []byte("x"), 0, "")
	})
	wf := DefineWorkflow("replay_fs_after_suspend", func(ctx *WorkflowContext, _ struct{}) (string, error) {
		if _, err := write.Execute(ctx, "/actor/"+ctx.InstanceID()+"/a"); err != nil {
			return "", err
		}
		if err := Sleep(ctx, 50*time.Millisecond); err != nil {
			return "", err
		}
		if _, err := write.Execute(ctx, "/actor/"+ctx.InstanceID()+"/b"); err != nil {
			return "", err
		}
		return "ok", nil
	})
	RegisterWorkflow(app, wf)
	id, err := StartWorkflow(context.Background(), app, wf, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if inst := waitForTerminalInstance(t, app, id); inst.Status != storage.StatusCompleted {
		t.Fatalf("instance = %s %s", inst.Status, inst.OutputData)
	}
}

// FSList and FSRead stay inside the caller's own /actor/{id}/ space: an id
// that merely shares a prefix (g1 vs g10) or a differently-cased namespace
// is not readable.
func TestFS_ReadScope(t *testing.T) {
	app, cleanup := replayTestApp(t)
	defer cleanup()
	ctx := context.Background()
	if err := app.Storage().FSPut(ctx, "/actor/sup:w:g10/secret.txt", []byte("s"), "sup:w:g10", 0); err != nil {
		t.Fatal(err)
	}
	list := DefineActivity("fs_scope_list", func(actx context.Context, p string) ([]string, error) {
		objs, err := FSList(GetWorkflowContext(actx), p)
		if err != nil {
			return nil, err
		}
		paths := []string{}
		for _, o := range objs {
			paths = append(paths, o.Path)
		}
		return paths, nil
	})
	read := DefineActivity("fs_scope_read", func(actx context.Context, p string) (string, error) {
		data, _, err := FSRead(GetWorkflowContext(actx), p)
		return string(data), err
	})
	wf := DefineWorkflow("fs_read_scope", func(ctx *WorkflowContext, _ struct{}) (string, error) {
		own, err := list.Execute(ctx, "/actor/"+ctx.InstanceID())
		if err != nil {
			return "", err
		}
		if len(own) != 0 {
			return "", fmt.Errorf("own listing leaked %v", own)
		}
		if _, err := read.Execute(ctx, "/ACTOR/sup:w:g10/secret.txt"); !errors.Is(err, ErrFSForbidden) {
			return "", fmt.Errorf("read of /ACTOR/... = %v; want ErrFSForbidden", err)
		}
		if _, err := list.Execute(ctx, "/"); !errors.Is(err, ErrFSForbidden) {
			return "", fmt.Errorf("list / = %v; want ErrFSForbidden", err)
		}
		return "ok", nil
	})
	RegisterWorkflow(app, wf)
	id, err := StartWorkflow(ctx, app, wf, struct{}{}, WithInstanceID("sup:w:g1"))
	if err != nil {
		t.Fatal(err)
	}
	if inst := waitForTerminalInstance(t, app, id); inst.Status != storage.StatusCompleted {
		t.Fatalf("instance = %s %s", inst.Status, inst.OutputData)
	}
}

// WaitTask's timeout is measured against a recorded deadline: it fires, and
// a replay does not re-arm it.
func TestTasks_WaitTaskTimeoutFires(t *testing.T) {
	app, cleanup := replayTestApp(t)
	defer cleanup()
	ctx := context.Background()
	if err := app.Storage().PostTask(ctx, &storage.ActorTask{ID: "wt-never", Channel: "wt-q", Data: []byte(`{}`), State: "open"}); err != nil {
		t.Fatal(err)
	}
	wf := DefineWorkflow("wait_task_timeout", func(ctx *WorkflowContext, taskID string) (string, error) {
		_, err := WaitTask(ctx, taskID, 300*time.Millisecond)
		var timeoutErr *ChannelMessageTimeoutError
		if errors.As(err, &timeoutErr) {
			return "timed-out", nil
		}
		if err != nil {
			return "", err
		}
		return "settled", nil
	})
	RegisterWorkflow(app, wf)
	id, err := StartWorkflow(ctx, app, wf, "wt-never")
	if err != nil {
		t.Fatal(err)
	}
	inst := waitForTerminalInstance(t, app, id)
	if inst.Status != storage.StatusCompleted || string(inst.OutputData) != `"timed-out"` {
		t.Fatalf("instance = %s %s", inst.Status, inst.OutputData)
	}
	var timeouts int
	_ = app.Storage().DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM workflow_history WHERE instance_id = ? AND event_type = ?`, id, storage.HistoryMessageTimeout).Scan(&timeouts)
	if timeouts != 1 {
		t.Errorf("recorded receive timeouts = %d; want 1", timeouts)
	}
}

// A publish inside a transactional activity wakes a waiting receiver right
// after the commit.
func TestChannels_TransactionalPublishWakesReceiver(t *testing.T) {
	app, cleanup := replayTestApp(t)
	defer cleanup()
	ctx := context.Background()
	recv := DefineWorkflow("txwake_recv", func(ctx *WorkflowContext, _ struct{}) (string, error) {
		if err := Subscribe(ctx, "txwake", ModeBroadcast); err != nil {
			return "", err
		}
		msg, err := Receive[string](ctx, "txwake")
		if err != nil {
			return "", err
		}
		return msg.Data, nil
	})
	pub := DefineActivity("txwake_pub", func(actx context.Context, in string) (bool, error) {
		return true, Publish(GetWorkflowContext(actx), "txwake", in)
	})
	send := DefineWorkflow("txwake_send", func(ctx *WorkflowContext, in string) (bool, error) {
		return pub.Execute(ctx, in)
	})
	RegisterWorkflow(app, recv)
	RegisterWorkflow(app, send)
	rid, err := StartWorkflow(ctx, app, recv, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if !waitForStatus(t, ctx, app, rid, storage.StatusWaitingForMessage, 5*time.Second) {
		t.Fatal("receiver never waited")
	}
	if _, err := StartWorkflow(ctx, app, send, "hello"); err != nil {
		t.Fatal(err)
	}
	// The requeue backstop runs once a minute; only the post-commit wake
	// gets the receiver going within seconds.
	if !waitForStatus(t, ctx, app, rid, storage.StatusCompleted, 5*time.Second) {
		inst, _ := app.GetInstance(ctx, rid)
		t.Fatalf("receiver not woken after the commit: %+v", inst)
	}
}

// ReadChannelSince enforces channel_sub sealing like Receive.
func TestTasks_ReadChannelSinceSealed(t *testing.T) {
	app, cleanup := replayTestApp(t)
	defer cleanup()
	ctx := context.Background()
	token := grantChannelCap(t, app, storage.CapabilityKindChannelSub, "sealed-feed")
	read := DefineWorkflow("read_since_sealed", func(ctx *WorkflowContext, token string) (string, error) {
		_, errNoToken := ReadChannelSince(ctx, "sealed-feed", "")
		if !errors.Is(errNoToken, ErrChannelForbidden) {
			return "", fmt.Errorf("read without token = %v; want ErrChannelForbidden", errNoToken)
		}
		if _, err := ReadChannelSince(ctx, "sealed-feed", "", WithReceiveCapability(token)); err != nil {
			return "", fmt.Errorf("read with token: %w", err)
		}
		return "ok", nil
	})
	RegisterWorkflow(app, read)
	id, err := StartWorkflow(ctx, app, read, token)
	if err != nil {
		t.Fatal(err)
	}
	if inst := waitForTerminalInstance(t, app, id); inst.Status != storage.StatusCompleted {
		t.Fatalf("instance = %s %s", inst.Status, inst.OutputData)
	}
}

// A seal on a channel also covers its physical direct names "{ch}:{id}":
// naming the physical channel is not a way around SendTo's or a direct
// Receive's capability check.
func TestChannels_SealCoversDirectNames(t *testing.T) {
	app, cleanup := replayTestApp(t)
	defer cleanup()
	ctx := context.Background()
	grantChannelCap(t, app, storage.CapabilityKindChannelPub, "sealed-req")
	grantChannelCap(t, app, storage.CapabilityKindChannelSub, "sealed-req")
	wf := DefineWorkflow("seal_direct_names", func(ctx *WorkflowContext, victim string) (string, error) {
		if err := Publish(ctx, "sealed-req:"+victim, "x"); !errors.Is(err, ErrChannelForbidden) {
			return "", fmt.Errorf("publish to the physical direct name = %v; want ErrChannelForbidden", err)
		}
		if err := SendTo(ctx, victim, "sealed-req", "x"); !errors.Is(err, ErrChannelForbidden) {
			return "", fmt.Errorf("SendTo = %v; want ErrChannelForbidden", err)
		}
		if err := Subscribe(ctx, "sealed-req:"+victim, ModeBroadcast); err != nil {
			return "", err
		}
		_, err := Receive[string](ctx, "sealed-req:"+victim, WithReceiveTimeout(time.Second))
		if !errors.Is(err, ErrChannelForbidden) {
			return "", fmt.Errorf("receive on another instance's direct name = %v; want ErrChannelForbidden", err)
		}
		return "ok", nil
	})
	RegisterWorkflow(app, wf)
	id, err := StartWorkflow(ctx, app, wf, "victim-1")
	if err != nil {
		t.Fatal(err)
	}
	if inst := waitForTerminalInstance(t, app, id); inst.Status != storage.StatusCompleted {
		t.Fatalf("instance = %s %s", inst.Status, inst.OutputData)
	}
}

// A cancelled instance stays cancelled: its timer neither fires a resume
// nor is left behind.
func TestCancel_TimerDoesNotResurrect(t *testing.T) {
	app, cleanup := replayTestApp(t)
	defer cleanup()
	ctx := context.Background()
	wf := DefineWorkflow("cancel_timer_sticky", func(ctx *WorkflowContext, _ struct{}) (string, error) {
		if err := Sleep(ctx, 300*time.Millisecond); err != nil {
			return "", err
		}
		return "ran after cancel", nil
	})
	RegisterWorkflow(app, wf)
	id, err := StartWorkflow(ctx, app, wf, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if !waitForStatus(t, ctx, app, id, storage.StatusWaitingForTimer, 5*time.Second) {
		t.Fatal("never slept")
	}
	// The supervisor cancels through storage, without the instance lock.
	if err := app.Storage().CancelInstance(ctx, id, "test"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1200 * time.Millisecond)
	inst, err := app.Storage().GetInstance(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if inst.Status != storage.StatusCancelled {
		t.Fatalf("cancelled instance came back: %s %s", inst.Status, inst.OutputData)
	}
	var timers int
	_ = app.Storage().DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM workflow_timer_subscriptions WHERE instance_id = ?`, id).Scan(&timers)
	if timers != 0 {
		t.Errorf("timer subscriptions left = %d", timers)
	}
}

// A compensation body runs like an activity body of its instance: it can
// use GetWorkflowContext and the FS, e.g. to undo a write.
func TestCompensation_UsesFS(t *testing.T) {
	app, cleanup := replayTestApp(t)
	defer cleanup()
	ctx := context.Background()
	write := DefineActivity("comp_fs_write", func(actx context.Context, p string) (bool, error) {
		return true, FSWrite(GetWorkflowContext(actx), p, []byte("x"), 0, "")
	}, WithCompensation[string, bool](func(cctx context.Context, p string) error {
		wf := GetWorkflowContext(cctx)
		if wf == nil {
			return errors.New("no workflow context in compensation")
		}
		return FSDelete(wf, p, 0, "")
	}))
	wf := DefineWorkflow("comp_fs_undo", func(ctx *WorkflowContext, _ struct{}) (string, error) {
		if _, err := write.Execute(ctx, "/actor/"+ctx.InstanceID()+"/tmp"); err != nil {
			return "", err
		}
		return "", errors.New("fail after the write")
	})
	RegisterWorkflow(app, wf)
	id, err := StartWorkflow(ctx, app, wf, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if inst := waitForTerminalInstance(t, app, id); inst.Status != storage.StatusFailed {
		t.Fatalf("instance = %s", inst.Status)
	}
	obj, err := app.Storage().FSGet(ctx, "/actor/"+id+"/tmp")
	if err != nil {
		t.Fatal(err)
	}
	if obj != nil {
		t.Fatal("the compensation did not undo the FS write")
	}
	var failed int
	_ = app.Storage().DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM workflow_history WHERE instance_id = ? AND event_type = 'CompensationFailed'`, id).Scan(&failed)
	if failed != 0 {
		t.Errorf("compensation failed %d times", failed)
	}
}

// A publish that a sealed channel refused is a recorded outcome: after a
// suspension the replay observes the same refusal, even though the channel
// was unsealed in the meantime, and publishes nothing. (The workflow parks on
// a receive the test controls, so the unsealing surely precedes the replay.)
func TestReplay_PublishRefusalIsRecorded(t *testing.T) {
	app, cleanup := replayTestApp(t)
	defer cleanup()
	ctx := context.Background()
	grantChannelCap(t, app, storage.CapabilityKindChannelPub, "rg-sealed")
	wf := DefineWorkflow("replay_publish_refusal", func(ctx *WorkflowContext, _ struct{}) (string, error) {
		pubErr := Publish(ctx, "rg-sealed", "x")
		sendErr := SendTo(ctx, "rg-target", "rg-sealed", "x")
		if err := Subscribe(ctx, "rg-resume", ModeBroadcast); err != nil {
			return "", err
		}
		if _, err := Receive[string](ctx, "rg-resume"); err != nil {
			return "", err
		}
		return fmt.Sprintf("publish refused=%v, send refused=%v",
			errors.Is(pubErr, ErrChannelForbidden), errors.Is(sendErr, ErrChannelForbidden)), nil
	})
	resume := DefineWorkflow("replay_publish_refusal_resume", func(ctx *WorkflowContext, _ struct{}) (bool, error) {
		return true, Publish(ctx, "rg-resume", "go")
	})
	RegisterWorkflow(app, wf)
	RegisterWorkflow(app, resume)
	id, err := StartWorkflow(ctx, app, wf, struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	if !waitForStatus(t, ctx, app, id, storage.StatusWaitingForMessage, 10*time.Second) {
		t.Fatal("workflow did not park on its receive")
	}
	if err := app.Storage().RevokeCapability(ctx, "cap-test-channel_pub-rg-sealed"); err != nil {
		t.Fatal(err)
	}
	if _, err := StartWorkflow(ctx, app, resume, struct{}{}); err != nil {
		t.Fatal(err)
	}
	inst := waitForTerminalInstance(t, app, id)
	if want := `"publish refused=true, send refused=true"`; inst.Status != storage.StatusCompleted || string(inst.OutputData) != want {
		t.Fatalf("instance = %s %s; want completed %s", inst.Status, inst.OutputData, want)
	}
	if n := countChannelMessages(t, app, "rg-sealed") + countChannelMessages(t, app, "rg-sealed:rg-target"); n != 0 {
		t.Errorf("the replay published %d messages after the channel was unsealed", n)
	}
}
