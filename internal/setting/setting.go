/*
╔═ setting.go ══════════════════════════════════════════════════════════════════════════
║  setting · the only write path, and the only thing that reads the table
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      Service        struct
║      Target         struct
║      Unknown        struct
║      ErrNotFound    error
║      ErrNotSet      error
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      internal/api  →  get · set · list · the unregistered report
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package setting

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound means the workspace or team is absent or out of the caller's reach - the
// same collapse internal/ticket, internal/document and internal/aspect make.
var ErrNotFound = errors.New("setting: not found")

// ErrNotSet means the key is real and reachable, and nothing has been written. It is
// separate from ErrNotFound because a caller does something different with each: fall
// back to a default, versus stop.
var ErrNotSet = errors.New("setting: not set")

/*
┏━ Target ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  which workspace or team a setting hangs on
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Scope    Scope     workspace · team
┃      ID       string    the workspace's or the team's
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      internal/api  →  from the request path
*/

// Target is a pair rather than two nullable fields, because 0009's arc says exactly one
// scope and a struct with two pointers would let a caller express zero or both.
type Target struct {
	Scope Scope
	ID    string
}

/*
┏━ Service ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  sole owner of the setting table
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      pool     *pgxpool.Pool    unexported, no reach-through
┣━ methods ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Set(ctx, accountID, target, key, value)    →  error
┃      Get(ctx, accountID, target, key)           →  json.RawMessage, error
┃      List(ctx, accountID, target)               →  map[Key]…, error
┃      Unset(ctx, accountID, target, key)         →  error
┃      Unregistered(ctx)                          →  []Unknown, error
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      NewService                  one per process
*/

type Service struct {
	pool *pgxpool.Pool
}

// NewService returns a Service owning the setting table through pool.
func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

// reachable proves the caller is a member of the workspace the target belongs to, and
// returns nothing but an error.
//
// A separate round trip rather than a predicate folded into each statement, because the
// two scopes need different joins and an upsert cannot carry a WHERE on the conflict
// path anyway. Set runs it inside its own transaction; the reads do not need to, since
// a read that races a membership change may answer either way and both are true.
func (s *Service) reachable(ctx context.Context, q querier, accountID string, t Target) error {
	var query string
	switch t.Scope {
	case WorkspaceScope:
		query = `SELECT 1 FROM workspace_member wm
		          WHERE wm.workspace_id = $1::uuid AND wm.account_id = $2::uuid`
	case TeamScope:
		query = `SELECT 1 FROM team tm
		           JOIN workspace_member wm
		                ON wm.workspace_id = tm.workspace_id AND wm.account_id = $2::uuid
		          WHERE tm.id = $1::uuid`
	default:
		return fmt.Errorf("%w: %q", ErrWrongScope, t.Scope)
	}

	var one int
	err := q.QueryRow(ctx, query, t.ID, accountID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("setting: reachable: %w", err)
	}

	return nil
}

/*
┌─ setting ───────────────────────────────────────
│  writes one registered setting
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string
│      t            Target
│      key          Key
│      value        json.RawMessage
├─ out ───────────────────────────────────────────
│      error        ErrNotFound · ErrUnknownKey ·
│                   ErrWrongScope · ErrWrongShape
├─ example ───────────────────────────────────────
│      sprint.length_days, team, 14  →  nil
*/

// Set is an upsert, and it is the only way a row reaches this table. Check runs first
// and is what makes the package worth having: an unregistered key, a key at the wrong
// level, or a value of the wrong shape all fail here, loudly, at the writer - rather
// than at some reader weeks later that quietly uses its default instead.
func (s *Service) Set(ctx context.Context, accountID string, t Target, key Key, value json.RawMessage) error {
	if err := Check(key, t.Scope, value); err != nil {
		return err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("setting: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	if err := s.reachable(ctx, tx, accountID, t); err != nil {
		return err
	}

	// Two statements rather than one parameterised by scope: ON CONFLICT has to
	// name a partial index's predicate to use it, and the two indexes have
	// different ones. A single statement would need the column and the predicate
	// both interpolated, which is string building where a branch will do.
	if t.Scope == WorkspaceScope {
		_, err = tx.Exec(ctx,
			`INSERT INTO setting (workspace_id, key, value) VALUES ($1::uuid, $2, $3)
			 ON CONFLICT (workspace_id, key) WHERE workspace_id IS NOT NULL
			 DO UPDATE SET value = EXCLUDED.value, updated_at = now()`,
			t.ID, key.String(), value)
	} else {
		_, err = tx.Exec(ctx,
			`INSERT INTO setting (team_id, key, value) VALUES ($1::uuid, $2, $3)
			 ON CONFLICT (team_id, key) WHERE team_id IS NOT NULL
			 DO UPDATE SET value = EXCLUDED.value, updated_at = now()`,
			t.ID, key.String(), value)
	}
	if err != nil {
		return fmt.Errorf("setting: set: %w", err)
	}

	return tx.Commit(ctx)
}

/*
┌─ setting ───────────────────────────────────────
│  reads one registered setting
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string
│      t            Target
│      key          Key
├─ out ───────────────────────────────────────────
│      json.RawMessage
│      error        ErrNotFound · ErrNotSet · ErrUnknownKey
*/

// Get refuses an unregistered key rather than answering ErrNotSet for it. Those are
// different facts - "nobody has configured this" and "there is no such setting" - and
// collapsing them is how a typo in a reader becomes a default that looks deliberate.
func (s *Service) Get(ctx context.Context, accountID string, t Target, key Key) (json.RawMessage, error) {
	def, ok := Registry[key]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownKey, key)
	}
	if def.Scope != t.Scope {
		return nil, fmt.Errorf("%w: %q is %s-scoped, not %s", ErrWrongScope, key, def.Scope, t.Scope)
	}

	if err := s.reachable(ctx, s.pool, accountID, t); err != nil {
		return nil, err
	}

	column := "workspace_id"
	if t.Scope == TeamScope {
		column = "team_id"
	}

	var value json.RawMessage
	err := s.pool.QueryRow(ctx,
		`SELECT s.value FROM setting s WHERE s.`+column+` = $1::uuid AND s.key = $2`,
		t.ID, key.String()).Scan(&value)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotSet
	}
	if err != nil {
		return nil, fmt.Errorf("setting: get: %w", err)
	}

	return value, nil
}

