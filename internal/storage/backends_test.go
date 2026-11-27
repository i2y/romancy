package storage

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"
)

// storageBackend opens a fresh, fully migrated, empty database for one test.
// SQLite is always available; integration builds (-tags integration) append
// PostgreSQL and MySQL (see backends_integration_test.go) so the same test
// bodies exercise every dialect's implementation of the Storage interface.
type storageBackend struct {
	name string
	open func(t *testing.T) *testDB
}

var storageBackends = []storageBackend{
	{name: "sqlite", open: openSQLiteBackend},
}

// testDB is a Storage plus the raw handle a few tests need to fake state
// (backdating timestamps, forcing terminal statuses).
type testDB struct {
	Storage
	db          *sql.DB
	placeholder func(n int) string
}

// rebind rewrites ? placeholders into the dialect's placeholder syntax.
func (d *testDB) rebind(query string) string {
	var b strings.Builder
	n := 0
	for _, r := range query {
		if r == '?' {
			n++
			b.WriteString(d.placeholder(n))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// exec runs dialect-neutral SQL written with ? placeholders.
func (d *testDB) exec(t *testing.T, ctx context.Context, query string, args ...any) {
	t.Helper()
	if _, err := d.db.ExecContext(ctx, d.rebind(query), args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

// queryString returns a single string column from dialect-neutral SQL.
func (d *testDB) queryString(t *testing.T, ctx context.Context, query string, args ...any) string {
	t.Helper()
	var v string
	if err := d.db.QueryRowContext(ctx, d.rebind(query), args...).Scan(&v); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return v
}

// forEachBackend runs fn as a subtest against every configured backend.
func forEachBackend(t *testing.T, fn func(t *testing.T, ctx context.Context, d *testDB)) {
	t.Helper()
	for _, b := range storageBackends {
		t.Run(b.name, func(t *testing.T) {
			fn(t, context.Background(), b.open(t))
		})
	}
}

func openSQLiteBackend(t *testing.T) *testDB {
	t.Helper()
	s, _ := newClaimTestStorage(t)
	return &testDB{Storage: s, db: s.DB(), placeholder: s.driver.Placeholder}
}

// createTestInstance creates a running instance through the Storage API.
func createTestInstance(t *testing.T, ctx context.Context, s Storage, instanceID, workflowName string) {
	t.Helper()
	now := time.Now()
	if err := s.CreateInstance(ctx, &WorkflowInstance{
		InstanceID:   instanceID,
		WorkflowName: workflowName,
		Status:       StatusRunning,
		InputData:    []byte(`{}`),
		StartedAt:    now,
		UpdatedAt:    now,
	}); err != nil {
		t.Fatalf("failed to create instance %s: %v", instanceID, err)
	}
}
