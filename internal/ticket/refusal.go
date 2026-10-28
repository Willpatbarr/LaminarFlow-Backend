/*
╔═ refusal.go ══════════════════════════════════════════════════════════════════════════
║  ticket · why a write that matched no row was refused
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      ErrStatusNotFound           error
║      ErrAssigneeNotFound         error
║      referencesValid             func
║      refError                    func
║      Service.afterEmptyInsert    method
║      Service.explainUpdate       method
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      CreateIdempotent  →  when the INSERT returns no row
║      Update            →  when the UPDATE returns no row
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package ticket

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrStatusNotFound is one answer for three different facts: no such status, a status
// belonging to a different team than the ticket's project, and (rarely) a status deleted
// between the check and the write. The caller can do nothing different in any of them -
// pick a status from the project's own team - and telling them apart would confirm that
// a status id exists in a team the caller may not be able to see.
var ErrStatusNotFound = errors.New("ticket: no such status for this project")

// ErrAssigneeNotFound is the same collapse for the assignee: an id that is no account,
// and an account that is not a member of the ticket's workspace, are one answer. The
// second case must not be distinguishable from the first, or the error becomes a way to
// ask whether an account id exists anywhere on the instance.
var ErrAssigneeNotFound = errors.New("ticket: no such assignee in this workspace")

// The names Postgres gave the two foreign keys in 0011, which is how a violation that
// slips past the in-statement checks is told apart from any other. By name rather than
// by the text of the message: the message is prose that changes with the server's
// language and version, the name is part of the schema. The schema test in this package
// fails if a migration renames either.
const (
	statusForeignKey    = "ticket_status_id_fkey"
	assigneeForeignKey  = "ticket_assignee_account_id_fkey"
	epicForeignKey      = "ticket_epic_fkey"
	milestoneForeignKey = "ticket_milestone_fkey"
	labelForeignKey     = "ticket_label_label_fkey"
)

// ErrEpicNotFound and ErrMilestoneNotFound (LAM-93) collapse like the status: no such
// epic and an epic of another project are one answer, because 0022's and 0033's
// composite keys make "another project's" the same fact to the database, and telling the
// caller which would confirm that an id exists somewhere they may not see.
var (
	ErrEpicNotFound      = errors.New("ticket: no such epic in this project")
	ErrMilestoneNotFound = errors.New("ticket: no such milestone in this project")
)

// How the schema relates the tables, which decides what the service has to check:
//
//	status.team_id   -> team            a status belongs to one team
//	project.team_id  -> team            a project belongs to one team
//	ticket.status_id -> status(id)      a plain foreign key, NOT composite
//
// So the database guarantees the status exists and nothing more: a ticket in team A's
// project may be pointed at team B's status and Postgres accepts it. 0013's composite-key
// pattern would close that, but it needs a team_id on ticket, which 0011 left out
// deliberately and a migration that adds and backfills it is not this ticket's to make.
// The check therefore lives here, as a predicate of the writing statement itself.
//
// The assignee is the same shape: assignee_account_id references account(id), so any
// account will do as far as Postgres is concerned, and workspace membership is a join.

// statusValid is true when the status is absent, or exists in the given team.
func statusValid(teamAlias, param string) string {
	return fmt.Sprintf(`(%[2]s::uuid IS NULL OR EXISTS (
		        SELECT 1 FROM status rs WHERE rs.id = %[2]s::uuid AND rs.team_id = %[1]s.id))`,
		teamAlias, param)
}

// assigneeValid is true when the assignee is absent, or a member of the team's workspace.
func assigneeValid(teamAlias, param string) string {
	return fmt.Sprintf(`(%[2]s::uuid IS NULL OR EXISTS (
		        SELECT 1 FROM workspace_member rwm
		         WHERE rwm.account_id = %[2]s::uuid AND rwm.workspace_id = %[1]s.workspace_id))`,
		teamAlias, param)
}

// epicValid and milestoneValid are true when the value is absent, or exists in the
// project. expr is a uuid-typed SQL expression - the value the statement is about to
// write, which for an update that leaves the column alone is the column itself.
func epicValid(projectAlias, expr string) string {
	return fmt.Sprintf(`(%[2]s IS NULL OR EXISTS (
		        SELECT 1 FROM epic re WHERE re.id = %[2]s AND re.project_id = %[1]s.id))`, projectAlias, expr)
}

func milestoneValid(projectAlias, expr string) string {
	return fmt.Sprintf(`(%[2]s IS NULL OR EXISTS (
		        SELECT 1 FROM milestone rm WHERE rm.id = %[2]s AND rm.project_id = %[1]s.id))`, projectAlias, expr)
}

// hierarchyValid is both, for a statement that writes.
func hierarchyValid(projectAlias, epicExpr, milestoneExpr string) string {
	return epicValid(projectAlias, epicExpr) + ` AND ` + milestoneValid(projectAlias, milestoneExpr)
}

// after is the value an update writes to a column a Ref governs.
func after(setParam, idParam, column string) string {
	return fmt.Sprintf(`(CASE WHEN %s::boolean THEN %s::uuid ELSE %s END)`, setParam, idParam, column)
}

// What Update writes to the two columns, with its own parameter numbers.
var (
	epicAfter      = after("$8", "$9", "t.epic_id")
	milestoneAfter = after("$10", "$11", "t.milestone_id")
)

// referencesValid is both, for a statement that writes.
func referencesValid(teamAlias, statusParam, assigneeParam string) string {
	return statusValid(teamAlias, statusParam) + ` AND ` + assigneeValid(teamAlias, assigneeParam)
}

/*
┌─ ticket ────────────────────────────────────────
│  maps a foreign-key violation to its field's error
├─ in ────────────────────────────────────────────
│      err    error      from a write
├─ out ───────────────────────────────────────────
│      error             ErrStatusNotFound · ErrAssigneeNotFound · nil
├─ example ───────────────────────────────────────
│      23503 on ticket_status_id_fkey  →  ErrStatusNotFound
*/

