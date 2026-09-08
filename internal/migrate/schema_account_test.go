package migrate

import (
	"context"
	"testing"
)

// The constraints 0007_account.sql claims, each asserted by trying to violate
// it. account has no parent, so unlike team and project there is no cascade
// to test and no workspace to set up.
func TestAccountConstraints(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()

	insert := func(email, hash, name string) error {
		_, err := pool.Exec(ctx,
			`INSERT INTO account (email, password_hash, display_name)
             VALUES ($1, $2, $3)`,
			email, hash, name)
		return err
	}

	t.Run("rejects a duplicate email", func(t *testing.T) {
		if err := insert("dup@example.com", "hash", "First"); err != nil {
			t.Fatalf("first insert: %v", err)
		}

		wantPgError(t, insert("dup@example.com", "hash", "Second"),
			uniqueViolation, "duplicate email")
	})

	// The half that carries the claim. A plain UNIQUE (email) passes the
	// subtest above and fails this one, which is the whole reason
	// 0007_account.sql indexes lower(email) instead.
	t.Run("rejects an email differing only in case", func(t *testing.T) {
		if err := insert("Will@example.com", "hash", "Will"); err != nil {
			t.Fatalf("first insert: %v", err)
		}

		wantPgError(t, insert("will@example.com", "hash", "Will again"),
			uniqueViolation, "same email in different case")
	})

	t.Run("rejects a null email", func(t *testing.T) {
		_, err := pool.Exec(ctx,
			`INSERT INTO account (email, password_hash, display_name)
             VALUES (NULL, 'hash', 'Nameless')`)
		wantPgError(t, err, notNullViolation, "null email")
	})

	t.Run("rejects a null password_hash", func(t *testing.T) {
		_, err := pool.Exec(ctx,
			`INSERT INTO account (email, password_hash, display_name)
             VALUES ('nohash@example.com', NULL, 'No Hash')`)
		wantPgError(t, err, notNullViolation, "null password_hash")
	})

	t.Run("rejects a null display_name", func(t *testing.T) {
		_, err := pool.Exec(ctx,
			`INSERT INTO account (email, password_hash, display_name)
             VALUES ('noname@example.com', 'hash', NULL)`)
		wantPgError(t, err, notNullViolation, "null display_name")
	})

	// Two different addresses must still both be insertable - a unique index
	// on lower(email) that somehow folded more than case would pass every
	// rejection test above while making the table unusable.
	t.Run("allows two different emails", func(t *testing.T) {
		if err := insert("one@example.com", "hash", "One"); err != nil {
			t.Fatalf("first insert: %v", err)
		}
		if err := insert("two@example.com", "hash", "Two"); err != nil {
			t.Fatalf("second insert: %v", err)
		}
	})
}
