// Package migrations embeds the SQL migrations, applied with goose under an
// advisory lock by `monitor migrate` and at startup (ADR-0011).
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
