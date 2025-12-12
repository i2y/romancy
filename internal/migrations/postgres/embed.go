// Package postgres provides embedded PostgreSQL migrations.
package postgres

import "embed"

//go:embed *.sql
var MigrationsFS embed.FS
