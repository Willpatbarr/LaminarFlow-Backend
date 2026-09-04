package document

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"testing"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/dbtest"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/migrate"
	"github.com/Willpatbarr/LaminarFlow-Backend/migrations"

	"github.com/jackc/pgx/v5/pgxpool"
)

// testDatabaseURL names the throwaway database TestMain built for this run.
// It is empty when TEST_DATABASE_URL is unset, in which case the tests skip.
var testDatabaseURL string

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

// run owns the throwaway database's lifecycle. It exists as its own function so
// teardown can be deferred: TestMain has to call os.Exit, and os.Exit skips
// defers. For the same reason nothing below uses log.Fatalf once the database
// exists - that would exit and leak it.
func run(m *testing.M) int {
	ctx := context.Background()

	dsn, cleanup, err := dbtest.Create(ctx)
	if errors.Is(err, dbtest.ErrNoDatabase) {
		// Skipping is right on a dev machine with no database. dbtest makes it
		// an error in CI, where a silently empty run would be worse.
		return m.Run()
	}
	if err != nil {
		log.Printf("%v", err)
		return 1
	}
	defer cleanup()

	testDatabaseURL = dsn

	// Logged so a passing CI run visibly shows the throwaway database was
	// really created, rather than leaving it to be inferred from the exit code.
	log.Printf("test database: %s", dbtest.Name(dsn))

	if err := applyMigrations(ctx, dsn); err != nil {
		log.Printf("%v", err)
		return 1
	}

	return m.Run()
}

// applyMigrations brings the throwaway database up to the current schema using
// the same runner the migrate command and the server use.
//
// It used to be a second implementation - its own glob over migrations/*.sql -
// which meant a migration the tests could apply but the real runner could not
// would have stayed invisible until deploy. Calling the real runner here is
// what makes a green suite evidence that the pipeline works.
//
// Because this happens on an empty database every run, the fresh-install path a
// self-hosted user takes is exercised continuously rather than only when
// someone remembers to check it by hand.
func applyMigrations(ctx context.Context, dsn string) error {
	loaded, err := migrate.Load(migrations.FS)
	if err != nil {
		return fmt.Errorf("load migrations: %w", err)
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect to test database: %w", err)
	}
	defer pool.Close()

	if _, err := migrate.Up(ctx, pool, loaded); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}

	return nil
}