/*
┌─ setting ───────────────────────────────────────
│  every registered setting on one target
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string
│      t            Target
├─ out ───────────────────────────────────────────
│      map[Key]json.RawMessage
│      error        ErrNotFound
*/

// List answers LAM-52 decision 3 for reads: a row whose key is not registered is
// skipped, not returned and not an error.
//
// Rejecting on read would turn one bad row - written before this package existed, or by
// hand - into a broken settings screen for the whole team, which is a worse failure
// than the one being fixed. Unregistered is where those rows are visible instead.
func (s *Service) List(ctx context.Context, accountID string, t Target) (map[Key]json.RawMessage, error) {
	if err := s.reachable(ctx, s.pool, accountID, t); err != nil {
		return nil, err
	}

	rows, err := s.pool.Query(ctx, scopedRead(t.Scope), t.ID)
	if err != nil {
		return nil, fmt.Errorf("setting: list: %w", err)
	}
	defer rows.Close()

	out := map[Key]json.RawMessage{}
	for rows.Next() {
		var (
			key   string
			value json.RawMessage
		)
		if err := rows.Scan(&value, &key); err != nil {
			return nil, fmt.Errorf("setting: list scan: %w", err)
		}

		registered, ok := Lookup(key)
		if !ok {
			continue
		}
		out[registered] = value
	}

	return out, rows.Err()
}

/*
┌─ setting ───────────────────────────────────────
│  removes one setting, restoring its default
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string
│      t            Target
│      key          Key
├─ out ───────────────────────────────────────────
│      error        ErrNotFound · ErrNotSet
*/

// Unset deletes the row. There is no "set it back to the default" value to write -
// absence *is* the default, and writing one would freeze today's default into the row.
func (s *Service) Unset(ctx context.Context, accountID string, t Target, key Key) error {
	if err := s.reachable(ctx, s.pool, accountID, t); err != nil {
		return err
	}

	column := "workspace_id"
	if t.Scope == TeamScope {
		column = "team_id"
	}

	var gone string
	err := s.pool.QueryRow(ctx,
		`DELETE FROM setting s WHERE s.`+column+` = $1::uuid AND s.key = $2
		 RETURNING s.id::text`, t.ID, key.String()).Scan(&gone)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotSet
	}
	if err != nil {
		return fmt.Errorf("setting: unset: %w", err)
	}

	return nil
}

/*
┏━ Unknown ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  one stored row whose key is not registered
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Key         string
┃      Scope       Scope
┃      TargetID    string
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Unregistered               one per offending row
*/

type Unknown struct {
	Key      string
	Scope    Scope
	TargetID string
}

/*
┌─ setting ───────────────────────────────────────
│  every stored row this registry does not know
├─ in ────────────────────────────────────────────
│      ctx    context.Context
├─ out ───────────────────────────────────────────
│      []Unknown
│      error
├─ example ───────────────────────────────────────
│      →  [{kanban_colums, team, 8b1c…}]
*/

// Unregistered is the other half of LAM-52 decision 3: ignoring a row on read is only
// acceptable if something can still see it.
//
// Instance-wide and unscoped by design, the same call internal/search's rebuild makes -
// it is an operator's question, not a caller's, and an answer filtered to one
// workspace's rows would not tell an operator whether the instance is clean. The API
// does not expose it; a command does.
func (s *Service) Unregistered(ctx context.Context) ([]Unknown, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT s.key, s.workspace_id::text, s.team_id::text FROM setting s ORDER BY s.key`)
	if err != nil {
		return nil, fmt.Errorf("setting: unregistered: %w", err)
	}
	defer rows.Close()

	var out []Unknown
	for rows.Next() {
		var (
			key                 string
			workspaceID, teamID *string
		)
		if err := rows.Scan(&key, &workspaceID, &teamID); err != nil {
			return nil, fmt.Errorf("setting: unregistered scan: %w", err)
		}

		if _, ok := Lookup(key); ok {
			continue
		}

		u := Unknown{Key: key, Scope: WorkspaceScope}
		if teamID != nil {
			u.Scope, u.TargetID = TeamScope, *teamID
		} else if workspaceID != nil {
			u.TargetID = *workspaceID
		}
		out = append(out, u)
	}

	return out, rows.Err()
}

// scopedRead is List's statement. The column is chosen from a closed set rather than
// taken from a caller, so this is a branch rather than interpolation.
func scopedRead(scope Scope) string {
	if scope == TeamScope {
		return `SELECT s.value, s.key FROM setting s WHERE s.team_id = $1::uuid`
	}

	return `SELECT s.value, s.key FROM setting s WHERE s.workspace_id = $1::uuid`
}

// querier is whatever can run a query - the pool, or a transaction. reachable is called
// both ways: on its own before a read, and inside Set's transaction.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}
