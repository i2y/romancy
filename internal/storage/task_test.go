package storage

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestStorage_ActorTasks(t *testing.T) {
	forEachBackend(t, func(t *testing.T, ctx context.Context, s *testDB) {
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
		if task.ID != "t1" || task.State != "claimed" || task.ClaimedBy != "wf-1" || string(task.Data) != `{"n":1}` {
			t.Fatalf("task = %+v", task)
		}
		// second claimant gets next task
		task2, err := s.ClaimTask(ctx, "tasks:x", "wf-2")
		if err != nil || task2 == nil || task2.ID != "t2" {
			t.Fatalf("claim2 = %v, %v", task2, err)
		}
		// none left
		if task3, err := s.ClaimTask(ctx, "tasks:x", "wf-3"); err != nil || task3 != nil {
			t.Fatalf("unexpected claim: %+v, %v", task3, err)
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
		list, err := s.ListTasks(ctx, "tasks:x", "")
		if err != nil || len(list) != 2 {
			t.Fatalf("list = %+v, %v", list, err)
		}
		states := map[string]string{}
		for _, tk := range list {
			states[tk.ID] = tk.State
		}
		if states["t1"] != "done" || states["t2"] != "failed" {
			t.Fatalf("states = %v", states)
		}
		if open, err := s.ListTasks(ctx, "tasks:x", "open"); err != nil || len(open) != 0 {
			t.Fatalf("open = %+v, %v", open, err)
		}
		if y, err := s.ListTasks(ctx, "tasks:y", "open"); err != nil || len(y) != 1 {
			t.Fatalf("y-open = %+v, %v", y, err)
		}
		got, err := s.GetTask(ctx, "t1")
		if err != nil || got == nil || string(got.Result) != `{"ok":true}` || got.State != "done" {
			t.Fatalf("GetTask(t1) = %+v, %v", got, err)
		}
	})
}

// A claimant that already holds a claimed task must get the NEW task back
// from its next claim, not the one it already holds.
func TestStorage_ClaimTaskReturnsTheClaimedRow(t *testing.T) {
	forEachBackend(t, func(t *testing.T, ctx context.Context, s *testDB) {
		for _, id := range []string{"a1", "a2", "a3"} {
			if err := s.PostTask(ctx, &ActorTask{ID: id, Channel: "tasks:same", Data: []byte(`"` + id + `"`)}); err != nil {
				t.Fatal(err)
			}
		}
		seen := map[string]bool{}
		for i := 0; i < 3; i++ {
			task, err := s.ClaimTask(ctx, "tasks:same", "wf-greedy")
			if err != nil || task == nil {
				t.Fatalf("claim %d = %v, %v", i, task, err)
			}
			if seen[task.ID] {
				t.Fatalf("claim %d returned already-held task %s", i, task.ID)
			}
			if string(task.Data) != `"`+task.ID+`"` {
				t.Fatalf("claim %d: data %s does not belong to %s", i, task.Data, task.ID)
			}
			seen[task.ID] = true
		}
		claimed, err := s.ListTasks(ctx, "tasks:same", "claimed")
		if err != nil || len(claimed) != 3 {
			t.Fatalf("claimed = %+v, %v", claimed, err)
		}
	})
}

func TestStorage_ChannelMessagesSince(t *testing.T) {
	forEachBackend(t, func(t *testing.T, ctx context.Context, s *testDB) {
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
		// An unknown cursor is an error, not "read everything again".
		if _, err := s.ChannelMessagesSince(ctx, "cur", "no-such-id"); !errors.Is(err, ErrCursorNotFound) {
			t.Fatalf("unknown cursor = %v; want ErrCursorNotFound", err)
		}
	})
}

func TestStorage_TaskNoticesAndGetTask(t *testing.T) {
	forEachBackend(t, func(t *testing.T, ctx context.Context, s *testDB) {
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
		if none, err := s.GetTask(ctx, "nope"); err != nil || none != nil {
			t.Fatalf("GetTask(nope) = %+v, %v", none, err)
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
	})
}

func TestStorage_ReclaimStaleTasks(t *testing.T) {
	forEachBackend(t, func(t *testing.T, ctx context.Context, s *testDB) {
		mk := func(id string) {
			if err := s.PostTask(ctx, &ActorTask{ID: id, Channel: "tasks:r"}); err != nil {
				t.Fatal(err)
			}
		}
		mk("t1")
		mk("t2")
		if _, err := s.ClaimTask(ctx, "tasks:r", "wf-1"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ClaimTask(ctx, "tasks:r", "wf-2"); err != nil {
			t.Fatal(err)
		}
		// attempts incremented on claim
		task, _ := s.GetTask(ctx, "t1")
		if task == nil || task.Attempts != 1 {
			t.Fatalf("attempts = %+v", task)
		}
		// nothing stale yet — claims are fresh (this also catches timezone
		// skew between how claimed_at is written and how the sweep compares)
		if n, err := s.ReclaimStaleTasks(ctx, 5*time.Minute); err != nil || n != 0 {
			t.Fatalf("reclaim fresh = %d, %v", n, err)
		}
		// backdate t1's claim to simulate a dead claimant
		s.exec(t, ctx, `UPDATE actor_tasks SET claimed_at = ? WHERE id = ?`,
			time.Now().UTC().Add(-2*time.Hour), "t1")
		n, err := s.ReclaimStaleTasks(ctx, time.Hour)
		if err != nil || n != 1 {
			t.Fatalf("reclaim = %d, %v", n, err)
		}
		task, _ = s.GetTask(ctx, "t1")
		if task.State != "open" || task.ClaimedBy != "" {
			t.Fatalf("reclaimed task = %+v", task)
		}
		// fresh claim (t2) untouched
		task, _ = s.GetTask(ctx, "t2")
		if task.State != "claimed" || task.ClaimedBy != "wf-2" {
			t.Fatalf("fresh claim = %+v", task)
		}
		// a reclaimed task is claimable again, with attempts carried over
		again, err := s.ClaimTask(ctx, "tasks:r", "wf-3")
		if err != nil || again == nil || again.ID != "t1" || again.Attempts != 2 {
			t.Fatalf("reclaim+claim = %+v, %v", again, err)
		}
	})
}
