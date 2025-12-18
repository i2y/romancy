package migrations

import (
	"context"
	"database/sql"
	"io/fs"
	"testing"
	"testing/fstest"

	_ "modernc.org/sqlite"
)

func TestDetectDBType(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		want    string
		wantErr bool
	}{
		{"sqlite with file: prefix", "file:test.db", "sqlite", false},
		{"sqlite with .db suffix", "test.db", "sqlite", false},
		{"sqlite with sqlite:// prefix", "sqlite://test.db", "sqlite", false},
		{"postgres with postgres://", "postgres://user:pass@localhost/db", "postgresql", false},
		{"postgres with postgresql://", "postgresql://user:pass@localhost/db", "postgresql", false},
		{"mysql with mysql://", "mysql://user:pass@localhost/db", "mysql", false},
		{"unknown database", "oracle://user:pass@localhost/db", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DetectDBType(tt.url)
			if (err != nil) != tt.wantErr {
				t.Errorf("DetectDBType() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("DetectDBType() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestExtractVersionFromFilename(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		want     string
	}{
		{"standard dbmate format", "20251217000000_initial_schema.sql", "20251217000000"},
		{"with description", "20240101123456_add_users_table.sql", "20240101123456"},
		{"no underscore", "20251217000000.sql", "20251217000000"},
		{"no version prefix", "initial_schema.sql", "initial_schema"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExtractVersionFromFilename(tt.filename); got != tt.want {
				t.Errorf("ExtractVersionFromFilename() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseMigrationFile(t *testing.T) {
	tests := []struct {
		name      string
		content   string
		wantUp    string
		wantDown  string
	}{
		{
			name: "standard format",
			content: `-- migrate:up
CREATE TABLE users (id INTEGER PRIMARY KEY);

-- migrate:down
DROP TABLE users;
`,
			wantUp:   "CREATE TABLE users (id INTEGER PRIMARY KEY);",
			wantDown: "DROP TABLE users;",
		},
		{
			name: "only migrate:up",
			content: `-- migrate:up
CREATE TABLE users (id INTEGER PRIMARY KEY);
`,
			wantUp:   "CREATE TABLE users (id INTEGER PRIMARY KEY);",
			wantDown: "",
		},
		{
			name:     "empty file",
			content:  "",
			wantUp:   "",
			wantDown: "",
		},
		{
			name: "multiline SQL",
			content: `-- migrate:up
CREATE TABLE users (
    id INTEGER PRIMARY KEY,
    name TEXT NOT NULL,
    email TEXT UNIQUE
);

CREATE INDEX idx_users_email ON users(email);

-- migrate:down
DROP INDEX idx_users_email;
DROP TABLE users;
`,
			wantUp: `CREATE TABLE users (
    id INTEGER PRIMARY KEY,
    name TEXT NOT NULL,
    email TEXT UNIQUE
);

CREATE INDEX idx_users_email ON users(email);`,
			wantDown: `DROP INDEX idx_users_email;
DROP TABLE users;`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotUp, gotDown := ParseMigrationFile(tt.content)
			if gotUp != tt.wantUp {
				t.Errorf("ParseMigrationFile() up = %q, want %q", gotUp, tt.wantUp)
			}
			if gotDown != tt.wantDown {
				t.Errorf("ParseMigrationFile() down = %q, want %q", gotDown, tt.wantDown)
			}
		})
	}
}

func TestApplyMigrations_SQLite(t *testing.T) {
	// Create in-memory SQLite database
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer db.Close()

	// Create test migrations filesystem
	testFS := fstest.MapFS{
		"sqlite/20240101000000_create_users.sql": &fstest.MapFile{
			Data: []byte(`-- migrate:up
CREATE TABLE users (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL
);

-- migrate:down
DROP TABLE users;
`),
		},
		"sqlite/20240102000000_add_email.sql": &fstest.MapFile{
			Data: []byte(`-- migrate:up
ALTER TABLE users ADD COLUMN email TEXT;

-- migrate:down
-- SQLite doesn't support DROP COLUMN
`),
		},
	}

	ctx := context.Background()

	// Apply migrations
	applied, err := ApplyMigrations(ctx, db, "sqlite", testFS)
	if err != nil {
		t.Fatalf("ApplyMigrations() error = %v", err)
	}

	// Should have applied 2 migrations
	if len(applied) != 2 {
		t.Errorf("ApplyMigrations() applied %d migrations, want 2", len(applied))
	}

	// Verify schema_migrations table
	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&count)
	if err != nil {
		t.Fatalf("failed to query schema_migrations: %v", err)
	}
	if count != 2 {
		t.Errorf("schema_migrations has %d rows, want 2", count)
	}

	// Verify users table was created with email column
	_, err = db.Exec("INSERT INTO users (name, email) VALUES ('test', 'test@example.com')")
	if err != nil {
		t.Errorf("failed to insert into users: %v", err)
	}

	// Apply migrations again - should be idempotent
	applied2, err := ApplyMigrations(ctx, db, "sqlite", testFS)
	if err != nil {
		t.Fatalf("ApplyMigrations() second run error = %v", err)
	}
	if len(applied2) != 0 {
		t.Errorf("ApplyMigrations() second run applied %d migrations, want 0", len(applied2))
	}
}

func TestApplyMigrations_NoMigrationsFS(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer db.Close()

	ctx := context.Background()

	// Apply with nil filesystem
	applied, err := ApplyMigrations(ctx, db, "sqlite", nil)
	if err != nil {
		t.Fatalf("ApplyMigrations() error = %v", err)
	}
	if len(applied) != 0 {
		t.Errorf("ApplyMigrations() applied %d migrations, want 0", len(applied))
	}
}

func TestApplyMigrations_MissingDirectory(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer db.Close()

	// Create empty filesystem (no sqlite directory)
	testFS := fstest.MapFS{}

	ctx := context.Background()

	// Should not error, just warn and skip
	applied, err := ApplyMigrations(ctx, db, "sqlite", testFS)
	if err != nil {
		t.Fatalf("ApplyMigrations() error = %v", err)
	}
	if len(applied) != 0 {
		t.Errorf("ApplyMigrations() applied %d migrations, want 0", len(applied))
	}
}

func TestRecordMigration_DuplicateHandling(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer db.Close()

	ctx := context.Background()

	// Create schema_migrations table
	err = EnsureSchemaMigrationsTable(ctx, db, "sqlite")
	if err != nil {
		t.Fatalf("EnsureSchemaMigrationsTable() error = %v", err)
	}

	// First record should succeed
	recorded, err := RecordMigration(ctx, db, "sqlite", "20240101000000")
	if err != nil {
		t.Fatalf("RecordMigration() first call error = %v", err)
	}
	if !recorded {
		t.Error("RecordMigration() first call should return true")
	}

	// Second record of same version should return false (duplicate)
	recorded2, err := RecordMigration(ctx, db, "sqlite", "20240101000000")
	if err != nil {
		t.Fatalf("RecordMigration() second call error = %v", err)
	}
	if recorded2 {
		t.Error("RecordMigration() second call should return false (duplicate)")
	}
}

func TestExecuteSQLStatements(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer db.Close()

	ctx := context.Background()

	// Test multiple statements
	sql := `
CREATE TABLE test1 (id INTEGER PRIMARY KEY);
CREATE TABLE test2 (id INTEGER PRIMARY KEY);
-- This is a comment
CREATE INDEX idx_test1 ON test1(id);
`

	err = ExecuteSQLStatements(ctx, db, sql, "sqlite")
	if err != nil {
		t.Fatalf("ExecuteSQLStatements() error = %v", err)
	}

	// Verify tables were created
	var name string
	err = db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name='test1'").Scan(&name)
	if err != nil {
		t.Error("test1 table was not created")
	}
	err = db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name='test2'").Scan(&name)
	if err != nil {
		t.Error("test2 table was not created")
	}
}

func TestGetAppliedMigrations(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer db.Close()

	ctx := context.Background()

	// Before table exists - should return empty map
	applied, err := GetAppliedMigrations(ctx, db)
	if err != nil {
		t.Fatalf("GetAppliedMigrations() before table error = %v", err)
	}
	if len(applied) != 0 {
		t.Errorf("GetAppliedMigrations() before table = %d, want 0", len(applied))
	}

	// Create table and add some migrations
	err = EnsureSchemaMigrationsTable(ctx, db, "sqlite")
	if err != nil {
		t.Fatalf("EnsureSchemaMigrationsTable() error = %v", err)
	}

	_, _ = RecordMigration(ctx, db, "sqlite", "20240101000000")
	_, _ = RecordMigration(ctx, db, "sqlite", "20240102000000")

	// Should return both migrations
	applied, err = GetAppliedMigrations(ctx, db)
	if err != nil {
		t.Fatalf("GetAppliedMigrations() after inserts error = %v", err)
	}
	if len(applied) != 2 {
		t.Errorf("GetAppliedMigrations() after inserts = %d, want 2", len(applied))
	}
	if !applied["20240101000000"] {
		t.Error("GetAppliedMigrations() missing 20240101000000")
	}
	if !applied["20240102000000"] {
		t.Error("GetAppliedMigrations() missing 20240102000000")
	}
}

// Helper to suppress unused import warning
var _ fs.FS
