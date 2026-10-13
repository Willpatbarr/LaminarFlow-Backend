package auth

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The SQLSTATE codes these tests assert on. Asserting the code rather than "an error
// came back" is what makes the assertions mean anything: a typo in the INSERT also
// returns an error, and would otherwise read as a constraint doing its job.
const (
	notNullViolation    = "23502"
	foreignKeyViolation = "23503"
	uniqueViolation     = "23505"
)

// A pool on the database TestMain migrated. Unlike internal/migrate's tests, which get
// a fresh database each, this one is shared by the whole package - so the helpers below
// make every account email and token prefix unique, or a second run in the same process
// (-count=2) would collide with the first on the very constraints being tested.
func schemaPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	if testDatabaseURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	pool, err := pgxpool.New(context.Background(), testDatabaseURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	return pool
}

func newAccount(t *testing.T, pool *pgxpool.Pool, name string) string {
	t.Helper()

	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO account (email, password_hash, display_name)
		 VALUES ($1 || '-' || gen_random_uuid() || '@example.com', 'hash', $1)
		 RETURNING id::text`, name,
	).Scan(&id); err != nil {
		t.Fatalf("create account %s: %v", name, err)
	}

	return id
}

func uniquePrefix(prefix string) string {
	return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
}

// wantPgError fails unless err is a Postgres error carrying code. what names the
// attempted violation, so a failure says which constraint did not hold.
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

// The constraints 0008_api_token.sql claims, each asserted by trying to
// violate it.
func TestAPITokenConstraints(t *testing.T) {
	pool := schemaPool(t)
	ctx := context.Background()

	insert := func(accountID, label, prefix string) error {
		_, err := pool.Exec(ctx,
			`INSERT INTO api_token (account_id, label, token_prefix, token_hash)
             VALUES ($1::uuid, $2, $3, 'bcrypt-hash')`,
			accountID, label, prefix)
		return err
	}

	t.Run("rejects a token for an account that does not exist", func(t *testing.T) {
		err := insert("00000000-0000-0000-0000-000000000000", "CI", "lam_orphan")
		wantPgError(t, err, foreignKeyViolation, "orphan token")
	})

	t.Run("rejects a null account", func(t *testing.T) {
		_, err := pool.Exec(ctx,
			`INSERT INTO api_token (account_id, label, token_prefix, token_hash)
             VALUES (NULL, 'CI', 'lam_nullacct', 'bcrypt-hash')`)
		wantPgError(t, err, notNullViolation, "null account_id")
	})

	t.Run("rejects a null token_prefix", func(t *testing.T) {
		acct := newAccount(t, pool, "nullprefix")

		_, err := pool.Exec(ctx,
			`INSERT INTO api_token (account_id, label, token_prefix, token_hash)
             VALUES ($1::uuid, 'CI', NULL, 'bcrypt-hash')`, acct)
		wantPgError(t, err, notNullViolation, "null token_prefix")
	})

	t.Run("rejects a null token_hash", func(t *testing.T) {
		acct := newAccount(t, pool, "nullhash")

		_, err := pool.Exec(ctx,
			`INSERT INTO api_token (account_id, label, token_prefix, token_hash)
             VALUES ($1::uuid, 'CI', 'lam_nullhash', NULL)`, acct)
		wantPgError(t, err, notNullViolation, "null token_hash")
	})

	t.Run("rejects a null label", func(t *testing.T) {
		acct := newAccount(t, pool, "nulllabel")

		_, err := pool.Exec(ctx,
			`INSERT INTO api_token (account_id, label, token_prefix, token_hash)
             VALUES ($1::uuid, NULL, 'lam_nulllabel', 'bcrypt-hash')`, acct)
		wantPgError(t, err, notNullViolation, "null label")
	})

	// The lookup key has to be unique or a prefix collision makes the lookup
	// ambiguous, and the server would have to bcrypt-compare more than one
	// row - which is the scan the split-token design exists to avoid.
	t.Run("rejects a duplicate token_prefix, even across accounts", func(t *testing.T) {
		first := newAccount(t, pool, "dupprefix-one")
		second := newAccount(t, pool, "dupprefix-two")
		prefix := uniquePrefix("lam_collision")

		if err := insert(first, "CI", prefix); err != nil {
			t.Fatalf("first insert: %v", err)
		}

		wantPgError(t, insert(second, "CI", prefix),
			uniqueViolation, "duplicate token_prefix")
	})

	// Deny by default. A token created without scopes must grant nothing, so
	// the default is the empty array rather than null or a wildcard.
	t.Run("scopes default to empty, not null", func(t *testing.T) {
		acct := newAccount(t, pool, "defaultscopes")

		prefix := uniquePrefix("lam_defaultscopes")

		if err := insert(acct, "CI", prefix); err != nil {
			t.Fatalf("insert: %v", err)
		}

		var scopes []string
		var expires *string
		if err := pool.QueryRow(ctx,
			`SELECT scopes, expires_at::text FROM api_token WHERE token_prefix = $1`,
			prefix,
		).Scan(&scopes, &expires); err != nil {
			t.Fatalf("read back: %v", err)
		}
		if len(scopes) != 0 {
			t.Errorf("scopes defaulted to %v, want empty", scopes)
		}
		// Null expires_at is the "never expires" case, which is every token
		// until something sets an expiry.
		if expires != nil {
			t.Errorf("expires_at defaulted to %q, want null", *expires)
		}
	})

	t.Run("rejects null scopes", func(t *testing.T) {
		acct := newAccount(t, pool, "nullscopes")

		_, err := pool.Exec(ctx,
			`INSERT INTO api_token (account_id, label, token_prefix, token_hash, scopes)
             VALUES ($1::uuid, 'CI', 'lam_nullscopes', 'bcrypt-hash', NULL)`, acct)
		wantPgError(t, err, notNullViolation, "null scopes")
	})

	// A token outliving the account it authenticates is an open door, so this
	// cascade is a security property, not a convenience.
	t.Run("deleting an account deletes its tokens", func(t *testing.T) {
		acct := newAccount(t, pool, "cascade")

		if err := insert(acct, "Doomed", uniquePrefix("lam_cascade")); err != nil {
			t.Fatalf("insert: %v", err)
		}

		if _, err := pool.Exec(ctx, `DELETE FROM account WHERE id = $1::uuid`, acct); err != nil {
			t.Fatalf("delete account: %v", err)
		}

		var tokens int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM api_token WHERE account_id = $1::uuid`, acct,
		).Scan(&tokens); err != nil {
			t.Fatalf("count tokens: %v", err)
		}
		if tokens != 0 {
			t.Errorf("%d tokens survived their account, want 0", tokens)
		}
	})

	// Both index claims. token_prefix is the hot path, indexed by the UNIQUE
	// constraint; account_id is LAM-15 step 4, indexed explicitly because
	// nothing else covers it.
	for _, col := range []string{"token_prefix", "account_id"} {
		t.Run(col+" leads an index", func(t *testing.T) {
			var leads bool
			if err := pool.QueryRow(ctx,
				`SELECT EXISTS (
                     SELECT 1
                       FROM pg_index i
                       JOIN pg_attribute a
                         ON a.attrelid = i.indrelid
                        AND a.attnum = i.indkey[0]
                      WHERE i.indrelid = 'api_token'::regclass
                        AND a.attname = $1
                 )`, col,
			).Scan(&leads); err != nil {
				t.Fatalf("read indexes: %v", err)
			}
			if !leads {
				t.Errorf("no index on api_token leads with %s", col)
			}
		})
	}
}
