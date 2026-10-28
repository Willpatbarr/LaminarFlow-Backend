/*
╔═ membership.go ═══════════════════════════════════════════════════════════════════════
║  ticket · joining and leaving a sprint
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      ErrSprintNotFound         error
║      ErrNotInSprintProject     error
║      Service.AddToSprint       method
║      Service.RemoveFromSprint  method
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      internal/api  →  POST /sprints/{id}/tickets · DELETE /sprints/{id}/tickets/{ticket_id}
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package ticket

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

var (
	// ErrSprintNotFound is a sprint that is absent or out of scope.
	ErrSprintNotFound = errors.New("ticket: no such sprint")

	// ErrNotInSprintProject is a ticket that is absent, archived, out of scope, or in a
	// different project than the sprint. One answer for all of them: 0013's composite
	// keys make a foreign ticket the same fact to the database, and a caller learns only
	// that this ticket cannot go in this sprint.
	ErrNotInSprintProject = errors.New("ticket: no such ticket in this sprint's project")
)

// sprintProject is the scoping predicate for a sprint named by $1, with the caller as
// $ACCOUNT. It selects the sprint's project.
const sprintProject = `SELECT s.project_id::text FROM sprint s
	          JOIN project p ON p.id = s.project_id
	          JOIN team tm ON tm.id = p.team_id
	          JOIN workspace_member wm
	               ON wm.workspace_id = tm.workspace_id AND wm.account_id = $ACCOUNT::uuid
	         WHERE s.id = $1::uuid`

/*
┌─ ticket ────────────────────────────────────────
│  puts a live ticket in a sprint of its project
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string
│      sprintID     string
│      ticketID     string
├─ out ───────────────────────────────────────────
│      Ticket                   as written, Version + 1
│      bool                     false: it was already a member, nothing was written
│      error                    ErrSprintNotFound · ErrNotInSprintProject
├─ example ───────────────────────────────────────
│      ticket of another project  →  ErrNotInSprintProject
*/

// Joining a sprint is a change to the ticket, so it moves the ticket's version and
// updated_at like any edit and the caller announces it as ticket.updated; a client that
// upserts by version would otherwise ignore the event. Being a member already is not a
// change: it answers success, writes nothing and is not announced.
func (s *Service) AddToSprint(ctx context.Context, accountID, sprintID, ticketID string) (Ticket, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Ticket{}, false, fmt.Errorf("ticket: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	var projectID string
	err = tx.QueryRow(ctx, expand(sprintProject, "$2"), sprintID, accountID).Scan(&projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Ticket{}, false, ErrSprintNotFound
	}
	if err != nil {
		return Ticket{}, false, fmt.Errorf("ticket: sprint scope: %w", err)
	}

	// Locked, so the version it is bumped from is the one it is read at.
	var t Ticket
	err = tx.QueryRow(ctx,
		expand(`SELECT `+columns+` FROM ticket t
		         WHERE t.id = $1::uuid AND t.project_id = $2::uuid AND `+liveOnly+` AND `+callerCanReach+`
		           FOR UPDATE OF t`, "$3"),
		ticketID, projectID, accountID,
	).Scan(t.dest()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Ticket{}, false, ErrNotInSprintProject
	}
	if err != nil {
		return Ticket{}, false, fmt.Errorf("ticket: sprint ticket: %w", err)
	}

	tag, err := tx.Exec(ctx,
		`INSERT INTO ticket_sprint (ticket_id, sprint_id, project_id)
		 VALUES ($1::uuid, $2::uuid, $3::uuid) ON CONFLICT DO NOTHING`,
		ticketID, sprintID, projectID)
	if err != nil {
		return Ticket{}, false, fmt.Errorf("ticket: join sprint: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return t, false, nil
	}

	t, err = bump(ctx, tx, accountID, ticketID)
	if err != nil {
		return Ticket{}, false, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Ticket{}, false, fmt.Errorf("ticket: commit: %w", err)
	}

	return t, true, nil
}

/*
┌─ ticket ────────────────────────────────────────
│  takes a ticket out of a sprint
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string
│      sprintID     string
│      ticketID     string
├─ out ───────────────────────────────────────────
│      Ticket                   as written, Version + 1
│      bool                     false: nothing was written to announce
│      error                    ErrSprintNotFound
*/

// Not being a member is not an error: the request asked for a state and it already
// holds, so it succeeds and writes nothing. An archived ticket is taken out too, but its
// row is not touched - an archived ticket's version belongs to its restore.
func (s *Service) RemoveFromSprint(ctx context.Context, accountID, sprintID, ticketID string) (Ticket, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Ticket{}, false, fmt.Errorf("ticket: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	var projectID string
	err = tx.QueryRow(ctx, expand(sprintProject, "$2"), sprintID, accountID).Scan(&projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Ticket{}, false, ErrSprintNotFound
	}
	if err != nil {
		return Ticket{}, false, fmt.Errorf("ticket: sprint scope: %w", err)
	}

	tag, err := tx.Exec(ctx,
		`DELETE FROM ticket_sprint WHERE sprint_id = $1::uuid AND ticket_id = $2::uuid`,
		sprintID, ticketID)
	if err != nil {
		return Ticket{}, false, fmt.Errorf("ticket: leave sprint: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return Ticket{}, false, nil
	}

	t, err := bump(ctx, tx, accountID, ticketID)
	if errors.Is(err, pgx.ErrNoRows) {
		// Archived: removed from the sprint, row untouched, nothing to announce.
		if err := tx.Commit(ctx); err != nil {
			return Ticket{}, false, fmt.Errorf("ticket: commit: %w", err)
		}

		return Ticket{}, false, nil
	}
	if err != nil {
		return Ticket{}, false, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Ticket{}, false, fmt.Errorf("ticket: commit: %w", err)
	}

	return t, true, nil
}

// bump records a change made to a ticket by way of a join table: version + 1,
// updated_at and updated_by, exactly as an edit would. pgx.ErrNoRows when the ticket is
// archived or absent.
func bump(ctx context.Context, tx pgx.Tx, accountID, ticketID string) (Ticket, error) {
	var t Ticket
	err := tx.QueryRow(ctx,
		`UPDATE ticket t
		    SET updated_at = now(), updated_by = $2::uuid, version = t.version + 1
		  WHERE t.id = $1::uuid AND `+liveOnly+`
		 RETURNING `+columns,
		ticketID, accountID,
	).Scan(t.dest()...)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Ticket{}, fmt.Errorf("ticket: bump: %w", err)
	}

	return t, err
}
