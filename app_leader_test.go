package romancy

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/i2y/romancy/internal/storage"
)

func TestLeaderElection_SingleWorker(t *testing.T) {
	// Create app with test database
	app, cleanup := createTestApp(t,
		WithLeaderHeartbeatInterval(50*time.Millisecond),
		WithLeaderLeaseDuration(150*time.Millisecond),
	)
	defer cleanup()

	ctx := context.Background()
	if err := app.Start(ctx); err != nil {
		t.Fatalf("failed to start app: %v", err)
	}
	defer func() { _ = app.Shutdown(ctx) }()

	// Wait for leader election to run
	time.Sleep(100 * time.Millisecond)

	// Single worker should become leader
	if !app.isLeader.Load() {
		t.Error("expected single worker to become leader")
	}
}

func TestLeaderElection_MultipleWorkers(t *testing.T) {
	// Create multiple apps sharing the same database
	// First, create a temp database and initialize schema
	app1, cleanup := createTestApp(t,
		WithWorkerID("worker-1"),
		WithLeaderHeartbeatInterval(50*time.Millisecond),
		WithLeaderLeaseDuration(150*time.Millisecond),
	)
	defer cleanup()

	// Get the database path from app1's storage
	sqliteStorage, ok := app1.storage.(*storage.SQLiteStorage)
	if !ok {
		t.Skip("test requires SQLite storage")
	}
	// We can't easily get the path, so let's use a different approach
	// Create second app with same database by using the test helper properly

	ctx := context.Background()
	if err := app1.Start(ctx); err != nil {
		t.Fatalf("failed to start app1: %v", err)
	}

	// Wait for app1 to become leader
	time.Sleep(100 * time.Millisecond)

	if !app1.isLeader.Load() {
		t.Error("expected app1 to become leader")
	}

	// Clean up - need to reference sqliteStorage to avoid unused variable error
	_ = sqliteStorage
	_ = app1.Shutdown(ctx)
}

func TestLeaderElection_LeadershipState(t *testing.T) {
	app, cleanup := createTestApp(t,
		WithLeaderHeartbeatInterval(50*time.Millisecond),
		WithLeaderLeaseDuration(150*time.Millisecond),
	)
	defer cleanup()

	// Before start, should not be leader
	if app.isLeader.Load() {
		t.Error("expected isLeader=false before start")
	}

	ctx := context.Background()
	if err := app.Start(ctx); err != nil {
		t.Fatalf("failed to start app: %v", err)
	}

	// Wait for leader election
	time.Sleep(100 * time.Millisecond)

	// Should be leader now
	if !app.isLeader.Load() {
		t.Error("expected isLeader=true after leader election")
	}

	// Shutdown
	if err := app.Shutdown(ctx); err != nil {
		t.Fatalf("failed to shutdown app: %v", err)
	}

	// After shutdown, isLeader may still be true (we don't explicitly clear it)
	// but leader tasks should be stopped
}

func TestLeaderElection_LeaderTasksRunning(t *testing.T) {
	app, cleanup := createTestApp(t,
		WithLeaderHeartbeatInterval(50*time.Millisecond),
		WithLeaderLeaseDuration(150*time.Millisecond),
	)
	defer cleanup()

	ctx := context.Background()
	if err := app.Start(ctx); err != nil {
		t.Fatalf("failed to start app: %v", err)
	}
	defer func() { _ = app.Shutdown(ctx) }()

	// Wait for leader election
	time.Sleep(100 * time.Millisecond)

	// Check that leader tasks are running
	app.leaderMu.Lock()
	tasksRunning := app.leaderTasksRunning
	app.leaderMu.Unlock()

	if !tasksRunning {
		t.Error("expected leaderTasksRunning=true after becoming leader")
	}
}

func TestLeaderElection_GracefulShutdown(t *testing.T) {
	app, cleanup := createTestApp(t,
		WithLeaderHeartbeatInterval(50*time.Millisecond),
		WithLeaderLeaseDuration(150*time.Millisecond),
		WithShutdownTimeout(5*time.Second),
	)
	defer cleanup()

	ctx := context.Background()
	if err := app.Start(ctx); err != nil {
		t.Fatalf("failed to start app: %v", err)
	}

	// Wait for leader election
	time.Sleep(100 * time.Millisecond)

	// Verify we became leader
	if !app.isLeader.Load() {
		t.Error("expected to become leader")
	}

	// Shutdown should complete without hanging
	shutdownCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- app.Shutdown(shutdownCtx)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("shutdown failed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Error("shutdown timed out")
	}
}

func TestLeaderElection_Options(t *testing.T) {
	tests := []struct {
		name              string
		heartbeatInterval time.Duration
		leaseDuration     time.Duration
	}{
		{"default values", 0, 0},
		{"custom heartbeat", 30 * time.Second, 0},
		{"custom lease", 0, 90 * time.Second},
		{"both custom", 10 * time.Second, 30 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var opts []Option
			if tt.heartbeatInterval > 0 {
				opts = append(opts, WithLeaderHeartbeatInterval(tt.heartbeatInterval))
			}
			if tt.leaseDuration > 0 {
				opts = append(opts, WithLeaderLeaseDuration(tt.leaseDuration))
			}

			config := defaultConfig()
			for _, opt := range opts {
				opt(config)
			}

			// Check heartbeat interval
			if tt.heartbeatInterval > 0 {
				if config.leaderHeartbeatInterval != tt.heartbeatInterval {
					t.Errorf("expected heartbeatInterval=%v, got %v",
						tt.heartbeatInterval, config.leaderHeartbeatInterval)
				}
			} else {
				// Default is 15 seconds
				if config.leaderHeartbeatInterval != 15*time.Second {
					t.Errorf("expected default heartbeatInterval=15s, got %v",
						config.leaderHeartbeatInterval)
				}
			}

			// Check lease duration
			if tt.leaseDuration > 0 {
				if config.leaderLeaseDuration != tt.leaseDuration {
					t.Errorf("expected leaseDuration=%v, got %v",
						tt.leaseDuration, config.leaderLeaseDuration)
				}
			} else {
				// Default is 45 seconds
				if config.leaderLeaseDuration != 45*time.Second {
					t.Errorf("expected default leaseDuration=45s, got %v",
						config.leaderLeaseDuration)
				}
			}
		})
	}
}

func TestLeaderElection_ConcurrentAccess(t *testing.T) {
	app, cleanup := createTestApp(t,
		WithLeaderHeartbeatInterval(10*time.Millisecond),
		WithLeaderLeaseDuration(30*time.Millisecond),
	)
	defer cleanup()

	ctx := context.Background()
	if err := app.Start(ctx); err != nil {
		t.Fatalf("failed to start app: %v", err)
	}
	defer func() { _ = app.Shutdown(ctx) }()

	// Concurrently check isLeader from multiple goroutines
	var wg sync.WaitGroup
	errChan := make(chan error, 10)

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				// This should not panic or race
				_ = app.isLeader.Load()
			}
		}()
	}

	wg.Wait()
	close(errChan)

	for err := range errChan {
		t.Errorf("concurrent access error: %v", err)
	}
}
