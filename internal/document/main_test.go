package document

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// testDatabaseURL names the throwaway database TestMain built for this run.
// It is empty when TEST_DATABASE_URL is unset, in which case the tests skip.
var testDatabaseURL string

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

// run owns the throwaway database's lifecycle. It exists as its own function
// so teardown can be deferred: TestMain has to call os.Exit, and os.Exit skips
// defers. For the same reason nothing below uses log.Fatalf once the database
// exists - that would exit and leak it.
//
// TEST_DATABASE_URL is only a bootstrap connection, used to CREATE and DROP
// the per-run database. The tests themselves never touch it.
func run(m *testing.M) int {
	bootstrap := os.Getenv("TEST_DATABASE_URL")
	if bootstrap == "" {
		// Skipping is right on a dev machine with no database. In CI it is not:
		// a run that silently tests nothing and reports success is the exact
		// failure this suite exists to prevent.
		if os.Getenv("CI") != "" {
			log.Print("TEST_DATABASE_URL must be set in CI - the database tests would skip")
			return 1
		}
		return m.Run()
	}

	// Parse before creating anything, so a bad URL can't leak a database.
	u, err := url.Parse(bootstrap)
	if err != nil {
		log.Printf("parse TEST_DATABASE_URL: %v", err)
		return 1
	}

	ctx := context.Background()
	admin, err := pgx.Connect(ctx, bootstrap)
	if err != nil {
		log.Printf("connect to bootstrap database: %v", err)
		return 1
	}
	defer admin.Close(ctx)

	name := fmt.Sprintf("laminarflow_test_%d_%d", os.Getpid(), time.Now().UnixNano())
	quoted := pgx.Identifier{name}.Sanitize()

	// PgConn().Exec is the simple query protocol, the same one psql -f uses.
	// Nothing actually executes until ReadAll drains the results.
	if _, err := admin.PgConn().Exec(ctx, "CREATE DATABASE "+quoted).ReadAll(); err != nil {
		log.Printf("create %s: %v", name, err)
		return 1
	}
	defer func() {
		// WITH (FORCE) terminates any connection a test left open, which would
		// otherwise fail the drop and leak the database.
		if _, err := admin.PgConn().Exec(ctx, "DROP DATABASE "+quoted+" WITH (FORCE)").ReadAll(); err != nil {
			log.Printf("warning: could not drop %s: %v", name, err)
		}
	}()

	u.Path = "/" + name
	testDatabaseURL = u.String()

	if err := applyMigrations(ctx, testDatabaseURL); err != nil {
		log.Printf("%v", err)
		return 1
	}

	return m.Run()
}

// applyMigrations runs every migrations/*.sql against dsn in filename order.
// Because this happens on an empty database every run, the fresh-install path
// a self-hosted user takes is exercised continuously rather than only when
// someone remembers to check it by hand.
func applyMigrations(ctx context.Context, dsn string) error {
	files, err := filepath.Glob(filepath.Join("..", "..", "migrations", "*.sql"))
	if err != nil {
		return fmt.Errorf("find migrations: %w", err)
	}
	if len(files) == 0 {
		return fmt.Errorf("no migrations found - wrong working directory?")
	}
	// Ordering is load-bearing here, so sort explicitly rather than relying on
	// whatever order Glob happens to return.
	sort.Strings(files)

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect to test database: %w", err)
	}
	defer conn.Close(ctx)

	for _, f := range files {
		sql, err := os.ReadFile(f)
		if err != nil {
			return fmt.Errorf("read %s: %w", f, err)
		}
		// A multi-statement simple query runs as one transaction, so each file
		// applies atomically - the same semantics as psql -1 -f.
		if _, err := conn.PgConn().Exec(ctx, string(sql)).ReadAll(); err != nil {
			return fmt.Errorf("apply %s: %w", filepath.Base(f), err)
		}
	}

	return nil
}
