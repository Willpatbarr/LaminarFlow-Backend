package migrate

import (
	"io/fs"

	"github.com/Willpatbarr/LaminarFlow-Backend/migrations"
)

// realMigrations returns the migrations the application actually ships.
//
// It lives in its own file so the import stays visible: internal/migrate has no
// dependency on the migrations package outside of tests, which is what lets the
// runner be reused for any migration set.
func realMigrations() fs.FS {
	return migrations.FS
}
