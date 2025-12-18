// Package migrations provides automatic dbmate-compatible migration support.
//
// This package provides functions to automatically apply dbmate migration files
// at application startup, eliminating the need to manually run `dbmate up`.
//
// The migration system is compatible with dbmate:
//   - Uses the same `schema_migrations` table for tracking applied migrations
//   - Reads the same `-- migrate:up` / `-- migrate:down` SQL format
//   - Supports SQLite, PostgreSQL, and MySQL
package migrations

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// DetectDBType detects database type from connection URL.
//
// Returns one of "sqlite", "postgresql", "mysql".
func DetectDBType(url string) (string, error) {
	url = strings.ToLower(url)

	if strings.Contains(url, "sqlite") || strings.HasPrefix(url, "file:") || strings.HasSuffix(url, ".db") {
		return "sqlite", nil
	}
	if strings.HasPrefix(url, "postgres") {
		return "postgresql", nil
	}
	if strings.HasPrefix(url, "mysql") {
		return "mysql", nil
	}

	return "", fmt.Errorf("cannot detect database type from URL: %s", url)
}

// EnsureSchemaMigrationsTable creates the schema_migrations table if it doesn't exist.
// This table is compatible with dbmate's migration tracking.
// Safe for concurrent execution by multiple workers.
func EnsureSchemaMigrationsTable(ctx context.Context, db *sql.DB, dbType string) error {
	// All databases use the same schema (VARCHAR(255) is universal)
	createSQL := `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version VARCHAR(255) PRIMARY KEY
		)
	`

	_, err := db.ExecContext(ctx, createSQL)
	if err != nil {
		// Handle concurrent table creation - another worker might have created it
		errMsg := strings.ToLower(err.Error())
		if strings.Contains(errMsg, "already exists") ||
			strings.Contains(errMsg, "duplicate") ||
			strings.Contains(errMsg, "42p07") { // PostgreSQL: relation already exists
			return nil
		}
		return err
	}
	return nil
}

// GetAppliedMigrations returns a set of already applied migration versions.
func GetAppliedMigrations(ctx context.Context, db *sql.DB) (map[string]bool, error) {
	applied := make(map[string]bool)

	rows, err := db.QueryContext(ctx, "SELECT version FROM schema_migrations")
	if err != nil {
		// Table might not exist yet
		return applied, nil
	}
	defer rows.Close()

	for rows.Next() {
		var version string
		if err := rows.Scan(&version); err != nil {
			return nil, err
		}
		applied[version] = true
	}

	return applied, rows.Err()
}

