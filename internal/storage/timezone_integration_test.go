//go:build integration

package storage

import (
	"context"
	"testing"
	"time"
)

// A database server whose default time zone is not UTC must not skew
// leases, timers or receive deadlines. The schema's TIMESTAMP / DATETIME
// columns hold wall clocks, Go writes them in UTC, and NOW() follows the
// session zone — so the session is pinned to UTC. Unpinned, a server east of
// UTC makes every live lease look expired (stale-lock cleanup steals running
// instances) and fires timers and deadlines early; west of UTC, late.
func TestStorage_NonUTCServerZone(t *testing.T) {
	ctx := context.Background()

	t.Run("postgres", func(t *testing.T) {
		_ = openPostgresBackend(t) // starts the shared server
		name := nextBackendDBName()
		if _, err := pgServer.admin.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
			t.Fatal(err)
		}
		if _, err := pgServer.admin.ExecContext(ctx, "ALTER DATABASE "+name+" SET timezone TO 'Asia/Tokyo'"); err != nil {
			t.Fatal(err)
		}
		store, err := NewPostgresStorage(pgServer.url(name))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = store.Close()
			_, _ = pgServer.admin.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		})
		if err := InitializeTestSchemaPostgres(ctx, store); err != nil {
			t.Fatal(err)
		}
		checkClockAgreement(t, ctx, store)
	})

	t.Run("mysql", func(t *testing.T) {
		_ = openMySQLBackend(t) // starts the shared server
		if _, err := mysqlServer.admin.ExecContext(ctx, "SET GLOBAL time_zone = '+09:00'"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = mysqlServer.admin.ExecContext(context.Background(), "SET GLOBAL time_zone = 'SYSTEM'")
		})
		checkClockAgreement(t, ctx, openMySQLBackend(t))
	})
}

func checkClockAgreement(t *testing.T, ctx context.Context, s Storage) {
	t.Helper()

	createTestInstance(t, ctx, s, "tz-lock", "wf")
	mustAcquire(t, ctx, s, "tz-lock", NewLockIdentity("a"))
	stale, err := s.CleanupStaleLocks(ctx, 300)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 0 {
		t.Fatalf("a fresh 300s lease was cleaned up as stale: %+v", stale)
	}
	if inst, _ := s.GetInstance(ctx, "tz-lock"); inst.LockedBy == "" {
		t.Fatal("stale-lock cleanup released a live lease")
	}

	createTestInstance(t, ctx, s, "tz-wait", "wf")
	deadline := time.Now().Add(time.Hour)
	waitOn(t, ctx, s, "tz-wait", "tz", ChannelModeBroadcast, &deadline)
	expired, err := s.FindExpiredChannelSubscriptions(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(expired) != 0 {
		t.Fatalf("a receive deadline an hour ahead is already expired: %+v", *expired[0])
	}

	createTestInstance(t, ctx, s, "tz-timer", "wf")
	if err := s.RegisterTimerSubscription(ctx, &TimerSubscription{InstanceID: "tz-timer", TimerID: "sleep:1",
		ExpiresAt: time.Now().Add(time.Hour), ActivityID: "sleep:1"}); err != nil {
		t.Fatal(err)
	}
	timers, err := s.FindExpiredTimers(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(timers) != 0 {
		t.Fatalf("a timer an hour ahead already fired: %+v", *timers[0])
	}
}

// Every MySQL session runs at READ COMMITTED, also on a server whose
// default is REPEATABLE READ (MySQL's own default).
func TestStorage_MySQLSessionIsolation(t *testing.T) {
	ctx := context.Background()
	d := openMySQLBackend(t)
	var level string
	if err := d.db.QueryRowContext(ctx, `SELECT @@transaction_isolation`).Scan(&level); err != nil {
		t.Fatal(err)
	}
	if level != "READ-COMMITTED" {
		t.Fatalf("session isolation = %s; want READ-COMMITTED", level)
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := tx.QueryRowContext(ctx, `SELECT @@transaction_isolation`).Scan(&level); err != nil {
		t.Fatal(err)
	}
	if level != "READ-COMMITTED" {
		t.Fatalf("transaction isolation = %s; want READ-COMMITTED", level)
	}
}
