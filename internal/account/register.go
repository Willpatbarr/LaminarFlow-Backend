/*
╔═ register.go ═════════════════════════════════════════════════════════════════════════
║  account · creation, and who an account is
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      NewAccount       struct
║      Registration     struct
║      Profile          struct
║      ErrEmailTaken    error
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      internal/api  →  POST /api/v1/auth/register · GET /api/v1/auth/me
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package account

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrEmailTaken means an account with this address already exists, compared
// case-insensitively - 0007's unique index is on lower(email).
var ErrEmailTaken = errors.New("account: email already registered")

const (
	// defaultWorkspaceName is what a workspace created by registration is called. It
	// matches the row migration 0003 seeds, so an install that reached "no workspace"
	// by deleting that row gets it back under the same name.
	defaultWorkspaceName = "Default"

	// registrationLock serialises registrations. The value is arbitrary; it only has
	// to be one no other advisory lock in this module uses, and none does.
	registrationLock int64 = 0x4c414d3631 // "LAM61"
)

/*
┏━ NewAccount ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  what Register needs to create an account
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Email           string    stored as given, matched lower-cased
┃      PasswordHash    string    already hashed, never plaintext
┃      DisplayName     string
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      the register handler        one per request
*/

// NewAccount takes a hash and not a password: hashing is internal/auth's, and one
// implementation of it is the point. This package never sees plaintext.
type NewAccount struct {
	Email        string
	PasswordHash string
	DisplayName  string
}

/*
┏━ Registration ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  what Register created or joined
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      AccountID          string
┃      WorkspaceID        string
┃      Role               string    owner for the first member, else member
┃      WorkspaceCreated   bool      true only on an install with none
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Register                    one per call
*/

type Registration struct {
	AccountID        string
	WorkspaceID      string
	Role             string
	WorkspaceCreated bool
}

/*
┏━ Profile ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  the parts of an account a client displays
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Email          string
┃      DisplayName    string
┃      WorkspaceID    string    empty when the account has no membership
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Profile                     one per call
*/

type Profile struct {
	Email       string
	DisplayName string
	WorkspaceID string
}

/*
┌─ account ───────────────────────────────────────
│  creates an account and puts it in the workspace
├─ in ────────────────────────────────────────────
│      ctx    context.Context
│      in     NewAccount
├─ out ───────────────────────────────────────────
│      Registration            ids, role, whether a workspace was made
│      error                   ErrEmailTaken
├─ example ───────────────────────────────────────
│      empty install  →  new workspace, owner
*/

// Register is one transaction: account, then workspace if there is none, then the
// membership. All three or none, so there is no moment at which an account exists with
// nowhere to go - the state a half-finished sign-up would leave, and one the frontend
// could not recover from, because me would answer with no workspace and nothing in the
// API can add one.
//
// Single-workspace model, ADR 0004. If a workspace exists the account joins the oldest
// one as a member; if none does, this creates it and the account owns it. Joining is
// unconditional because there is no invitation or approval step yet - that is a
// recorded decision, not an oversight.
//
// The advisory lock is what makes "none exists" mean something. Without it two first
// registrations can both see an empty workspace table and each create one, leaving two
// workspaces and an owner of each; the second would never be noticed because every
// later registration joins only the oldest.
func (s *Service) Register(ctx context.Context, in NewAccount) (Registration, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Registration{}, fmt.Errorf("account: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, registrationLock); err != nil {
		return Registration{}, fmt.Errorf("account: lock registration: %w", err)
	}

	var out Registration

	err = tx.QueryRow(ctx,
		`INSERT INTO account (email, password_hash, display_name)
		 VALUES ($1, $2, $3) RETURNING id::text`,
		strings.TrimSpace(in.Email), in.PasswordHash, in.DisplayName,
	).Scan(&out.AccountID)
	if isUniqueViolation(err, "account_email_lower_key") {
		return Registration{}, ErrEmailTaken
	}
	if err != nil {
		return Registration{}, fmt.Errorf("account: insert: %w", err)
	}

	// Oldest first, with id as the tiebreak, so "the workspace" is the same row on
	// every call and Profile can report the same one.
	err = tx.QueryRow(ctx,
		`SELECT id::text FROM workspace ORDER BY created_at, id LIMIT 1`,
	).Scan(&out.WorkspaceID)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx,
			`INSERT INTO workspace (name) VALUES ($1) RETURNING id::text`, defaultWorkspaceName,
		).Scan(&out.WorkspaceID)
		out.WorkspaceCreated = true
	}
	if err != nil {
		return Registration{}, fmt.Errorf("account: find or create workspace: %w", err)
	}

	// The first member of a workspace owns it, including the workspace migration 0003
	// seeded, which exists on every install but has nobody in it. The role has no
	// meaning yet (no authority model); recording who came first costs nothing now
	// and is unrecoverable later.
	err = tx.QueryRow(ctx,
		`INSERT INTO workspace_member (workspace_id, account_id, role)
		 VALUES ($1::uuid, $2::uuid,
		         CASE WHEN EXISTS (SELECT 1 FROM workspace_member WHERE workspace_id = $1::uuid)
		              THEN 'member' ELSE 'owner' END)
		 RETURNING role`,
		out.WorkspaceID, out.AccountID,
	).Scan(&out.Role)
	if err != nil {
		return Registration{}, fmt.Errorf("account: insert membership: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Registration{}, fmt.Errorf("account: commit: %w", err)
	}

	return out, nil
}

/*
┌─ account ───────────────────────────────────────
│  reads what a client shows for an account
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string
├─ out ───────────────────────────────────────────
│      Profile                 oldest membership's workspace
│      error                   ErrNotFound
├─ example ───────────────────────────────────────
│      one member of Default  →  its id, email, name
*/

func (s *Service) Profile(ctx context.Context, accountID string) (Profile, error) {
	var (
		p         Profile
		workspace *string
	)

	err := s.pool.QueryRow(ctx,
		`SELECT a.email, a.display_name,
		        (`+WorkspaceOf("a.id")+`)::text
		   FROM account a WHERE a.id = $1::uuid`, accountID,
	).Scan(&p.Email, &p.DisplayName, &workspace)
	if errors.Is(err, pgx.ErrNoRows) {
		return Profile{}, ErrNotFound
	}
	if err != nil {
		return Profile{}, fmt.Errorf("account: profile: %w", err)
	}

	if workspace != nil {
		p.WorkspaceID = *workspace
	}

	return p, nil
}

/*
┌─ account ───────────────────────────────────────
│  the name a client shows for someone else's edit
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string
├─ out ───────────────────────────────────────────
│      string                  the display name
│      error                   ErrNotFound
├─ example ───────────────────────────────────────
│      Sarah's id  →  "Sarah Okafor"
*/

// DisplayName exists so that a package which must say who did something does not read
// the account table to find out. Ticket conflicts name the account that changed the
// ticket, and sqlguard keeps account reads to this package and auth - joining it from
// internal/ticket would mean widening that rule for one column.
func (s *Service) DisplayName(ctx context.Context, accountID string) (string, error) {
	var name string

	err := s.pool.QueryRow(ctx,
		`SELECT display_name FROM account WHERE id = $1::uuid`, accountID).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("account: display name: %w", err)
	}

	return name, nil
}

// isUniqueViolation reports a 23505 on the named constraint or index. Named, because a
// unique violation on some other constraint is a bug to surface, not an email to blame.
func isUniqueViolation(err error, constraint string) bool {
	var pg *pgconn.PgError

	return errors.As(err, &pg) && pg.Code == "23505" && pg.ConstraintName == constraint
}