// refError returns nil for anything that is not one of the two foreign keys, so the
// caller falls through to its own wrapping.
func refError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
		return nil
	}

	switch pgErr.ConstraintName {
	case statusForeignKey:
		return ErrStatusNotFound
	case assigneeForeignKey:
		return ErrAssigneeNotFound
	case epicForeignKey:
		return ErrEpicNotFound
	case milestoneForeignKey:
		return ErrMilestoneNotFound
	case labelForeignKey:
		// A label deleted between the check and the insert. Which entry is not known.
		return &LabelError{Index: -1}
	}

	return nil
}

/*
┌─ Service ───────────────────────────────────────
│  explains an INSERT that wrote nothing
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      tx           pgx.Tx           the INSERT's own transaction
│      accountID    string
│      p            CreateParams
├─ out ───────────────────────────────────────────
│      Ticket                        the original, on a replay
│      bool                          true: it was a replay
│      error                         ErrNotFound · ErrStatusNotFound
│                                    ErrAssigneeNotFound · *Conflict · fault
├─ example ───────────────────────────────────────
│      key used, ticket live      →  original, true
│      key used, ticket archived  →  *Conflict{Archived: true}
*/

// The order of the questions is the order of precedence. A key already used answers
// first, because a replay returns what the first call made whatever the body says now -
// including a status that has since been deleted, which would otherwise turn a retry of
// a create that succeeded into a 422. Then the project, then the status, then the
// assignee: one answer at a time, the first thing the caller has to fix.
func (s *Service) afterEmptyInsert(ctx context.Context, tx pgx.Tx, accountID string, p CreateParams) (Ticket, bool, error) {
	if p.IdempotencyKey != nil {
		var (
			cur        Ticket
			archivedAt *time.Time
			archivedBy *string
		)

		// The second place ticket is read without liveOnly, after conflict.go's, and
		// for the same reason: whether the ticket is archived is half of the answer.
		// Scoped by the creator and by callerCanReach, so it finds only this account's
		// own ticket and only while the account can still reach its project.
		err := tx.QueryRow(ctx,
			expand(`SELECT `+columns+`, t.archived_at, t.archived_by::text FROM ticket t
			         WHERE t.created_by = $1::uuid AND t.idempotency_key = $2::text AND `+callerCanReach, "$1"),
			accountID, *p.IdempotencyKey,
		).Scan(append(cur.dest(), &archivedAt, &archivedBy)...)
		switch {
		case err == nil && archivedAt != nil:
			return Ticket{}, false, &Conflict{Archived: true, Replay: true, Current: cur, ChangedBy: archivedBy, ChangedAt: *archivedAt}
		case err == nil:
			return cur, true, nil
		case !errors.Is(err, pgx.ErrNoRows):
			return Ticket{}, false, fmt.Errorf("ticket: read by key: %w", err)
		}
	}

	var statusOK, assigneeOK, epicOK, milestoneOK bool
	err := tx.QueryRow(ctx,
		`SELECT `+statusValid("tm", "$2")+`, `+assigneeValid("tm", "$3")+`,
		        `+epicValid("p", "$5::uuid")+`, `+milestoneValid("p", "$6::uuid")+`
		   FROM project p
		   JOIN team tm ON tm.id = p.team_id
		   JOIN workspace_member wm
		        ON wm.workspace_id = tm.workspace_id AND wm.account_id = $4::uuid
		  WHERE p.id = $1::uuid`,
		p.ProjectID, p.StatusID, p.AssigneeID, accountID, p.EpicID, p.MilestoneID,
	).Scan(&statusOK, &assigneeOK, &epicOK, &milestoneOK)
	if errors.Is(err, pgx.ErrNoRows) {
		return Ticket{}, false, ErrNotFound
	}
	if err != nil {
		return Ticket{}, false, fmt.Errorf("ticket: explain insert: %w", err)
	}

	switch {
	case !statusOK:
		return Ticket{}, false, ErrStatusNotFound
	case !assigneeOK:
		return Ticket{}, false, ErrAssigneeNotFound
	case !epicOK:
		return Ticket{}, false, ErrEpicNotFound
	case !milestoneOK:
		return Ticket{}, false, ErrMilestoneNotFound
	}

	// Every precondition holds now, and the key is unused, yet nothing was written. The
	// state moved between the two statements in a way that cannot be reconstructed;
	// reporting a fault is better than guessing at a refusal.
	return Ticket{}, false, errors.New("ticket: insert matched no row with every precondition satisfied")
}

