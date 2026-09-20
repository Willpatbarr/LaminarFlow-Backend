/*
╔═ account.go ══════════════════════════════════════════════════════════════════════════
║  account · deletion, and the repair the schema cannot make
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      Service        struct
║      Removal        struct
║      ErrNotFound    error
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      internal/api  →  DELETE /api/v1/accounts/me
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

// Package account owns the account table, and exists for one reason: deleting an
// account is not a DELETE.
//
// 0027 records the gap and explains why the schema cannot close it.
// saved_view.owner_account_id is ON DELETE SET NULL, because a *shared* team view has
// to survive its author leaving - the same call comment.author_id made. But a view is
// either shared or private, and the two want opposite things. Nulling the owner of a
// private view leaves a row visible to nobody and owned by nobody: it cannot be listed,
// cannot be edited, and cannot be deleted through any interface, because every one of
// those is scoped by an owner it no longer has.
//
// Both schema-level fixes were considered in 0027 and rejected there:
//
//   - CHECK (is_shared OR owner_account_id IS NOT NULL) does not repair anything. It
//     converts the leak into an undeletable account - the foreign key's SET NULL would
//     violate the CHECK and the whole delete would be rejected.
//   - Flipping is_shared as part of the delete is not expressible. A foreign key action
//     can null a column; it cannot set a second one.
//
// So the repair lives here, in the one path that deletes an account.
package account

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound means there is no such account. Deleting one twice is the ordinary way to
// see it - a retried request after a successful delete.
var ErrNotFound = errors.New("account: not found")

/*
┏━ Removal ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  what a deletion did, besides deleting
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      PrivateViewsDeleted    int
┃      SharedViewsKept        int
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Delete                            one per call
*/

// Removal is returned rather than discarded because deletion is irreversible and the
// counts are the only record of what it took with it. A caller that sees
// PrivateViewsDeleted: 9 has been told something it cannot find out afterwards.
type Removal struct {
	PrivateViewsDeleted int
	SharedViewsKept     int
}

/*
┏━ Service ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  sole owner of the account table
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      pool     *pgxpool.Pool    unexported, no reach-through
┣━ methods ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Delete(ctx, accountID)   →  Removal, error
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      NewService                  one per process
*/

type Service struct {
	pool *pgxpool.Pool
}

// NewService returns a Service owning the account table through pool.
func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

/*
┌─ account ───────────────────────────────────────
│  deletes an account, and its private saved views
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string
├─ out ───────────────────────────────────────────
│      Removal                   what went with it
│      error                     ErrNotFound
├─ example ───────────────────────────────────────
│      3 private, 2 shared  →  {3, 2}
*/

// Delete is one transaction, and the order of its three statements is the whole
// ticket.
//
// The private views go first. After the account row is gone every one of its views has
// owner_account_id NULL, so there is no longer anything in the database that says which
// rows were that account's - the information the repair needs is destroyed by the thing
// it is repairing. Sweeping first is not a tidiness preference; it is the only order in
// which the sweep is possible at all.
//
// Everything else the deletion touches is already right in the schema and is left
// alone. Sessions and API tokens CASCADE, which is correct - a credential for an
// account that no longer exists is not a credential. Memberships CASCADE. Authored
// comments and assigned tickets SET NULL, which LAM-24 settled: work outlives the
// people who touched it.
func (s *Service) Delete(ctx context.Context, accountID string) (Removal, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Removal{}, fmt.Errorf("account: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	// Counted before anything is deleted, for the same reason the sweep runs first:
	// afterwards these rows are indistinguishable from every other ownerless
	// shared view.
	var kept int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM saved_view
		  WHERE owner_account_id = $1::uuid AND is_shared`, accountID).Scan(&kept); err != nil {
		return Removal{}, fmt.Errorf("account: count shared views: %w", err)
	}

	// The repair. A private view is one nobody else could ever see, so deleting it
	// loses nothing anyone but its owner had - and its owner is being deleted.
	// Promoting it to shared would publish work its author never chose to share;
	// reassigning it would need a rule for who inherits, which nothing in this
	// schema defines.
	tag, err := tx.Exec(ctx,
		`DELETE FROM saved_view
		  WHERE owner_account_id = $1::uuid AND NOT is_shared`, accountID)
	if err != nil {
		return Removal{}, fmt.Errorf("account: delete private views: %w", err)
	}

	var gone string
	err = tx.QueryRow(ctx,
		`DELETE FROM account WHERE id = $1::uuid RETURNING id::text`, accountID).Scan(&gone)
	if errors.Is(err, pgx.ErrNoRows) {
		// The sweep above is rolled back with everything else, so a bad id does
		// not delete one account's views on its way to finding nothing.
		return Removal{}, ErrNotFound
	}
	if err != nil {
		return Removal{}, fmt.Errorf("account: delete: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Removal{}, fmt.Errorf("account: commit: %w", err)
	}

	return Removal{
		PrivateViewsDeleted: int(tag.RowsAffected()),
		SharedViewsKept:     kept,
	}, nil
}
