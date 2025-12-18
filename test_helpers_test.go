package romancy

import (
	"context"
	"os"
	"testing"

	"github.com/i2y/romancy/internal/storage"
)

// createTestApp creates a new App with a pre-initialized test database.
// This helper should be used in tests that create workflows and need
// the database schema to be ready.
// Additional options can be passed to configure the App.
func createTestApp(t *testing.T, opts ...Option) (*App, func()) {
	t.Helper()

	// Create a temporary database
	tmpFile, err := os.CreateTemp("", "romancy-test-*.db")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	tmpPath := tmpFile.Name()
	_ = tmpFile.Close()

	// Create SQLite storage directly to initialize schema
	store, err := storage.NewSQLiteStorage(tmpPath)
	if err != nil {
		_ = os.Remove(tmpPath)
		t.Fatalf("failed to create storage: %v", err)
	}

	// Initialize test schema
	ctx := context.Background()
	if err := storage.InitializeTestSchema(ctx, store); err != nil {
		_ = store.Close()
		_ = os.Remove(tmpPath)
		t.Fatalf("failed to initialize test schema: %v", err)
	}
	_ = store.Close()

	// Create app with the database path and any additional options
	// Disable auto-migrate since we already created the schema manually
	allOpts := append([]Option{WithDatabase(tmpPath), WithAutoMigrate(false)}, opts...)
	app := NewApp(allOpts...)

	cleanup := func() {
		_ = app.Shutdown(context.Background())
		_ = os.Remove(tmpPath)
	}

	return app, cleanup
}