// RecordMigration records a migration as applied.
//
// Returns true if recorded successfully, false if already recorded (race condition).
func RecordMigration(ctx context.Context, db *sql.DB, dbType string, version string) (bool, error) {
	// Use appropriate placeholder syntax
	var query string
	if dbType == "postgresql" {
		query = "INSERT INTO schema_migrations (version) VALUES ($1)"
	} else {
		query = "INSERT INTO schema_migrations (version) VALUES (?)"
	}
	_, err := db.ExecContext(ctx, query, version)
	if err != nil {
		// Handle race condition: another worker already recorded this migration
		errMsg := strings.ToLower(err.Error())
		if strings.Contains(errMsg, "unique") ||
			strings.Contains(errMsg, "duplicate") ||
			strings.Contains(errMsg, "constraint") {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// ExtractVersionFromFilename extracts version from migration filename.
//
// dbmate uses format: YYYYMMDDHHMMSS_description.sql
// Example: "20251217000000_initial_schema.sql" -> "20251217000000"
func ExtractVersionFromFilename(filename string) string {
	// Match leading digits followed by underscore
	re := regexp.MustCompile(`^(\d+)_`)
	match := re.FindStringSubmatch(filename)
	if len(match) > 1 {
		return match[1]
	}
	// Fallback: remove .sql extension
	return strings.TrimSuffix(filename, ".sql")
}

// ParseMigrationFile parses dbmate migration file content.
//
// Returns (upSQL, downSQL) extracted from the file.
func ParseMigrationFile(content string) (upSQL string, downSQL string) {
	// Extract "-- migrate:up" section
	upRe := regexp.MustCompile(`(?s)-- migrate:up\s*(.*?)(?:-- migrate:down|$)`)
	upMatch := upRe.FindStringSubmatch(content)
	if len(upMatch) > 1 {
		upSQL = strings.TrimSpace(upMatch[1])
	}

	// Extract "-- migrate:down" section
	downRe := regexp.MustCompile(`(?s)-- migrate:down\s*(.*)$`)
	downMatch := downRe.FindStringSubmatch(content)
	if len(downMatch) > 1 {
		downSQL = strings.TrimSpace(downMatch[1])
	}

	return upSQL, downSQL
}

// ExecuteSQLStatements executes SQL statements from migration file.
//
// Handles:
//   - Multiple statements separated by semicolons
//   - Comment lines
//   - "already exists" errors (idempotent)
func ExecuteSQLStatements(ctx context.Context, db *sql.DB, sqlContent string, dbType string) error {
	if sqlContent == "" {
		return nil
	}

	// Split by semicolons
	statements := splitSQLStatements(sqlContent)

	for _, stmt := range statements {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}

		// Remove leading comment-only lines
		lines := strings.Split(stmt, "\n")
		var sqlLines []string
		for _, line := range lines {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "--") {
				continue
			}
			sqlLines = append(sqlLines, line)
		}

		actualSQL := strings.TrimSpace(strings.Join(sqlLines, "\n"))
		if actualSQL == "" {
			continue
		}

		_, err := db.ExecContext(ctx, actualSQL)
		if err != nil {
			// Log but continue - some statements might fail if objects exist
			errMsg := strings.ToLower(err.Error())
			if strings.Contains(errMsg, "already exists") ||
				strings.Contains(errMsg, "duplicate") {
				slog.Debug("object already exists, skipping", "error", err)
				continue
			}
			return fmt.Errorf("failed to execute SQL: %w", err)
		}
	}

	return nil
}

// splitSQLStatements splits SQL content by semicolons.
// Handles basic cases but may not work for complex stored procedures.
func splitSQLStatements(sql string) []string {
	// Simple split by semicolon
	// Note: This doesn't handle semicolons inside strings or complex cases
	parts := strings.Split(sql, ";")
	var result []string
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

// ApplyMigrations applies dbmate migration files automatically.
//
// This function:
//  1. Finds migration files for the given database type
//  2. Checks which migrations have already been applied
//  3. Applies pending migrations in order
//  4. Records applied migrations in schema_migrations table
//
// Parameters:
//   - ctx: Context for cancellation
//   - db: Database connection
//   - dbType: One of "sqlite", "postgresql", "mysql"
//   - migrationsFS: Filesystem containing migrations (expects dbType subdirectory)
//
// Returns list of applied migration versions.
func ApplyMigrations(ctx context.Context, db *sql.DB, dbType string, migrationsFS fs.FS) ([]string, error) {
	if migrationsFS == nil {
		slog.Warn("no migrations filesystem provided, skipping automatic migration")
		return nil, nil
	}

	// Find migrations directory for this database type
	migrationsDir := dbType

	// Check if directory exists
	entries, err := fs.ReadDir(migrationsFS, migrationsDir)
	if err != nil {
		slog.Warn("migrations directory not found, skipping automatic migration",
			"db_type", dbType, "error", err)
		return nil, nil
	}

	// Ensure schema_migrations table exists
	if err := EnsureSchemaMigrationsTable(ctx, db, dbType); err != nil {
		return nil, fmt.Errorf("failed to create schema_migrations table: %w", err)
	}

	// Get already applied migrations
	applied, err := GetAppliedMigrations(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("failed to get applied migrations: %w", err)
	}

	// Get all migration files sorted by name (timestamp order)
	var migrationFiles []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			migrationFiles = append(migrationFiles, entry.Name())
		}
	}
	sort.Strings(migrationFiles)

	var appliedVersions []string

	for _, filename := range migrationFiles {
		version := ExtractVersionFromFilename(filename)

		// Skip if already applied
		if applied[version] {
			slog.Debug("migration already applied, skipping", "version", version)
			continue
		}

		slog.Info("applying migration", "filename", filename)

		// Read and parse migration file
		filePath := filepath.Join(migrationsDir, filename)
		content, err := fs.ReadFile(migrationsFS, filePath)
		if err != nil {
			return appliedVersions, fmt.Errorf("failed to read migration file %s: %w", filename, err)
		}

		upSQL, _ := ParseMigrationFile(string(content))
		if upSQL == "" {
			slog.Warn("no '-- migrate:up' section found", "filename", filename)
			continue
		}

		// Execute migration
		if err := ExecuteSQLStatements(ctx, db, upSQL, dbType); err != nil {
			return appliedVersions, fmt.Errorf("failed to apply migration %s: %w", version, err)
		}

		// Record as applied (handles race condition with other workers)
		recorded, err := RecordMigration(ctx, db, dbType, version)
		if err != nil {
			return appliedVersions, fmt.Errorf("failed to record migration %s: %w", version, err)
		}

		if recorded {
			appliedVersions = append(appliedVersions, version)
			slog.Info("successfully applied migration", "version", version)
		} else {
			// Another worker already applied this migration
			slog.Debug("migration was applied by another worker", "version", version)
		}
	}

	if len(appliedVersions) > 0 {
		slog.Info("applied migrations", "count", len(appliedVersions))
	} else {
		slog.Debug("no pending migrations to apply")
	}

	return appliedVersions, nil
}
