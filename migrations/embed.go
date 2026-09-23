// Package migrations embeds the raw *.sql migration files into the compiled
// binary so `racetify-api` and its `migrate` CLI ship as a single artifact
// with no separate migrations folder to deploy alongside them.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
