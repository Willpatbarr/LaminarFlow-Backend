package migrate

import (
	"context"
	"errors"
	"os"
	"testing"
	"testing/fstest"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/dbtest"

	"github.com/jackc/pgx/v5/pgxpool"
)

// steps is a two-migration set with a real dependency: the second table
// references the first, so rolling back in the wrong order would fail.
var steps = fstest.MapFS{
	"0001_parent.sql": {Data: []byte(
		"-- +migrate Up\nCREATE TABLE parent (id int PRIMARY KEY);\n" +
			"-- +migrate Down\nDROP TABLE parent;\n")},
	"0002_child.sql": {Data: []byte(
		"-- +migrate Up\nCREATE TABLE child (id int PRIMARY KEY, parent_id int REFERENCES parent(id));\n" +
			"-- +migrate Down\nDROP TABLE child;\n")},
}

// newPool builds a throwaway database and returns a pool onto it. The test is
// skipped rather than failed when there is no database, matching the rest of
// the suite.
func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	ctx := context.Background()

	dsn, cleanup, err := dbtest.Create(ctx)
	if errors.Is(err, dbtest.ErrNoDatabase) {
		t.Skip("TEST_DATABASE_URL not set")
	}
	if err != nil {
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(cleanup)

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	return pool
}

// tableExists asks the catalog rather than querying the table, so this helper
// works for a table that does not exist yet.
func tableExists(t *testing.T, pool *pgxpool.Pool, name string) bool {
	t.Helper()

	var exists bool
	if err := pool.QueryRow(context.Background(),
		`SELECT to_regclass($1) IS NOT NULL`, name,
	).Scan(&exists); err != nil {
		t.Fatalf("check %s: %v", name, err)
	}

	return exists
}

func TestUpAppliesEverythingThenIsIdempotent(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)

	loaded, err := Load(steps)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	applied, err := Up(ctx, pool, loaded)
	if err != nil {
		t.Fatalf("up: %v", err)
	}
	if len(applied) != 2 {
		t.Fatalf("applied %d migrations, want 2", len(applied))
	}
	if !tableExists(t, pool, "parent") || !tableExists(t, pool, "child") {
		t.Error("up did not create both tables")
	}

	// A second run must be a no-op. If this ever applies anything, the version
	// bookkeeping is not being read.
	again, err := Up(ctx, pool, loaded)
	if err != nil {
		t.Fatalf("second up: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("second up applied %d migrations, want 0", len(again))
	}
}

// The round trip the ticket asks for: up, then down, then up again. A down that
// leaves anything behind fails the second up with "already exists".
func TestDownReversesUpAndUpRunsAgain(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)

	loaded, err := Load(steps)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if _, err := Up(ctx, pool, loaded); err != nil {
		t.Fatalf("up: %v", err)
	}

	// Roll back everything, newest first. Reverse order is load-bearing here -
	// child references parent, so dropping parent first would fail.
	reverted, err := Down(ctx, pool, loaded, len(loaded))
	if err != nil {
		t.Fatalf("down: %v", err)
	}
	if len(reverted) != 2 {
		t.Fatalf("reverted %d migrations, want 2", len(reverted))
	}
	if reverted[0].Version != 2 {
		t.Errorf("rolled back %d first, want the newest (2)", reverted[0].Version)
	}
	if tableExists(t, pool, "parent") || tableExists(t, pool, "child") {
		t.Error("down left a table behind")
	}

	if _, err := Up(ctx, pool, loaded); err != nil {
		t.Fatalf("up after down: %v", err)
	}
	if !tableExists(t, pool, "parent") || !tableExists(t, pool, "child") {
		t.Error("up after down did not restore both tables")
	}
}

// Down with no count rolls back exactly one, not everything.
func TestDownDefaultsToOne(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)

	loaded, err := Load(steps)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, err := Up(ctx, pool, loaded); err != nil {
		t.Fatalf("up: %v", err)
	}

	reverted, err := Down(ctx, pool, loaded, 0)
	if err != nil {
		t.Fatalf("down: %v", err)
	}
	if len(reverted) != 1 {
		t.Fatalf("reverted %d migrations, want 1", len(reverted))
	}
	if tableExists(t, pool, "child") {
		t.Error("child survived its own rollback")
	}
	if !tableExists(t, pool, "parent") {
		t.Error("down rolled back more than it was asked to")
	}
}

