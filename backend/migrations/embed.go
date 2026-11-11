// Package migrations embeds the SQL migration files.
//
// Embedding rather than reading from disk means the migrate binary is self-contained:
// the Kubernetes migration Job runs the same image as the API with no volume mount, so
// the migrations that run are provably the ones built from that commit.
package migrations

import "embed"

// FS holds every migration, applied in filename order by goose.
//
//go:embed *.sql
var FS embed.FS
