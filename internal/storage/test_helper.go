// Package storage provides the storage layer for Romancy.
package storage

import (
	"context"
	"database/sql"

	"github.com/i2y/romancy/internal/migrations"
)

// applyTestSchema applies the real embedded dbmate migrations — the same
// files production auto-migration runs — so test databases can never drift
// from the shared durax-io/schema definitions. (Hand-maintained schema
// copies used to live here; they silently diverged from the migrations and
// broke every PostgreSQL/MySQL integration test.)
func applyTestSchema(ctx context.Context, db *sql.DB, dbType string) error {
	_, err := migrations.ApplyMigrations(ctx, db, dbType, migrations.EmbeddedFS())
	return err
}

// InitializeTestSchema applies the schema migrations to an SQLiteStorage.
// This is intended for use in tests only.
func InitializeTestSchema(ctx context.Context, s *SQLiteStorage) error {
	return applyTestSchema(ctx, s.DB(), "sqlite")
}

// InitializeTestSchemaForStorage initializes the test schema for any Storage interface.
// This is useful for external test packages.
func InitializeTestSchemaForStorage(ctx context.Context, s Storage) error {
	if sqliteStorage, ok := s.(*SQLiteStorage); ok {
		return InitializeTestSchema(ctx, sqliteStorage)
	}
	if pgStorage, ok := s.(*PostgresStorage); ok {
		return InitializeTestSchemaPostgres(ctx, pgStorage)
	}
	if mysqlStorage, ok := s.(*MySQLStorage); ok {
		return InitializeTestSchemaMySQL(ctx, mysqlStorage)
	}
	return nil
}

// InitializeTestSchemaPostgres applies the schema migrations to a PostgresStorage.
// This is intended for use in tests only.
func InitializeTestSchemaPostgres(ctx context.Context, s *PostgresStorage) error {
	return applyTestSchema(ctx, s.DB(), "postgresql")
}

// InitializeTestSchemaMySQL applies the schema migrations to a MySQLStorage.
// This is intended for use in tests only.
func InitializeTestSchemaMySQL(ctx context.Context, s *MySQLStorage) error {
	return applyTestSchema(ctx, s.DB(), "mysql")
}
