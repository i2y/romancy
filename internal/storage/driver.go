package storage

import "fmt"

// Driver abstracts database-specific SQL operations.
type Driver interface {
	// DriverName returns the driver name (e.g., "sqlite", "postgres")
	DriverName() string

	// GetCurrentTimeExpr returns the SQL expression for current time.
	// SQLite: datetime('now'), PostgreSQL: NOW()
	GetCurrentTimeExpr() string

	// MakeDatetimeComparable wraps a datetime column for comparison.
	// SQLite needs datetime() wrapper, PostgreSQL doesn't.
	MakeDatetimeComparable(column string) string

	// SelectForUpdateSkipLocked returns the clause for row locking.
	// PostgreSQL: "FOR UPDATE SKIP LOCKED", SQLite: "" (uses table-level locking)
	SelectForUpdateSkipLocked() string

	// Placeholder returns the placeholder for the nth parameter.
	// SQLite: ?, PostgreSQL: $n
	Placeholder(n int) string

	// OnConflictDoNothing returns the clause for idempotent inserts.
	// SQLite: "ON CONFLICT DO NOTHING", PostgreSQL: "ON CONFLICT DO NOTHING"
	OnConflictDoNothing(conflictColumns ...string) string

	// ReturningClause returns the RETURNING clause for insert/update.
	// SQLite: "", PostgreSQL: "RETURNING id"
	ReturningClause(columns ...string) string
}

// SQLiteDriver implements Driver for SQLite.
type SQLiteDriver struct{}

func (d *SQLiteDriver) DriverName() string {
	return "sqlite"
}

func (d *SQLiteDriver) GetCurrentTimeExpr() string {
	return "datetime('now')"
}

func (d *SQLiteDriver) MakeDatetimeComparable(column string) string {
	return fmt.Sprintf("datetime(%s)", column)
}

func (d *SQLiteDriver) SelectForUpdateSkipLocked() string {
	// SQLite uses table-level locking, no row-level locking support
	return ""
}

func (d *SQLiteDriver) Placeholder(n int) string {
	return "?"
}

func (d *SQLiteDriver) OnConflictDoNothing(conflictColumns ...string) string {
	if len(conflictColumns) == 0 {
		return "ON CONFLICT DO NOTHING"
	}
	cols := ""
	for i, c := range conflictColumns {
		if i > 0 {
			cols += ", "
		}
		cols += c
	}
	return fmt.Sprintf("ON CONFLICT (%s) DO NOTHING", cols)
}

func (d *SQLiteDriver) ReturningClause(columns ...string) string {
	// SQLite 3.35+ supports RETURNING, but we'll use it minimally
	if len(columns) == 0 {
		return ""
	}
	result := "RETURNING "
	for i, c := range columns {
		if i > 0 {
			result += ", "
		}
		result += c
	}
	return result
}

// PostgresDriver implements Driver for PostgreSQL.
type PostgresDriver struct{}

func (d *PostgresDriver) DriverName() string {
	return "postgres"
}

func (d *PostgresDriver) GetCurrentTimeExpr() string {
	return "NOW()"
}

func (d *PostgresDriver) MakeDatetimeComparable(column string) string {
	// PostgreSQL timestamps are already comparable
	return column
}

func (d *PostgresDriver) SelectForUpdateSkipLocked() string {
	return "FOR UPDATE SKIP LOCKED"
}

func (d *PostgresDriver) Placeholder(n int) string {
	return fmt.Sprintf("$%d", n)
}

func (d *PostgresDriver) OnConflictDoNothing(conflictColumns ...string) string {
	if len(conflictColumns) == 0 {
		return "ON CONFLICT DO NOTHING"
	}
	cols := ""
	for i, c := range conflictColumns {
		if i > 0 {
			cols += ", "
		}
		cols += c
	}
	return fmt.Sprintf("ON CONFLICT (%s) DO NOTHING", cols)
}

func (d *PostgresDriver) ReturningClause(columns ...string) string {
	if len(columns) == 0 {
		return ""
	}
	result := "RETURNING "
	for i, c := range columns {
		if i > 0 {
			result += ", "
		}
		result += c
	}
	return result
}

// MySQLDriver implements Driver for MySQL 8.0+.
type MySQLDriver struct{}

func (d *MySQLDriver) DriverName() string {
	return "mysql"
}

func (d *MySQLDriver) GetCurrentTimeExpr() string {
	return "NOW()"
}

func (d *MySQLDriver) MakeDatetimeComparable(column string) string {
	// MySQL timestamps are directly comparable
	return column
}

func (d *MySQLDriver) SelectForUpdateSkipLocked() string {
	return "FOR UPDATE SKIP LOCKED"
}

func (d *MySQLDriver) Placeholder(n int) string {
	return "?"
}

func (d *MySQLDriver) OnConflictDoNothing(conflictColumns ...string) string {
	// MySQL doesn't support ON CONFLICT; use INSERT IGNORE instead
	return ""
}

func (d *MySQLDriver) ReturningClause(columns ...string) string {
	// MySQL doesn't support RETURNING clause; use LastInsertId() instead
	return ""
}

// NewDriver creates a new driver based on the database URL.
func NewDriver(dbURL string) Driver {
	if len(dbURL) >= 8 && dbURL[:8] == "postgres" {
		return &PostgresDriver{}
	}
	if len(dbURL) >= 5 && dbURL[:5] == "mysql" {
		return &MySQLDriver{}
	}
	// Default to SQLite
	return &SQLiteDriver{}
}
