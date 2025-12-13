// Package migrations provides database migration support for Romancy.
package migrations

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"
	"github.com/golang-migrate/migrate/v4/database/mysql"
	"github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/database/sqlite"
	"github.com/golang-migrate/migrate/v4/source"
	"github.com/golang-migrate/migrate/v4/source/iofs"

	mysqlmigrations "github.com/i2y/romancy/internal/migrations/mysql"
	postgresmigrations "github.com/i2y/romancy/internal/migrations/postgres"
	sqlitemigrations "github.com/i2y/romancy/internal/migrations/sqlite"
)

// DriverType represents the database driver type.
type DriverType string

const (
	// DriverSQLite represents SQLite database.
	DriverSQLite DriverType = "sqlite"
	// DriverPostgres represents PostgreSQL database.
	DriverPostgres DriverType = "postgres"
	// DriverMySQL represents MySQL database.
	DriverMySQL DriverType = "mysql"
)

// Migrator handles database migrations.
type Migrator struct {
	db         *sql.DB
	driverType DriverType
}

// NewMigrator creates a new Migrator instance.
func NewMigrator(db *sql.DB, driverType DriverType) *Migrator {
	return &Migrator{
		db:         db,
		driverType: driverType,
	}
}

// Up runs all pending migrations.
// Note: We intentionally don't call migrator.Close() here because it closes
// the underlying database connection, which we need to keep open.
func (m *Migrator) Up() error {
	migrator, err := m.createMigrate()
	if err != nil {
		return fmt.Errorf("failed to create migrator: %w", err)
	}

	// Handle existing databases that predate migrations
	if err := m.handleExistingDatabase(migrator); err != nil {
		return fmt.Errorf("failed to handle existing database: %w", err)
	}

	if err := migrator.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("failed to run migrations: %w", err)
	}
	return nil
}

// Down rolls back all migrations.
func (m *Migrator) Down() error {
	migrator, err := m.createMigrate()
	if err != nil {
		return fmt.Errorf("failed to create migrator: %w", err)
	}

	if err := migrator.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("failed to rollback migrations: %w", err)
	}
	return nil
}

// Version returns the current migration version.
// Returns (version, dirty, error).
func (m *Migrator) Version() (version uint, dirty bool, err error) {
	migrator, err := m.createMigrate()
	if err != nil {
		return 0, false, fmt.Errorf("failed to create migrator: %w", err)
	}

	return migrator.Version()
}

// Steps runs n migrations (positive = up, negative = down).
func (m *Migrator) Steps(n int) error {
	migrator, err := m.createMigrate()
	if err != nil {
		return fmt.Errorf("failed to create migrator: %w", err)
	}

	if err := migrator.Steps(n); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("failed to run migration steps: %w", err)
	}
	return nil
}

// Force sets the migration version without running migrations.
// Use with caution - this is primarily for handling existing databases.
func (m *Migrator) Force(version int) error {
	migrator, err := m.createMigrate()
	if err != nil {
		return fmt.Errorf("failed to create migrator: %w", err)
	}

	return migrator.Force(version)
}

// handleExistingDatabase handles databases that existed before migrations were added.
// If tables exist but no schema_migrations table, we assume version 1 (init).
func (m *Migrator) handleExistingDatabase(migrator *migrate.Migrate) error {
	_, _, err := migrator.Version()
	if err == nil {
		// Version exists, nothing to do
		return nil
	}

	if !errors.Is(err, migrate.ErrNilVersion) {
		return err
	}

	// No version set - check if workflow_instances table exists
	if m.tablesExist() {
		// Existing database without migration tracking - force version 1
		if err := migrator.Force(1); err != nil {
			return fmt.Errorf("failed to force version for existing database: %w", err)
		}
	}

	return nil
}

// tablesExist checks if the core Romancy tables already exist.
func (m *Migrator) tablesExist() bool {
	var query string
	switch m.driverType {
	case DriverSQLite:
		query = "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='workflow_instances'"
	case DriverPostgres:
		query = "SELECT COUNT(*) FROM information_schema.tables WHERE table_name='workflow_instances'"
	case DriverMySQL:
		query = "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = 'workflow_instances'"
	default:
		return false
	}

	var count int
	if err := m.db.QueryRow(query).Scan(&count); err != nil {
		return false
	}
	return count > 0
}

// createMigrate creates the migrate instance with appropriate drivers.
func (m *Migrator) createMigrate() (*migrate.Migrate, error) {
	var sourceDriver source.Driver
	var dbDriver database.Driver
	var err error

	switch m.driverType {
	case DriverSQLite:
		sourceDriver, err = iofs.New(sqlitemigrations.MigrationsFS, ".")
		if err != nil {
			return nil, fmt.Errorf("failed to create SQLite source driver: %w", err)
		}
		dbDriver, err = sqlite.WithInstance(m.db, &sqlite.Config{})
		if err != nil {
			return nil, fmt.Errorf("failed to create SQLite database driver: %w", err)
		}

	case DriverPostgres:
		sourceDriver, err = iofs.New(postgresmigrations.MigrationsFS, ".")
		if err != nil {
			return nil, fmt.Errorf("failed to create PostgreSQL source driver: %w", err)
		}
		dbDriver, err = pgx.WithInstance(m.db, &pgx.Config{})
		if err != nil {
			return nil, fmt.Errorf("failed to create PostgreSQL database driver: %w", err)
		}

	case DriverMySQL:
		sourceDriver, err = iofs.New(mysqlmigrations.MigrationsFS, ".")
		if err != nil {
			return nil, fmt.Errorf("failed to create MySQL source driver: %w", err)
		}
		dbDriver, err = mysql.WithInstance(m.db, &mysql.Config{})
		if err != nil {
			return nil, fmt.Errorf("failed to create MySQL database driver: %w", err)
		}

	default:
		return nil, fmt.Errorf("unsupported driver type: %s", m.driverType)
	}

	return migrate.NewWithInstance("iofs", sourceDriver, string(m.driverType), dbDriver)
}
