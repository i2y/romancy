package romancy

import (
	"embed"
	"io/fs"
)

// embeddedMigrations contains the bundled database migration files.
// The migrations are organized by database type:
//   - schema/db/migrations/sqlite/*.sql
//   - schema/db/migrations/postgresql/*.sql
//   - schema/db/migrations/mysql/*.sql
//
//go:embed schema/db/migrations
var embeddedMigrations embed.FS

// EmbeddedMigrationsFS returns a filesystem rooted at schema/db/migrations
// for use with the migrations package.
//
// The returned FS contains subdirectories for each database type:
//   - sqlite/
//   - postgresql/
//   - mysql/
func EmbeddedMigrationsFS() fs.FS {
	subFS, err := fs.Sub(embeddedMigrations, "schema/db/migrations")
	if err != nil {
		// This should never happen with embedded files
		panic("failed to create sub filesystem for migrations: " + err.Error())
	}
	return subFS
}
