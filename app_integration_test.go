//go:build integration

package romancy

import (
	"context"
	"sync"
	"testing"
	"time"
)

// ========================================
// PostgreSQL Integration Tests
// ========================================

func TestPostgresIntegration_AppStartShutdown(t *testing.T) {
	app, cleanup := createTestAppWithPostgres(t)
	defer cleanup()

	ctx := context.Background()
	if err := app.Start(ctx); err != nil {
		t.Fatalf("failed to start app: %v", err)
	}

	// Give it a moment to start background tasks
	time.Sleep(100 * time.Millisecond)

	if err := app.Shutdown(ctx); err != nil {
		t.Fatalf("failed to shutdown app: %v", err)
	}
}

func TestPostgresIntegration_LeaderElection(t *testing.T) {
	app, cleanup := createTestAppWithPostgres(t,
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
	time.Sleep(200 * time.Millisecond)

	// Single worker should become leader
	if !app.isLeader.Load() {
		t.Error("expected single worker to become leader")
	}
}

func TestPostgresIntegration_LeaderElectionMultiWorker(t *testing.T) {
	// Create first app and start it
	app1, cleanup1 := createTestAppWithPostgres(t,
		WithWorkerID("worker-1"),
		WithLeaderHeartbeatInterval(50*time.Millisecond),
		WithLeaderLeaseDuration(150*time.Millisecond),
	)
	defer cleanup1()

	ctx := context.Background()
	if err := app1.Start(ctx); err != nil {
		t.Fatalf("failed to start app1: %v", err)
	}

	// Wait for app1 to become leader
	time.Sleep(200 * time.Millisecond)

	if !app1.isLeader.Load() {
		t.Error("expected app1 to become leader")
	}

	// Note: Creating a second app with the same container is complex
	// because we'd need to reuse the same container's connection.
	// For now, this test verifies single worker leader election works.
	// A more complete multi-worker test would require shared container setup.

	_ = app1.Shutdown(ctx)
}

func TestPostgresIntegration_LeaderTasksRunning(t *testing.T) {
	app, cleanup := createTestAppWithPostgres(t,
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
	time.Sleep(200 * time.Millisecond)

	// Check that leader tasks are running
	app.leaderMu.Lock()
	tasksRunning := app.leaderTasksRunning
	app.leaderMu.Unlock()

	if !tasksRunning {
		t.Error("expected leaderTasksRunning=true after becoming leader")
	}
}

func TestPostgresIntegration_GracefulShutdown(t *testing.T) {
	app, cleanup := createTestAppWithPostgres(t,
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
	time.Sleep(200 * time.Millisecond)

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

func TestPostgresIntegration_ConcurrentAccess(t *testing.T) {
	app, cleanup := createTestAppWithPostgres(t,
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

// ========================================
// MySQL Integration Tests
// ========================================

func TestMySQLIntegration_AppStartShutdown(t *testing.T) {
	app, cleanup := createTestAppWithMySQL(t)
	defer cleanup()

	ctx := context.Background()
	if err := app.Start(ctx); err != nil {
		t.Fatalf("failed to start app: %v", err)
	}

	// Give it a moment to start background tasks
	time.Sleep(100 * time.Millisecond)

	if err := app.Shutdown(ctx); err != nil {
		t.Fatalf("failed to shutdown app: %v", err)
	}
}

func TestMySQLIntegration_LeaderElection(t *testing.T) {
	app, cleanup := createTestAppWithMySQL(t,
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
	time.Sleep(200 * time.Millisecond)

	// Single worker should become leader
	if !app.isLeader.Load() {
		t.Error("expected single worker to become leader")
	}
}

func TestMySQLIntegration_LeaderTasksRunning(t *testing.T) {
	app, cleanup := createTestAppWithMySQL(t,
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
	time.Sleep(200 * time.Millisecond)

	// Check that leader tasks are running
	app.leaderMu.Lock()
	tasksRunning := app.leaderTasksRunning
	app.leaderMu.Unlock()

	if !tasksRunning {
		t.Error("expected leaderTasksRunning=true after becoming leader")
	}
}

func TestMySQLIntegration_GracefulShutdown(t *testing.T) {
	app, cleanup := createTestAppWithMySQL(t,
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
	time.Sleep(200 * time.Millisecond)

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

func TestMySQLIntegration_ConcurrentAccess(t *testing.T) {
	app, cleanup := createTestAppWithMySQL(t,
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
