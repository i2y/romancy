package migrations

import (
	"database/sql"
	"os"
	"testing"

	_ "modernc.org/sqlite"
)

func TestMigrator_SQLite_Up(t *testing.T) {
	// Create temporary database file
	tmpFile, err := os.CreateTemp("", "romancy-migrate-test-*.db")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	_ = tmpFile.Close()
	defer func() { _ = os.Remove(tmpFile.Name()) }()

	db, err := sql.Open("sqlite", tmpFile.Name())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer func() { _ = db.Close() }()

	migrator := NewMigrator(db, DriverSQLite)
	if err := migrator.Up(); err != nil {
		t.Fatalf("failed to run migrations: %v", err)
	}

	// Verify tables exist
	tables := []string{
		"workflow_instances",
		"workflow_history",
		"workflow_event_subscriptions",
		"workflow_timer_subscriptions",
		"workflow_outbox",
		"workflow_compensations",
	}

	for _, table := range tables {
		var count int
		err = db.QueryRow(
			"SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?",
			table,
		).Scan(&count)
		if err != nil {
			t.Fatalf("failed to query for table %s: %v", table, err)
		}
		if count != 1 {
			t.Errorf("table %s should exist, but count=%d", table, count)
		}
	}
}

func TestMigrator_SQLite_Version(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "romancy-version-test-*.db")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	_ = tmpFile.Close()
	defer func() { _ = os.Remove(tmpFile.Name()) }()

	db, err := sql.Open("sqlite", tmpFile.Name())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer func() { _ = db.Close() }()

	migrator := NewMigrator(db, DriverSQLite)

	// Run migrations
	if err := migrator.Up(); err != nil {
		t.Fatalf("failed to run migrations: %v", err)
	}

	// Check version
	version, dirty, err := migrator.Version()
	if err != nil {
		t.Fatalf("failed to get version: %v", err)
	}
	if version != 6 {
		t.Errorf("expected version 6, got %d", version)
	}
	if dirty {
		t.Errorf("expected dirty=false, got dirty=true")
	}
}

func TestMigrator_SQLite_Idempotent(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "romancy-idempotent-test-*.db")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	_ = tmpFile.Close()
	defer func() { _ = os.Remove(tmpFile.Name()) }()

	db, err := sql.Open("sqlite", tmpFile.Name())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer func() { _ = db.Close() }()

	migrator := NewMigrator(db, DriverSQLite)

	// Run Up twice - should be idempotent
	if err := migrator.Up(); err != nil {
		t.Fatalf("first Up failed: %v", err)
	}

	if err := migrator.Up(); err != nil {
		t.Fatalf("second Up failed: %v", err)
	}

	// Verify version is still the latest
	version, _, err := migrator.Version()
	if err != nil {
		t.Fatalf("failed to get version: %v", err)
	}
	if version != 6 {
		t.Errorf("expected version 6 after idempotent Up, got %d", version)
	}
}

