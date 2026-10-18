// Package migrations embeds the goose SQL migrations.
package migrations

import "embed"

// FS holds every *.sql migration.
//
//go:embed *.sql
var FS embed.FS
