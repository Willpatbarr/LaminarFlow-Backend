package migrate

import (
	"context"
	"testing"
)

// The constraints 0008_api_token.sql claims, each asserted by trying to
// violate it.
func TestAPITokenConstraints(t *testing.T) {
	pool := migratedPool(t)
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
		acct := newAccount(t, pool, "nullprefix@example.com")

		_, err := pool.Exec(ctx,
			`INSERT INTO api_token (account_id, label, token_prefix, token_hash)
             VALUES ($1::uuid, 'CI', NULL, 'bcrypt-hash')`, acct)
		wantPgError(t, err, notNullViolation, "null token_prefix")
	})

	t.Run("rejects a null token_hash", func(t *testing.T) {
		acct := newAccount(t, pool, "nullhash@example.com")

		_, err := pool.Exec(ctx,
			`INSERT INTO api_token (account_id, label, token_prefix, token_hash)
             VALUES ($1::uuid, 'CI', 'lam_nullhash', NULL)`, acct)
		wantPgError(t, err, notNullViolation, "null token_hash")
	})

	t.Run("rejects a null label", func(t *testing.T) {
		acct := newAccount(t, pool, "nulllabel@example.com")

		_, err := pool.Exec(ctx,
			`INSERT INTO api_token (account_id, label, token_prefix, token_hash)
             VALUES ($1::uuid, NULL, 'lam_nulllabel', 'bcrypt-hash')`, acct)
		wantPgError(t, err, notNullViolation, "null label")
	})

	// The lookup key has to be unique or a prefix collision makes the lookup
	// ambiguous, and the server would have to bcrypt-compare more than one
	// row - which is the scan the split-token design exists to avoid.
	t.Run("rejects a duplicate token_prefix, even across accounts", func(t *testing.T) {
		first := newAccount(t, pool, "dupprefix-one@example.com")
		second := newAccount(t, pool, "dupprefix-two@example.com")

		if err := insert(first, "CI", "lam_collision"); err != nil {
			t.Fatalf("first insert: %v", err)
		}

		wantPgError(t, insert(second, "CI", "lam_collision"),
			uniqueViolation, "duplicate token_prefix")
	})

	// Deny by default. A token created without scopes must grant nothing, so
	// the default is the empty array rather than null or a wildcard.
	t.Run("scopes default to empty, not null", func(t *testing.T) {
		acct := newAccount(t, pool, "defaultscopes@example.com")

		if err := insert(acct, "CI", "lam_defaultscopes"); err != nil {
			t.Fatalf("insert: %v", err)
		}

		var scopes []string
		var expires *string
		if err := pool.QueryRow(ctx,
			`SELECT scopes, expires_at::text FROM api_token WHERE token_prefix = $1`,
			"lam_defaultscopes",
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
		acct := newAccount(t, pool, "nullscopes@example.com")

		_, err := pool.Exec(ctx,
			`INSERT INTO api_token (account_id, label, token_prefix, token_hash, scopes)
             VALUES ($1::uuid, 'CI', 'lam_nullscopes', 'bcrypt-hash', NULL)`, acct)
		wantPgError(t, err, notNullViolation, "null scopes")
	})

	// A token outliving the account it authenticates is an open door, so this
	// cascade is a security property, not a convenience.
	t.Run("deleting an account deletes its tokens", func(t *testing.T) {
		acct := newAccount(t, pool, "cascade@example.com")

		if err := insert(acct, "Doomed", "lam_cascade"); err != nil {
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
