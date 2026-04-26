package romancy

import (
	"embed"
	"io/fs"
)

// embeddedMigrations bundles the database migration .sql files into the
// compiled binary. The on-disk source of truth is the durax-io/schema
// submodule mounted at schema/, shared with edda (Python) and shikibu;
// internal/migrations/sql/ holds a release-time copy because Go's module
// proxy strips submodule gitlinks from the published zip and //go:embed
// would otherwise resolve to an empty fs.FS for downstream consumers
// (`go install …/cmd/romancy@latest`, chinotto, …).
//
// Refresh the copy with `make sync-schema`; CI runs `make
// check-schema-sync` to fail builds where the two trees have drifted.
//
// Layout:
//   - internal/migrations/sql/sqlite/*.sql
//   - internal/migrations/sql/postgresql/*.sql
//   - internal/migrations/sql/mysql/*.sql
//
//go:embed internal/migrations/sql
var embeddedMigrations embed.FS

// EmbeddedMigrationsFS returns a filesystem rooted at the per-dialect
// migration directories (sqlite/, postgresql/, mysql/) so callers can
// hand it straight to the migrations package without knowing where the
// files physically live in the source tree.
func EmbeddedMigrationsFS() fs.FS {
	subFS, err := fs.Sub(embeddedMigrations, "internal/migrations/sql")
	if err != nil {
		// This should never happen with embedded files
		panic("failed to create sub filesystem for migrations: " + err.Error())
	}
	return subFS
}
