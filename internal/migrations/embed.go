package migrations

import (
	"embed"
	"io/fs"
)

// embedded bundles the release-time copy of the durax-io/schema migrations
// (see romancy.EmbeddedMigrationsFS for why the copy exists). It lives in
// this package so internal callers — notably the storage test helpers —
// can apply the real migrations instead of hand-maintained schema copies.
//
//go:embed sql
var embedded embed.FS

// EmbeddedFS returns a filesystem rooted at the per-dialect migration
// directories (sqlite/, postgresql/, mysql/), ready for ApplyMigrations.
func EmbeddedFS() fs.FS {
	sub, err := fs.Sub(embedded, "sql")
	if err != nil {
		// Unreachable for an embedded directory that exists at build time.
		panic("failed to create sub filesystem for migrations: " + err.Error())
	}
	return sub
}