/*
┌─ Service ───────────────────────────────────────
│  explains an UPDATE that wrote nothing
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      tx           pgx.Tx           the UPDATE's own transaction
│      accountID    string
│      p            UpdateParams
├─ out ───────────────────────────────────────────
│      error                         ErrNotFound · *Conflict · ErrStatusNotFound
│                                    ErrAssigneeNotFound · fault
├─ example ───────────────────────────────────────
│      live, version ok, status in another team  →  ErrStatusNotFound
*/

// The ticket itself is asked about first, and only a ticket the caller can reach, live,
// at the version they sent, gets as far as a complaint about its status. So a stale write
// is a 409 even when its status is also wrong - the client has to reconcile with the
// newer ticket before its status choice means anything - and a ticket the caller cannot
// reach never learns whether an id it sent was valid.
func (s *Service) explainUpdate(ctx context.Context, tx pgx.Tx, accountID string, p UpdateParams) error {
	var (
		version                              int
		statusOK, assigneeOK, epicOK, mileOK bool
	)
	err := tx.QueryRow(ctx,
		expand(`SELECT t.version, `+statusValid("ttm", "$2")+`, `+assigneeValid("ttm", "$3")+`,
		               `+epicValid("tp", after("$5", "$6", "t.epic_id"))+`,
		               `+milestoneValid("tp", after("$7", "$8", "t.milestone_id"))+`
		          FROM ticket t
		          JOIN project tp ON tp.id = t.project_id
		          JOIN team ttm ON ttm.id = tp.team_id
		         WHERE t.id = $1::uuid AND `+liveOnly+` AND `+callerCanReach, "$4"),
		p.ID, p.StatusID, p.AssigneeID, accountID,
		p.Epic.Set, p.Epic.ID, p.Milestone.Set, p.Milestone.ID,
	).Scan(&version, &statusOK, &assigneeOK, &epicOK, &mileOK)
	if errors.Is(err, pgx.ErrNoRows) {
		return s.noRow(ctx, tx, accountID, p.ID, p.IfVersion, false)
	}
	if err != nil {
		return fmt.Errorf("ticket: explain update: %w", err)
	}

	if p.IfVersion != nil && version != *p.IfVersion {
		return s.noRow(ctx, tx, accountID, p.ID, p.IfVersion, false)
	}

	switch {
	case !statusOK:
		return ErrStatusNotFound
	case !assigneeOK:
		return ErrAssigneeNotFound
	case !epicOK:
		return ErrEpicNotFound
	case !mileOK:
		return ErrMilestoneNotFound
	}

	return s.noRow(ctx, tx, accountID, p.ID, p.IfVersion, false)
}
