package storage

import (
	"testing"
)

// ActorRegistry: register/whereis, monitors, and the terminal DOWN sweep.

func TestSQLiteStorage_ActorRegistry(t *testing.T) {
	s, ctx := newClaimTestStorage(t)

	createClaimInstance(t, ctx, s, "wf-actor-1", "wf")
	createClaimInstance(t, ctx, s, "wf-actor-2", "wf")

	// register + whereis
	if err := s.RegisterActorName(ctx, "order-actor", "wf-actor-1", "go"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if id, _ := s.WhereisActor(ctx, "order-actor"); id != "wf-actor-1" {
		t.Fatalf("whereis = %q, want wf-actor-1", id)
	}
	if id, _ := s.WhereisActor(ctx, "nope"); id != "" {
		t.Fatalf("whereis unbound = %q, want \"\"", id)
	}

	// duplicate name rejected
	if err := s.RegisterActorName(ctx, "order-actor", "wf-actor-2", "go"); err == nil {
		t.Fatal("duplicate register should fail")
	}

	// unregister frees the name
	if err := s.UnregisterActorName(ctx, "order-actor"); err != nil {
		t.Fatalf("unregister: %v", err)
	}
	if err := s.RegisterActorName(ctx, "order-actor", "wf-actor-2", "go"); err != nil {
		t.Fatalf("re-register after unregister: %v", err)
	}
}

func TestSQLiteStorage_ActorMonitorDown(t *testing.T) {
	s, ctx := newClaimTestStorage(t)

	createClaimInstance(t, ctx, s, "wf-watcher", "wf")
	createClaimInstance(t, ctx, s, "wf-target", "wf")

	if err := s.MonitorActor(ctx, "wf-watcher", "wf-target", "mon:wf-watcher"); err != nil {
		t.Fatalf("monitor: %v", err)
	}
	// idempotent re-monitor
	if err := s.MonitorActor(ctx, "wf-watcher", "wf-target", "mon:wf-watcher"); err != nil {
		t.Fatalf("re-monitor: %v", err)
	}

	// not terminal yet — nothing emitted
	if n, err := s.EmitActorDownForTerminated(ctx); err != nil || n != 0 {
		t.Fatalf("emit before terminal = %d, %v; want 0, nil", n, err)
	}

	// mark target terminal, then sweep
	if _, err := s.db.ExecContext(ctx, `
		UPDATE workflow_instances SET status = 'completed' WHERE instance_id = 'wf-target'
	`); err != nil {
		t.Fatalf("set terminal: %v", err)
	}
	n, err := s.EmitActorDownForTerminated(ctx)
	if err != nil || n != 1 {
		t.Fatalf("emit = %d, %v; want 1, nil", n, err)
	}

	// DOWN message delivered to the monitor channel
	var data string
	if err := s.db.QueryRowContext(ctx, `
		SELECT data FROM channel_messages WHERE channel = 'mon:wf-watcher'
	`).Scan(&data); err != nil {
		t.Fatalf("down message: %v", err)
	}
	if want := `{"type":"DOWN","actor":"wf-target","status":"completed"}`; data != want {
		t.Fatalf("down payload = %q, want %q", data, want)
	}

	// monitor row consumed — re-emit is a no-op
	if n, err := s.EmitActorDownForTerminated(ctx); err != nil || n != 0 {
		t.Fatalf("re-emit = %d, %v; want 0, nil", n, err)
	}

	// unmonitor removes registration
	if err := s.MonitorActor(ctx, "wf-watcher", "wf-target", "mon:wf-watcher"); err != nil {
		t.Fatalf("re-monitor: %v", err)
	}
	if err := s.UnmonitorActor(ctx, "wf-watcher", "wf-target"); err != nil {
		t.Fatalf("unmonitor: %v", err)
	}
	if n, err := s.EmitActorDownForTerminated(ctx); err != nil || n != 0 {
		t.Fatalf("emit after unmonitor = %d, %v; want 0, nil", n, err)
	}
}

func TestSQLiteStorage_ActorNameFreedOnTerminal(t *testing.T) {
	s, ctx := newClaimTestStorage(t)

	createClaimInstance(t, ctx, s, "wf-w", "wf")
	createClaimInstance(t, ctx, s, "wf-t", "wf")

	if err := s.RegisterActorName(ctx, "named", "wf-t", "go"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := s.MonitorActor(ctx, "wf-w", "wf-t", "mon:wf-w"); err != nil {
		t.Fatalf("monitor: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE workflow_instances SET status = 'failed' WHERE instance_id = 'wf-t'
	`); err != nil {
		t.Fatalf("set terminal: %v", err)
	}
	if _, err := s.EmitActorDownForTerminated(ctx); err != nil {
		t.Fatalf("emit: %v", err)
	}

	// terminal actor's name binding is freed by the sweep
	if id, _ := s.WhereisActor(ctx, "named"); id != "" {
		t.Fatalf("name still bound to %q after terminal", id)
	}
}
