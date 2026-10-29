/*
╔═ idempotency.go ══════════════════════════════════════════════════════════════════════
║  Idempotency-Key · the shared mechanism for creates without a key column (LAM-127)
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      Store     claims keys in the idempotency_key table
║      Do        runs one create at most once per (account, key, operation)
║      ErrKeyShape
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      internal/api  →  every create that is not ticket, document, comment, review
║                       request or attachment (those keep their own columns, 0032-0044)
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package idempotency

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MaxKeyLength is checked by the API and repeated as a CHECK in 0045.
const MaxKeyLength = 128

// ErrKeyShape is a key that is empty, too long or not printable ASCII.
var ErrKeyShape = errors.New("must be 1 to 128 printable ASCII characters")

// staleAfter is how long a claim with no resource may stand before another request
// takes it over: the process that made it died between claiming and recording.
const staleAfter = 30 * time.Second

// pollEvery is how often a request that lost the claim looks again.
const pollEvery = 20 * time.Millisecond

// Store is the idempotency_key table.
type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Request names one keyed create.
type Request struct {
	AccountID string
	Key       *string // nil: an ordinary create, nothing is stored
	Operation string  // the operation id, e.g. "create-epic"
}

/*
┌─ Do ────────────────────────────────────────────
│  create at most once per key, replay after
├─ in ────────────────────────────────────────────
│      req       Request
│      create    makes the resource, returns it and its id; runs only for the claim holder
│      load      reads a recorded resource as the account sees it now
│      gone      the error load returns when the resource no longer exists
├─ out ───────────────────────────────────────────
│      T         the resource
│      bool      true when it is a replay: nothing was created
│      error     ErrKeyShape · create's or load's own
*/

func Do[T any](ctx context.Context, s *Store, req Request,
	create func(context.Context) (T, string, error),
	load func(context.Context, string) (T, error),
	gone error,
) (T, bool, error) {
	var zero T

	if req.Key == nil {
		v, _, err := create(ctx)
		return v, false, err
	}
	if !validKey(*req.Key) {
		return zero, false, ErrKeyShape
	}

	for {
		claimed, err := s.claim(ctx, req)
		if err != nil {
			return zero, false, err
		}

		if claimed {
			v, id, err := create(ctx)
			if err != nil {
				s.release(req)
				return zero, false, err
			}
			if err := s.record(ctx, req, id); err != nil {
				return zero, false, err
			}

			return v, false, nil
		}

		id, stale, err := s.lookup(ctx, req)
		if err != nil {
			return zero, false, err
		}

		switch {
		case id != "":
			v, err := load(ctx, id)
			if err == nil {
				return v, true, nil
			}
			if !errors.Is(err, gone) {
				return zero, false, err
			}
			// The resource was deleted since: free the key and create afresh.
			if err := s.drop(ctx, req, id); err != nil {
				return zero, false, err
			}
		case stale:
			if err := s.drop(ctx, req, ""); err != nil {
				return zero, false, err
			}
		default:
			select {
			case <-ctx.Done():
				return zero, false, ctx.Err()
			case <-time.After(pollEvery):
			}
		}
	}
}

func validKey(k string) bool {
	if len(k) < 1 || len(k) > MaxKeyLength {
		return false
	}
	for i := 0; i < len(k); i++ {
		if k[i] < ' ' || k[i] > '~' {
			return false
		}
	}

	return true
}

// claim inserts the row; true means this request holds the claim.
func (s *Store) claim(ctx context.Context, r Request) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`INSERT INTO idempotency_key (account_id, key, operation) VALUES ($1::uuid, $2, $3)
		 ON CONFLICT DO NOTHING`, r.AccountID, *r.Key, r.Operation)
	if err != nil {
		return false, fmt.Errorf("idempotency: claim: %w", err)
	}

	return tag.RowsAffected() == 1, nil
}

// lookup reads the claim another request holds. id is the recorded resource ("" while in
// flight); stale says an in-flight claim is old enough to take over. A claim that vanished
// between the insert and here (its create failed) reads as in flight, and the next pass
// claims it.
func (s *Store) lookup(ctx context.Context, r Request) (id string, stale bool, err error) {
	var rid *string
	err = s.pool.QueryRow(ctx,
		`SELECT resource_id::text, claimed_at < now() - make_interval(secs => $4)
		   FROM idempotency_key WHERE account_id = $1::uuid AND key = $2 AND operation = $3`,
		r.AccountID, *r.Key, r.Operation, staleAfter.Seconds()).Scan(&rid, &stale)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("idempotency: lookup: %w", err)
	}
	if rid != nil {
		return *rid, false, nil
	}

	return "", stale, nil
}

func (s *Store) record(ctx context.Context, r Request, id string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE idempotency_key SET resource_id = $4::uuid
		  WHERE account_id = $1::uuid AND key = $2 AND operation = $3`,
		r.AccountID, *r.Key, r.Operation, id)
	if err != nil {
		return fmt.Errorf("idempotency: record: %w", err)
	}

	return nil
}

// release drops a claim whose create failed. It uses its own context: the request's may be
// the reason the create failed, and a claim left behind blocks the retry for 30 seconds.
func (s *Store) release(r Request) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, _ = s.pool.Exec(ctx,
		`DELETE FROM idempotency_key
		  WHERE account_id = $1::uuid AND key = $2 AND operation = $3 AND resource_id IS NULL`,
		r.AccountID, *r.Key, r.Operation)
}

// drop removes the row the caller saw: the one pointing at id (id != "") or a stale claim.
// Conditional, so two requests dropping the same row cannot drop a fresh claim.
func (s *Store) drop(ctx context.Context, r Request, id string) error {
	var err error
	if id != "" {
		_, err = s.pool.Exec(ctx,
			`DELETE FROM idempotency_key
			  WHERE account_id = $1::uuid AND key = $2 AND operation = $3 AND resource_id = $4::uuid`,
			r.AccountID, *r.Key, r.Operation, id)
	} else {
		_, err = s.pool.Exec(ctx,
			`DELETE FROM idempotency_key
			  WHERE account_id = $1::uuid AND key = $2 AND operation = $3
			    AND resource_id IS NULL AND claimed_at < now() - make_interval(secs => $4)`,
			r.AccountID, *r.Key, r.Operation, staleAfter.Seconds())
	}
	if err != nil {
		return fmt.Errorf("idempotency: drop: %w", err)
	}

	return nil
}
