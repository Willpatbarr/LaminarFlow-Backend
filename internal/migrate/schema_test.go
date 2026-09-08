package migrate

// Shared helpers for the schema contract tests. One file per table, named
// schema_<table>_test.go.
//
// These live in internal/migrate rather than beside the code that reads each
// table, and that is deliberate: a constraint is asserted before any Go code
// depending on it exists, so a wrong ON DELETE action or a missing UNIQUE
// cannot ship and then sit untested until a later ticket happens to need the
// table. It is the same boundary crossing real_test.go makes - the runner
// itself stays generic, and only the test files know about the real migrations.
//
// LAM-27 owns the broad hierarchy integration tests. These are narrower: one
// table's own constraints, asserted by trying to violate each one.

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The SQLSTATE codes these tests assert on. Asserting the code rather than
// "an error came back" is what makes the assertions mean anything: a typo in
// the INSERT also returns an error, and would otherwise read as a constraint
// doing its job.
const (
	notNullViolation    = "23502"
	foreignKeyViolation = "23503"
	uniqueViolation     = "23505"
)

// migratedPool returns a throwaway database with every real migration applied,
// using the same runner the server and the migrate command use.
func migratedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	pool := newPool(t)

	loaded, err := Load(realMigrations())
	if err != nil {
		t.Fatalf("load real migrations: %v", err)
	}
	if _, err := Up(context.Background(), pool, loaded); err != nil {
		t.Fatalf("apply real migrations: %v", err)
	}

	return pool
}

// newWorkspace inserts a workspace and returns its ID. Every subtest owns its
// own workspace, which is what lets subtests share one database without
// colliding on the very (workspace_id, name) constraints they are testing.
func newWorkspace(t *testing.T, pool *pgxpool.Pool, name string) string {
	t.Helper()

	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO workspace (name) VALUES ($1) RETURNING id::text`, name,
	).Scan(&id); err != nil {
		t.Fatalf("create workspace %s: %v", name, err)
	}

	return id
}

// newTeam inserts a team into workspaceID and returns its ID, for the child
// tables that hang off team rather than off workspace.
func newTeam(t *testing.T, pool *pgxpool.Pool, workspaceID, name string) string {
	t.Helper()

	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO team (workspace_id, name) VALUES ($1::uuid, $2) RETURNING id::text`,
		workspaceID, name,
	).Scan(&id); err != nil {
		t.Fatalf("create team %s: %v", name, err)
	}

	return id
}

// newAccount inserts an account and returns its ID. email is folded to lower
// case by account_email_lower_key, so callers need distinct addresses rather
// than distinct capitalisation.
func newAccount(t *testing.T, pool *pgxpool.Pool, email string) string {
	t.Helper()

	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO account (email, password_hash, display_name)
		 VALUES ($1, 'hash', $1) RETURNING id::text`, email,
	).Scan(&id); err != nil {
		t.Fatalf("create account %s: %v", email, err)
	}

	return id
}

// wantPgError fails unless err is a Postgres error carrying code. what names
// the attempted violation, so a failure says which constraint did not hold.
func wantPgError(t *testing.T, err error, code, what string) {
	t.Helper()

	if err == nil {
		t.Fatalf("%s: statement succeeded, want SQLSTATE %s", what, code)
	}

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("%s: got %v, want a Postgres error with SQLSTATE %s", what, err, code)
	}
	if pgErr.Code != code {
		t.Fatalf("%s: got SQLSTATE %s (%s), want %s", what, pgErr.Code, pgErr.Message, code)
	}
}
