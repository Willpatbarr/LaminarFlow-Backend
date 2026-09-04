// Package migrations embeds the SQL migration files so the binary carries its
// own schema history.
//
// This matters for the container target: the image ships one binary, not a
// binary plus a directory that has to stay beside it. It also means a running
// server cannot disagree with the files on disk about what its own schema is.
package migrations

import "embed"

// FS holds every migration in this directory, in filename order.
//
//go:embed *.sql
var FS embed.FS
