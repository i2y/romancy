package storage

import (
	"errors"
	"testing"
)

func TestSQLiteStorage_ActorTasks(t *testing.T) {
	s, ctx := newClaimTestStorage(t)

	mk := func(id, ch string) {
		if err := s.PostTask(ctx, &ActorTask{ID: id, Channel: ch, Data: []byte(`{"n":1}`)}); err != nil {
			t.Fatal(err)
		}
	}
	mk("t1", "tasks:x")
	mk("t2", "tasks:x")
	mk("t3", "tasks:y")

	task, err := s.ClaimTask(ctx, "tasks:x", "wf-1")
	if err != nil || task == nil {
		t.Fatalf("claim = %v, %v", task, err)
	}
	if task.ID != "t1" || task.State != "claimed" || task.ClaimedBy != "wf-1" {
		t.Fatalf("task = %+v", task)
	}
	// second claimant gets next task
	task2, err := s.ClaimTask(ctx, "tasks:x", "wf-2")
	if err != nil || task2 == nil || task2.ID != "t2" {
		t.Fatalf("claim2 = %v, %v", task2, err)
	}
	// none left
	if task3, _ := s.ClaimTask(ctx, "tasks:x", "wf-3"); task3 != nil {
		t.Fatalf("unexpected claim: %+v", task3)
	}
	// complete only by claimant
	if err := s.CompleteTask(ctx, "t1", "wf-2", []byte(`{"ok":true}`), false); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("foreign complete: %v", err)
	}
	if err := s.CompleteTask(ctx, "t1", "wf-1", []byte(`{"ok":true}`), false); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if err := s.CompleteTask(ctx, "t2", "wf-2", []byte(`{"err":"x"}`), true); err != nil {
		t.Fatalf("fail-task: %v", err)
	}
	list, _ := s.ListTasks(ctx, "tasks:x", "")
	if len(list) != 2 || list[0].State != "done" || list[1].State != "failed" {
		t.Fatalf("list = %+v", list)
	}
	open, _ := s.ListTasks(ctx, "tasks:x", "open")
	if len(open) != 0 {
		t.Fatalf("open = %+v", open)
	}
	if y, _ := s.ListTasks(ctx, "tasks:y", "open"); len(y) != 1 {
		t.Fatalf("y-open = %+v", y)
	}
}

func TestSQLiteStorage_ChannelMessagesSince(t *testing.T) {
	s, ctx := newClaimTestStorage(t)
	for _, d := range []string{`{"i":1}`, `{"i":2}`, `{"i":3}`} {
		if _, err := s.PublishToChannel(ctx, "cur", []byte(d), nil); err != nil {
			t.Fatal(err)
		}
	}
	all, err := s.ChannelMessagesSince(ctx, "cur", "")
	if err != nil || len(all) != 3 {
		t.Fatalf("all = %d, %v", len(all), err)
	}
	rest, err := s.ChannelMessagesSince(ctx, "cur", all[0].MessageID)
	if err != nil || len(rest) != 2 || string(rest[0].Data) != `{"i":2}` {
		t.Fatalf("rest = %+v, %v", rest, err)
	}
	end, err := s.ChannelMessagesSince(ctx, "cur", all[2].MessageID)
	if err != nil || len(end) != 0 {
		t.Fatalf("end = %d, %v", len(end), err)
	}
	// unknown cursor behaves like empty (reads all)
	unk, err := s.ChannelMessagesSince(ctx, "cur", "no-such-id")
	if err != nil || len(unk) != 3 {
		t.Fatalf("unk = %d, %v", len(unk), err)
	}
}

func TestSQLiteStorage_TaskNoticesAndGetTask(t *testing.T) {
	s, ctx := newClaimTestStorage(t)

	if err := s.PostTask(ctx, &ActorTask{ID: "t1", Channel: "tasks:x", Data: []byte(`{"n":1}`)}); err != nil {
		t.Fatal(err)
	}
	// task_posted notice on the :tasks event stream
	msgs, err := s.ChannelMessagesSince(ctx, "tasks:x:tasks", "")
	if err != nil || len(msgs) != 1 {
		t.Fatalf("posted notice = %d, %v", len(msgs), err)
	}
	if string(msgs[0].Data) != `{"type":"task_posted","task_id":"t1","channel":"tasks:x"}` {
		t.Fatalf("notice data = %s", msgs[0].Data)
	}

	got, err := s.GetTask(ctx, "t1")
	if err != nil || got == nil || got.State != "open" {
		t.Fatalf("GetTask = %+v, %v", got, err)
	}
	if none, _ := s.GetTask(ctx, "nope"); none != nil {
		t.Fatalf("GetTask(nope) = %+v", none)
	}

	if _, err := s.ClaimTask(ctx, "tasks:x", "wf-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteTask(ctx, "t1", "wf-1", []byte(`{"ok":true}`), false); err != nil {
		t.Fatal(err)
	}
	msgs, err = s.ChannelMessagesSince(ctx, "tasks:x:tasks", msgs[0].MessageID)
	if err != nil || len(msgs) != 2 {
		t.Fatalf("notices = %d, %v", len(msgs), err)
	}
	if string(msgs[1].Data) != `{"type":"task_state","task_id":"t1","state":"done"}` {
		t.Fatalf("state notice = %s", msgs[1].Data)
	}
}
