// Package mysql provides embedded MySQL migrations.
package mysql

import "embed"

//go:embed *.sql
var MigrationsFS embed.FS
