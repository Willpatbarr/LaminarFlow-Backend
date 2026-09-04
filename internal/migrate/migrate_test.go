package migrate

import (
	"strings"
	"testing"
	"testing/fstest"
)

// good is a two-migration set used by the parsing tests.
var good = fstest.MapFS{
	"0001_first.sql":  {Data: []byte("-- header\n-- +migrate Up\nCREATE TABLE a ();\n-- +migrate Down\nDROP TABLE a;\n")},
	"0002_second.sql": {Data: []byte("-- +migrate Up\nCREATE TABLE b ();\n-- +migrate Down\nDROP TABLE b;\n")},
}

func TestLoadParsesAndOrders(t *testing.T) {
	migrations, err := Load(good)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if len(migrations) != 2 {
		t.Fatalf("loaded %d migrations, want 2", len(migrations))
	}

	if migrations[0].Version != 1 || migrations[1].Version != 2 {
		t.Errorf("versions = %d, %d; want 1, 2", migrations[0].Version, migrations[1].Version)
	}
	if migrations[0].Name != "first" {
		t.Errorf("name = %q, want %q", migrations[0].Name, "first")
	}

	// The file-level comment above the Up marker must not leak into the SQL -
	// it would be harmless here but would hide a real statement written above
	// the marker by mistake.
	if strings.Contains(migrations[0].Up, "header") {
		t.Errorf("Up contains the file header: %q", migrations[0].Up)
	}
	if migrations[0].Up != "CREATE TABLE a ();" {
		t.Errorf("Up = %q, want %q", migrations[0].Up, "CREATE TABLE a ();")
	}
	if migrations[0].Down != "DROP TABLE a;" {
		t.Errorf("Down = %q, want %q", migrations[0].Down, "DROP TABLE a;")
	}
}

// Load must fail loudly on a malformed set rather than skipping the file. A
// skipped migration is one the runner reports as up to date while the schema is
// missing a table.
func TestLoadRejectsBadInput(t *testing.T) {
	cases := map[string]fstest.MapFS{
		"unnumbered filename": {
			"create_thing.sql": {Data: []byte("-- +migrate Up\nSELECT 1;\n-- +migrate Down\nSELECT 1;\n")},
		},
		"duplicate version": {
			"0001_a.sql": {Data: []byte("-- +migrate Up\nSELECT 1;\n-- +migrate Down\nSELECT 1;\n")},
			"0001_b.sql": {Data: []byte("-- +migrate Up\nSELECT 1;\n-- +migrate Down\nSELECT 1;\n")},
		},
		"missing Up marker": {
			"0001_a.sql": {Data: []byte("CREATE TABLE a ();\n-- +migrate Down\nDROP TABLE a;\n")},
		},
		"missing Down marker": {
			"0001_a.sql": {Data: []byte("-- +migrate Up\nCREATE TABLE a ();\n")},
		},
		"empty Up section": {
			"0001_a.sql": {Data: []byte("-- +migrate Up\n\n-- +migrate Down\nDROP TABLE a;\n")},
		},
		"no migrations at all": {
			"README.md": {Data: []byte("not a migration")},
		},
	}

	for name, fsys := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(fsys); err == nil {
				t.Error("Load succeeded, want an error")
			}
		})
	}
}

// An empty Down section is allowed at parse time but must be rejected at
// rollback, so an irreversible migration fails with a clear reason rather than
// silently succeeding and leaving the schema untouched.
func TestLoadAllowsEmptyDown(t *testing.T) {
	fsys := fstest.MapFS{
		"0001_a.sql": {Data: []byte("-- +migrate Up\nCREATE TABLE a ();\n-- +migrate Down\n")},
	}

	migrations, err := Load(fsys)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if migrations[0].Down != "" {
		t.Errorf("Down = %q, want empty", migrations[0].Down)
	}
}
