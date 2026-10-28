/*
╔═ labels.go ═══════════════════════════════════════════════════════════════════════════
║  ticket · a ticket's labels, written with the ticket
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      LabelError      struct
║      ErrLabelNotFound  error
║      setLabels       func
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      CreateIdempotent · Update  →  when the write carries label_ids
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package ticket

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/jackc/pgx/v5"
)

// ErrLabelNotFound is one answer for an id that is no label and a label of another team
// than the ticket's project's, for the reason ErrStatusNotFound gives.
var ErrLabelNotFound = errors.New("ticket: no such label for this project's team")

var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// LabelError says which entry of label_ids was refused. Index is -1 when a label went
// away between the check and the write and the entry is not known.
type LabelError struct {
	Index int
}

func (e *LabelError) Error() string {
	return fmt.Sprintf("%v (label_ids[%d])", ErrLabelNotFound, e.Index)
}
func (e *LabelError) Unwrap() error { return ErrLabelNotFound }

/*
┌─ ticket ────────────────────────────────────────
│  makes a ticket's labels exactly ids
├─ in ────────────────────────────────────────────
│      ctx    context.Context
│      tx     pgx.Tx           the ticket write's own transaction
│      t      *Ticket          as just written; LabelIDs is set on the way out
│      ids    []string         may be empty, which clears; duplicates are one label
├─ out ───────────────────────────────────────────
│      error                   *LabelError · fault
├─ example ───────────────────────────────────────
│      ids[1] is another team's label  →  *LabelError{Index: 1}
*/

// Every id must be a label of the team that owns the ticket's project, checked in the
// same transaction as the write and in the order given, so the error names the first
// bad entry. The composite keys of 0024 would refuse a foreign label on their own, but
// with a constraint name and no index, and as a 23503 after half the work.
//
// Rows that stay are not rewritten, so created_at on ticket_label means "labelled since".
func setLabels(ctx context.Context, tx pgx.Tx, t *Ticket, ids []string) error {
	for i, id := range ids {
		if !uuidRE.MatchString(id) {
			return &LabelError{Index: i}
		}
	}

	if len(ids) > 0 {
		var bad int
		err := tx.QueryRow(ctx,
			`SELECT u.ord::int - 1
			   FROM unnest($1::text[]) WITH ORDINALITY AS u(id, ord)
			  WHERE NOT EXISTS (
			        SELECT 1 FROM label l JOIN project p ON p.team_id = l.team_id
			         WHERE l.id = u.id::uuid AND p.id = $2::uuid)
			  ORDER BY u.ord LIMIT 1`,
			ids, t.ProjectID).Scan(&bad)
		switch {
		case err == nil:
			return &LabelError{Index: bad}
		case !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("ticket: check labels: %w", err)
		}
	}

	if _, err := tx.Exec(ctx,
		`DELETE FROM ticket_label WHERE ticket_id = $1::uuid AND label_id <> ALL($2::text[]::uuid[])`,
		t.ID, ids); err != nil {
		return fmt.Errorf("ticket: clear labels: %w", err)
	}

	if len(ids) > 0 {
		_, err := tx.Exec(ctx,
			`INSERT INTO ticket_label (ticket_id, label_id, project_id, team_id)
			 SELECT $1::uuid, l.id, $2::uuid, l.team_id
			   FROM label l WHERE l.id = ANY($3::text[]::uuid[])
			 ON CONFLICT DO NOTHING`,
			t.ID, t.ProjectID, ids)
		if err := refError(err); err != nil {
			return err
		}
		if err != nil {
			return fmt.Errorf("ticket: write labels: %w", err)
		}
	}

	// Read back rather than echo ids: the order is the one every other read uses, and
	// duplicates are gone.
	t.LabelIDs = nil
	if err := tx.QueryRow(ctx,
		`SELECT ARRAY(SELECT tl.label_id::text FROM ticket_label tl WHERE tl.ticket_id = $1::uuid ORDER BY tl.label_id)`,
		t.ID).Scan(&t.LabelIDs); err != nil {
		return fmt.Errorf("ticket: read labels: %w", err)
	}

	return nil
}
