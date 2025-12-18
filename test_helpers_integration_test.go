//go:build integration

package romancy

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/i2y/romancy/internal/storage"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/mysql"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// createTestAppWithPostgres creates a new App with a PostgreSQL testcontainer backend.
// Additional options can be passed to configure the App.
func createTestAppWithPostgres(t *testing.T, opts ...Option) (*App, func()) {
	t.Helper()
	ctx := context.Background()

	container, err := postgres.Run(ctx,
		"postgres:15-alpine",
		postgres.WithDatabase("romancy_test"),
		postgres.WithUsername("romancy"),
		postgres.WithPassword("romancy"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(5*time.Minute)),
	)
	if err != nil {
		t.Fatalf("failed to start PostgreSQL container: %v", err)
	}

	connStr, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		testcontainers.CleanupContainer(t, container)
		t.Fatalf("failed to get connection string: %v", err)
	}

	// Create PostgreSQL storage directly to initialize schema
	store, err := storage.NewPostgresStorage(connStr)
	if err != nil {
		testcontainers.CleanupContainer(t, container)
		t.Fatalf("failed to create PostgreSQL storage: %v", err)
	}

	// Initialize test schema
	if err := storage.InitializeTestSchemaPostgres(ctx, store); err != nil {
		_ = store.Close()
		testcontainers.CleanupContainer(t, container)
		t.Fatalf("failed to initialize test schema: %v", err)
	}
	_ = store.Close()

	// Create app with the connection string and any additional options
	allOpts := append([]Option{WithDatabase(connStr)}, opts...)
	app := NewApp(allOpts...)

	cleanup := func() {
		_ = app.Shutdown(context.Background())
		testcontainers.CleanupContainer(t, container)
	}

	return app, cleanup
}

// createTestAppWithMySQL creates a new App with a MySQL testcontainer backend.
// Additional options can be passed to configure the App.
func createTestAppWithMySQL(t *testing.T, opts ...Option) (*App, func()) {
	t.Helper()
	ctx := context.Background()

	container, err := mysql.Run(ctx,
		"mysql:8.0",
		mysql.WithDatabase("romancy_test"),
		mysql.WithUsername("romancy"),
		mysql.WithPassword("romancy"),
	)
	if err != nil {
		t.Fatalf("failed to start MySQL container: %v", err)
	}

	// Get connection string (DSN format)
	connStr, err := container.ConnectionString(ctx, "parseTime=true", "loc=UTC", "multiStatements=true")
	if err != nil {
		testcontainers.CleanupContainer(t, container)
		t.Fatalf("failed to get connection string: %v", err)
	}

	// Create MySQL storage directly to initialize schema
	store, err := storage.NewMySQLStorage(connStr)
	if err != nil {
		testcontainers.CleanupContainer(t, container)
		t.Fatalf("failed to create MySQL storage: %v", err)
	}

	// Initialize test schema
	if err := storage.InitializeTestSchemaMySQL(ctx, store); err != nil {
		_ = store.Close()
		testcontainers.CleanupContainer(t, container)
		t.Fatalf("failed to initialize test schema: %v", err)
	}
	_ = store.Close()

	// Convert DSN format to mysql:// URL format for WithDatabase
	// DSN: user:pass@tcp(host:port)/db?params
	// URL: mysql://user:pass@host:port/db?params
	mysqlConnStr := connStr
	if strings.Contains(connStr, "@tcp(") {
		// Extract parts from DSN format
		parts := strings.SplitN(connStr, "@tcp(", 2)
		if len(parts) == 2 {
			userPass := parts[0]
			rest := parts[1]
			// rest is "host:port)/dbname?params"
			hostAndRest := strings.SplitN(rest, ")/", 2)
			if len(hostAndRest) == 2 {
				host := hostAndRest[0]
				dbAndParams := hostAndRest[1]
				mysqlConnStr = "mysql://" + userPass + "@" + host + "/" + dbAndParams
			}
		}
	}
	if !strings.HasPrefix(mysqlConnStr, "mysql://") {
		mysqlConnStr = "mysql://" + mysqlConnStr
	}

	// Create app with the connection string and any additional options
	allOpts := append([]Option{WithDatabase(mysqlConnStr)}, opts...)
	app := NewApp(allOpts...)

	cleanup := func() {
		_ = app.Shutdown(context.Background())
		testcontainers.CleanupContainer(t, container)
	}

	return app, cleanup
}
