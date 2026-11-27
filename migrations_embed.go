package romancy

import (
	"io/fs"

	"github.com/i2y/romancy/internal/migrations"
)

// EmbeddedMigrationsFS returns a filesystem rooted at the per-dialect
// migration directories (sqlite/, postgresql/, mysql/) so callers can
// hand it straight to the migrations package without knowing where the
// files physically live in the source tree.
//
// The .sql files are bundled into the compiled binary by the
// internal/migrations package. The on-disk source of truth is the
// durax-io/schema submodule mounted at schema/, shared with edda (Python)
// and shikibu; internal/migrations/sql/ holds a release-time copy because
// Go's module proxy strips submodule gitlinks from the published zip and
// //go:embed would otherwise resolve to an empty fs.FS for downstream
// consumers (`go install …/cmd/romancy@latest`, chinotto, …).
//
// Refresh the copy with `make sync-schema`; CI runs `make
// check-schema-sync` to fail builds where the two trees have drifted.
//
// Layout:
//   - internal/migrations/sql/sqlite/*.sql
//   - internal/migrations/sql/postgresql/*.sql
//   - internal/migrations/sql/mysql/*.sql
func EmbeddedMigrationsFS() fs.FS {
	return migrations.EmbeddedFS()
}
