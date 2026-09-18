package auth

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
// Empty when TEST_DATABASE_URL is unset, in which case the tests skip.
//
// Same shape as internal/document/main_test.go. The duplication is the two
// TestMain functions themselves, which Go requires one of per package; the
// parts worth sharing - building the database, applying the real migrations -
// are already in dbtest and migrate.
var testDatabaseURL string

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

// run owns the throwaway database's lifecycle, as its own function so teardown
// can be deferred: TestMain has to call os.Exit, and os.Exit skips defers.
func run(m *testing.M) int {
	ctx := context.Background()

	dsn, cleanup, err := dbtest.Create(ctx)
	if errors.Is(err, dbtest.ErrNoDatabase) {
		return m.Run()
	}
	if err != nil {
		log.Printf("%v", err)
		return 1
	}
	defer cleanup()

	testDatabaseURL = dsn
	log.Printf("test database: %s", dbtest.Name(dsn))

	if err := applyMigrations(ctx, dsn); err != nil {
		log.Printf("%v", err)
		return 1
	}

	return m.Run()
}

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

// newTestService returns a Service against the throwaway database, plus an
// account id to hang tokens off.
func newTestService(t *testing.T) (*Service, string) {
	t.Helper()

	if testDatabaseURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	ctx := context.Background()

	pool, err := pgxpool.New(ctx, testDatabaseURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	// A distinct email per call. account has a unique index over lower(email),
	// and a test that calls this twice would otherwise fail on the second call
	// for a reason that has nothing to do with what it was testing. The uuid
	// comes from Postgres rather than the test so nothing here needs a seed.
	var accountID string
	err = pool.QueryRow(ctx,
		`INSERT INTO account (email, password_hash, display_name)
		 VALUES ('token-test-' || gen_random_uuid() || '@example.com',
		         'not-a-real-hash', 'Test Account')
		 RETURNING id`,
	).Scan(&accountID)
	if err != nil {
		t.Fatalf("create account: %v", err)
	}

	return NewService(pool), accountID
}
