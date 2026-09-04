// Package dbtest builds throwaway Postgres databases for tests.
//
// It is a package rather than a copied TestMain because more than one package
// needs a real database now: internal/document tests the storage layer, and
// internal/migrate tests the runner that builds the schema in the first place.
// Two copies of this logic would be two things to keep in step.
//
// It deliberately does not apply migrations - the caller does, using the real
// runner. The migrate tests need an empty database to migrate.
package dbtest

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrNoDatabase means TEST_DATABASE_URL is unset. Tests should skip.
var ErrNoDatabase = errors.New("TEST_DATABASE_URL not set")

// Create builds a throwaway database and returns its URL and a cleanup
// function. TEST_DATABASE_URL is only a bootstrap connection, used to CREATE
// and DROP - the returned URL names a different database, so tests never write
// to whatever that variable points at.
//
// In CI a missing TEST_DATABASE_URL is an error rather than a skip: a run that
// silently tests nothing and reports success is the failure this suite exists
// to prevent.
func Create(ctx context.Context) (dsn string, cleanup func(), err error) {
	bootstrap := os.Getenv("TEST_DATABASE_URL")
	if bootstrap == "" {
		if os.Getenv("CI") != "" {
			return "", nil, errors.New("TEST_DATABASE_URL must be set in CI - the database tests would skip")
		}
		return "", nil, ErrNoDatabase
	}

	// Parse before creating anything, so a bad URL cannot leak a database.
	u, err := url.Parse(bootstrap)
	if err != nil {
		return "", nil, fmt.Errorf("parse TEST_DATABASE_URL: %w", err)
	}

	admin, err := pgx.Connect(ctx, bootstrap)
	if err != nil {
		return "", nil, fmt.Errorf("connect to bootstrap database: %w", err)
	}

	name := fmt.Sprintf("laminarflow_test_%d_%d", os.Getpid(), time.Now().UnixNano())
	quoted := pgx.Identifier{name}.Sanitize()

	// PgConn().Exec is the simple query protocol, the same one psql -f uses.
	// Nothing executes until ReadAll drains the results.
	if _, err := admin.PgConn().Exec(ctx, "CREATE DATABASE "+quoted).ReadAll(); err != nil {
		admin.Close(ctx)
		return "", nil, fmt.Errorf("create %s: %w", name, err)
	}

	cleanup = func() {
		// WITH (FORCE) terminates any connection a test left open, which would
		// otherwise fail the drop and leak the database.
		if _, err := admin.PgConn().Exec(ctx, "DROP DATABASE "+quoted+" WITH (FORCE)").ReadAll(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not drop %s: %v\n", name, err)
		}
		admin.Close(ctx)
	}

	u.Path = "/" + name
	return u.String(), cleanup, nil
}

// Name extracts the database name from a DSN, for logging.
func Name(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil || len(u.Path) < 2 {
		return dsn
	}
	return u.Path[1:]
}