func TestMigrator_SQLite_ExistingDatabase(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "romancy-existing-test-*.db")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	_ = tmpFile.Close()
	defer func() { _ = os.Remove(tmpFile.Name()) }()

	db, err := sql.Open("sqlite", tmpFile.Name())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer func() { _ = db.Close() }()

	// Create tables without using migrations (simulating pre-migration database)
	// Include all tables and columns that subsequent migrations might need to reference
	_, err = db.Exec(`
		CREATE TABLE workflow_instances (
			instance_id TEXT PRIMARY KEY,
			workflow_name TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'pending',
			input_data TEXT,
			output_data TEXT,
			error_message TEXT,
			current_activity_id TEXT,
			locked_by TEXT,
			locked_at DATETIME,
			lock_timeout_seconds INTEGER,
			lock_expires_at DATETIME,
			created_at DATETIME NOT NULL DEFAULT (datetime('now')),
			updated_at DATETIME NOT NULL DEFAULT (datetime('now'))
		);
		CREATE TABLE workflow_timer_subscriptions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			instance_id TEXT NOT NULL,
			timer_id TEXT NOT NULL,
			expires_at DATETIME NOT NULL,
			step INTEGER NOT NULL,
			created_at DATETIME NOT NULL DEFAULT (datetime('now'))
		);
		CREATE TABLE workflow_compensations (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			instance_id TEXT NOT NULL,
			activity_id TEXT NOT NULL,
			compensation_fn TEXT NOT NULL,
			compensation_arg BLOB,
			comp_order INTEGER NOT NULL,
			status TEXT NOT NULL DEFAULT 'pending',
			created_at DATETIME NOT NULL DEFAULT (datetime('now'))
		);
		CREATE TABLE channel_messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			channel_name TEXT NOT NULL,
			data_json TEXT,
			data_binary BLOB,
			metadata TEXT,
			target_instance_id TEXT,
			created_at DATETIME NOT NULL DEFAULT (datetime('now'))
		);
		CREATE TABLE channel_subscriptions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			instance_id TEXT NOT NULL,
			channel_name TEXT NOT NULL,
			mode TEXT NOT NULL DEFAULT 'broadcast',
			waiting INTEGER NOT NULL DEFAULT 0,
			timeout_at DATETIME,
			created_at DATETIME NOT NULL DEFAULT (datetime('now'))
		);
		CREATE TABLE channel_delivery_cursors (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			instance_id TEXT NOT NULL,
			channel_name TEXT NOT NULL,
			last_message_id INTEGER NOT NULL DEFAULT 0,
			updated_at DATETIME NOT NULL DEFAULT (datetime('now'))
		);
		CREATE TABLE channel_message_claims (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			message_id INTEGER NOT NULL,
			instance_id TEXT NOT NULL,
			claimed_at DATETIME NOT NULL DEFAULT (datetime('now'))
		);
		CREATE TABLE workflow_outbox (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			event_id TEXT UNIQUE NOT NULL,
			event_type TEXT NOT NULL,
			event_source TEXT NOT NULL,
			data_type TEXT NOT NULL,
			event_data TEXT,
			content_type TEXT,
			status TEXT NOT NULL DEFAULT 'pending',
			attempts INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL DEFAULT (datetime('now')),
			updated_at DATETIME NOT NULL DEFAULT (datetime('now'))
		);
		CREATE TABLE workflow_group_memberships (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			instance_id TEXT NOT NULL,
			group_name TEXT NOT NULL,
			created_at DATETIME NOT NULL DEFAULT (datetime('now'))
		);
	`)
	if err != nil {
		t.Fatalf("failed to create pre-existing tables: %v", err)
	}

	migrator := NewMigrator(db, DriverSQLite)

	// Should detect existing tables, force version 1, then migrate to version 2
	if err := migrator.Up(); err != nil {
		t.Fatalf("failed to run migrations on existing database: %v", err)
	}

	version, _, err := migrator.Version()
	if err != nil {
		t.Fatalf("failed to get version: %v", err)
	}
	if version != 6 {
		t.Errorf("expected version 6 for existing database after migration, got %d", version)
	}
}

func TestMigrator_SQLite_UpDown(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "romancy-updown-test-*.db")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	_ = tmpFile.Close()
	defer func() { _ = os.Remove(tmpFile.Name()) }()

	db, err := sql.Open("sqlite", tmpFile.Name())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer func() { _ = db.Close() }()

	migrator := NewMigrator(db, DriverSQLite)

	// Up
	if err := migrator.Up(); err != nil {
		t.Fatalf("Up failed: %v", err)
	}

	// Verify tables exist
	var count int
	err = db.QueryRow(
		"SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='workflow_instances'",
	).Scan(&count)
	if err != nil {
		t.Fatalf("failed to query tables: %v", err)
	}
	if count != 1 {
		t.Error("workflow_instances table should exist after Up")
	}

	// Down
	if err := migrator.Down(); err != nil {
		t.Fatalf("Down failed: %v", err)
	}

	// Verify tables are gone
	err = db.QueryRow(
		"SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='workflow_instances'",
	).Scan(&count)
	if err != nil {
		t.Fatalf("failed to query tables after Down: %v", err)
	}
	if count != 0 {
		t.Error("workflow_instances table should not exist after Down")
	}
}

func TestMigrator_TablesExist(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "romancy-tables-exist-test-*.db")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	_ = tmpFile.Close()
	defer func() { _ = os.Remove(tmpFile.Name()) }()

	db, err := sql.Open("sqlite", tmpFile.Name())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer func() { _ = db.Close() }()

	migrator := NewMigrator(db, DriverSQLite)

	// Initially tables don't exist
	if migrator.tablesExist() {
		t.Error("tablesExist should return false on empty database")
	}

	// Create the table
	_, err = db.Exec(`CREATE TABLE workflow_instances (instance_id TEXT PRIMARY KEY)`)
	if err != nil {
		t.Fatalf("failed to create table: %v", err)
	}

	// Now tables exist
	if !migrator.tablesExist() {
		t.Error("tablesExist should return true after creating workflow_instances")
	}
}