// A migration with an empty Down must fail loudly rather than report success
// having changed nothing.
func TestDownRefusesIrreversible(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)

	loaded, err := Load(fstest.MapFS{
		"0001_oneway.sql": {Data: []byte("-- +migrate Up\nCREATE TABLE oneway (id int);\n-- +migrate Down\n")},
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, err := Up(ctx, pool, loaded); err != nil {
		t.Fatalf("up: %v", err)
	}

	if _, err := Down(ctx, pool, loaded, 1); !errors.Is(err, ErrIrreversible) {
		t.Fatalf("down returned %v, want ErrIrreversible", err)
	}
}

// A migration that fails part-way must leave neither the schema change nor the
// version row. Postgres has transactional DDL, so this is a real guarantee
// rather than a hope - but only if the runner puts both in one transaction.
func TestFailedMigrationRecordsNothing(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)

	loaded, err := Load(fstest.MapFS{
		"0001_broken.sql": {Data: []byte(
			"-- +migrate Up\nCREATE TABLE fine (id int);\nCREATE TABLE fine (id int);\n" +
				"-- +migrate Down\nDROP TABLE fine;\n")},
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if _, err := Up(ctx, pool, loaded); err == nil {
		t.Fatal("up succeeded on a broken migration")
	}

	if tableExists(t, pool, "fine") {
		t.Error("the failed migration left its table behind")
	}

	var recorded int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&recorded); err != nil {
		t.Fatalf("count: %v", err)
	}
	if recorded != 0 {
		t.Errorf("schema_migrations has %d rows after a failed migration, want 0", recorded)
	}
}

func TestStatusReportsBothStates(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)

	loaded, err := Load(steps)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, err := Down(ctx, pool, loaded, 1); err != nil {
		t.Fatalf("down on an empty database should be a no-op: %v", err)
	}

	before, err := Status(ctx, pool, loaded)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	for _, r := range before {
		if r.Applied {
			t.Errorf("%04d_%s reported applied before any up", r.Version, r.Name)
		}
	}

	if _, err := Up(ctx, pool, loaded); err != nil {
		t.Fatalf("up: %v", err)
	}

	after, err := Status(ctx, pool, loaded)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	for _, r := range after {
		if !r.Applied {
			t.Errorf("%04d_%s reported pending after up", r.Version, r.Name)
		}
	}
}

// Baseline is for adopting a hand-built schema. It must record without running,
// and must refuse once the database has any history of its own.
func TestBaselineRecordsWithoutRunning(t *testing.T) {
	ctx := context.Background()
	pool := newPool(t)

	loaded, err := Load(steps)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	recorded, err := Baseline(ctx, pool, loaded)
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	if len(recorded) != 2 {
		t.Fatalf("recorded %d migrations, want 2", len(recorded))
	}
	if tableExists(t, pool, "parent") {
		t.Error("baseline actually ran the migration")
	}

	pending, err := Pending(ctx, pool, loaded)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("%d migrations still pending after baseline, want 0", len(pending))
	}

	if _, err := Baseline(ctx, pool, loaded); err == nil {
		t.Error("baseline succeeded on a database that already had history")
	}
}

// The real migrations must load and apply, not just the fixtures above. This is
// what would have caught 0003_workspace.sql's original NOT NULL break.
func TestRealMigrationsApplyToAnEmptyDatabase(t *testing.T) {
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	ctx := context.Background()
	pool := newPool(t)

	loaded, err := Load(realMigrations())
	if err != nil {
		t.Fatalf("load real migrations: %v", err)
	}

	applied, err := Up(ctx, pool, loaded)
	if err != nil {
		t.Fatalf("up: %v", err)
	}
	if len(applied) != len(loaded) {
		t.Fatalf("applied %d of %d migrations", len(applied), len(loaded))
	}

	// Every real migration must also reverse cleanly, or "down" is a promise
	// the pipeline cannot keep.
	reverted, err := Down(ctx, pool, loaded, len(loaded))
	if err != nil {
		t.Fatalf("down: %v", err)
	}
	if len(reverted) != len(loaded) {
		t.Errorf("reverted %d of %d migrations", len(reverted), len(loaded))
	}
}
