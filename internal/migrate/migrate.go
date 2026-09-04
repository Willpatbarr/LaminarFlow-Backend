// Package migrate applies versioned SQL migrations to Postgres.
//
// It is a package rather than a script because two callers need it: the migrate
// command an operator runs, and the test bootstrap that builds a throwaway
// database on every run. Before LAM-10 those were two implementations - the
// test suite had its own loop over migrations/*.sql - so a migration the tests
// could apply but the real runner could not would have stayed invisible until
// deploy. One implementation, two callers, no drift.
package migrate

import (
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// lockID is the advisory lock every runner takes before touching the schema.
// Two servers starting at once must not both conclude that migration 4 is
// pending and both try to apply it. The value is arbitrary but must never
// change - it is the identity of this lock, not a version number.
const lockID int64 = 0x4C414D10

// ErrIrreversible is returned when rolling back a migration that declared no
// Down statements.
var ErrIrreversible = errors.New("migration declares no Down section")

// filename matches NNNN_name.sql - the convention migrations/README.md sets.
var filename = regexp.MustCompile(`^(\d+)_(.+)\.sql$`)

// sectionUp and sectionDown delimit the two halves of a migration file. Both
// live in one file on purpose: an up and its down are one reversible change,
// and splitting them across two files is an invitation to update one and not
// the other.
const (
	sectionUp   = "-- +migrate Up"
	sectionDown = "-- +migrate Down"
)

// Migration is one versioned schema change.
type Migration struct {
	Version int64
	Name    string
	Up      string
	Down    string
}

// Record is one row of schema_migrations paired with whether the file that
// produced it is still present.
type Record struct {
	Version int64
	Name    string
	Applied bool
}

// Load reads every migration from fsys, which is normally the embedded
// migrations directory. It fails rather than guessing on a malformed filename,
// a duplicate version, or a missing Up section: a migration the runner cannot
// read is one it would otherwise silently skip.
func Load(fsys fs.FS) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}

	var migrations []Migration
	seen := make(map[int64]string)

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}

		match := filename.FindStringSubmatch(e.Name())
		if match == nil {
			return nil, fmt.Errorf("migration %q does not match NNNN_name.sql", e.Name())
		}

		version, err := strconv.ParseInt(match[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("migration %q has an unparseable version: %w", e.Name(), err)
		}
		if prev, dup := seen[version]; dup {
			return nil, fmt.Errorf("migrations %q and %q share version %d", prev, e.Name(), version)
		}
		seen[version] = e.Name()

		body, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", e.Name(), err)
		}

		up, down, err := split(string(body))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}

		migrations = append(migrations, Migration{
			Version: version,
			Name:    match[2],
			Up:      up,
			Down:    down,
		})
	}

	if len(migrations) == 0 {
		return nil, errors.New("no migrations found")
	}

	sort.Slice(migrations, func(i, j int) bool {
		return migrations[i].Version < migrations[j].Version
	})

	return migrations, nil
}

// split carves a migration file into its Up and Down halves. Anything before
// the Up marker is a file-level comment and is discarded.
func split(body string) (up, down string, err error) {
	upAt := strings.Index(body, sectionUp)
	if upAt < 0 {
		return "", "", fmt.Errorf("missing %q marker", sectionUp)
	}

	rest := body[upAt+len(sectionUp):]

	downAt := strings.Index(rest, sectionDown)
	if downAt < 0 {
		return "", "", fmt.Errorf("missing %q marker", sectionDown)
	}

	up = strings.TrimSpace(rest[:downAt])
	down = strings.TrimSpace(rest[downAt+len(sectionDown):])

	if up == "" {
		return "", "", errors.New("Up section is empty")
	}

	return up, down, nil
}
