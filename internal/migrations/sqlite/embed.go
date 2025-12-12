// Package sqlite provides embedded SQLite migrations.
package sqlite

import "embed"

//go:embed *.sql
var MigrationsFS embed.FS
