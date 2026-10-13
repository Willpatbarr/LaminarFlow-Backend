/*
╔═ conflict.go ═════════════════════════════════════════════════════════════════════════
║  ticket · what a write that lost the race is told
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      Conflict         struct
║      Service.noRow    method
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      Update · Archive · Unarchive  →  when a conditional write matches no row
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package ticket

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

/*
┏━ Conflict ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  a conditional write the ticket's current state refused
┣━ type ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      error
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Archived     bool          the ticket is archived, whatever the version
┃      Current      Ticket        the server's copy, archived or not
┃      ChangedBy    *string       account id, null if not recorded
┃      ChangedAt    time.Time     when that change was made
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Update · Archive · Unarchive     one per refused write
*/

// Conflict is the error for a write that carried a version and lost. It is a type
// rather than a sentinel because the caller needs more than the fact: the whole point
// of refusing, instead of overwriting, is to hand back what is there now and who put it
// there, so a client can show both versions and let a person choose.
//
// Archived separates the two things that can have happened. A stale version means
// someone edited it and Current is their edit; Archived means someone removed it, and
// ChangedBy and ChangedAt describe the archive rather than the last edit. In both cases
// Current is the full row - including an archived one, which no read returns, because
// a client holding unsynced edits to a ticket needs the other side of the conflict even
// when that other side is "gone".
type Conflict struct {
	Archived  bool
	Current   Ticket
	ChangedBy *string
	ChangedAt time.Time
}

func (c *Conflict) Error() string {
	if c.Archived {
		return "ticket: archived since the version the caller saw"
	}

	return fmt.Sprintf("ticket: changed since the version the caller saw (now %d)", c.Current.Version)
}

/*
┌─ Service ───────────────────────────────────────
│  explains why a write matched no row
├─ in ────────────────────────────────────────────
│      ctx            context.Context
│      tx             pgx.Tx           the write's own transaction
│      accountID      string
│      id             string
│      ifVersion      *int             nil = no precondition
│      needsArchived  bool             Unarchive's row must be archived
├─ out ───────────────────────────────────────────
│      error                      ErrNotFound · *Conflict · fault
├─ example ───────────────────────────────────────
│      Update, v4 sent, row at v5  →  *Conflict{Archived: false}
*/

// noRow turns "the UPDATE matched nothing" into the right answer, and it runs in the
// UPDATE's own transaction so the row it reads is the one the UPDATE just failed
// against. READ COMMITTED gives each statement its own snapshot, so after a writer we
// waited on has committed, this sees what it committed.
//
// With no precondition the answer is what it always was - not found - and no second
// statement runs, so a caller who sends no version pays nothing for this.
//
// The read here is the one deliberate exception to liveOnly: it has to see archived
// rows, because "archived" is half of what it reports. It is still scoped by
// callerCanReach, so a caller who cannot reach the ticket gets not found rather than a
// copy of it, and it is reachable only from a write that carried a version.
func (s *Service) noRow(ctx context.Context, tx pgx.Tx, accountID, id string, ifVersion *int, needsArchived bool) error {
	if ifVersion == nil {
		return ErrNotFound
	}

	var (
		cur        Ticket
		archivedAt *time.Time
		archivedBy *string
	)
	err := tx.QueryRow(ctx,
		expand(`SELECT `+columns+`, t.archived_at, t.archived_by::text FROM ticket t
		         WHERE t.id = $1::uuid AND `+callerCanReach, "$2"),
		id, accountID,
	).Scan(append(cur.dest(), &archivedAt, &archivedBy)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("ticket: read current: %w", err)
	}

	archived := archivedAt != nil

	// An archived ticket answers "archived" whatever version was sent, to anything but
	// the restore that is meant for it.
	if archived && !needsArchived {
		return &Conflict{Archived: true, Current: cur, ChangedBy: archivedBy, ChangedAt: *archivedAt}
	}

	if cur.Version == *ifVersion {
		// The version is right, so the write can only have been refused for being the
		// wrong verb for the state: restoring a ticket that is not archived. That is
		// what the same request without a version has always answered.
		if archived != needsArchived {
			return ErrNotFound
		}

		// Right version, right state, and still no row. Nothing can produce this,
		// because version only rises. Surfacing it as a fault is better than
		// reporting a conflict whose Current is the version the caller sent.
		return fmt.Errorf("ticket: write matched no row at version %d with the precondition satisfied", *ifVersion)
	}

	return &Conflict{Current: cur, ChangedBy: cur.UpdatedBy, ChangedAt: cur.UpdatedAt}
}
