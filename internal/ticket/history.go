/*
╔═ history.go ══════════════════════════════════════════════════════════════════════════
║  ticket · what a write records in the activity log (LAM-89)
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      snapshot        struct
║      lockSnapshot    func
║      Ticket.diff     method
║      Ticket.note     method
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      Create · Update · Move · setArchived · MoveStatus  →  inside their transactions
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package ticket

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/activity"

	"github.com/jackc/pgx/v5"
)

// snapshot is the part of a ticket the log compares: what it was before a write.
type snapshot struct {
	title, description string
	status, assignee   *string
	epic, milestone    *string
	labels             []string
}

// lockSnapshot reads the ticket FOR UPDATE, so the state it returns is the state the write
// that follows replaces. ok is false when there is no such row; the write then refuses by
// itself, with the explanation it always gave, and nothing is recorded.
func lockSnapshot(ctx context.Context, tx pgx.Tx, id string) (s snapshot, ok bool, err error) {
	err = tx.QueryRow(ctx,
		`SELECT t.title, t.description, t.status_id::text, t.assignee_account_id::text,
		        t.epic_id::text, t.milestone_id::text,
		        ARRAY(SELECT tl.label_id::text FROM ticket_label tl WHERE tl.ticket_id = t.id ORDER BY tl.label_id)
		   FROM ticket t WHERE t.id = $1::uuid FOR UPDATE OF t`, id,
	).Scan(&s.title, &s.description, &s.status, &s.assignee, &s.epic, &s.milestone, &s.labels)
	if errors.Is(err, pgx.ErrNoRows) {
		return snapshot{}, false, nil
	}
	if err != nil {
		return snapshot{}, false, fmt.Errorf("ticket: read before: %w", err)
	}

	return s, true, nil
}

// note records one entry about this ticket, made by actorID, and keeps it on the ticket so
// the API can announce it once the transaction has committed.
func (t *Ticket) note(ctx context.Context, tx activity.Tx, actorID, kind string, field, before, after *string) error {
	e, err := activity.Record(ctx, tx, activity.Entry{
		TicketID: &t.ID, ActorID: activity.Ptr(actorID),
		Kind: kind, Field: field, Before: before, After: after,
	})
	if err != nil {
		return err
	}
	t.Activity = append(t.Activity, e)

	return nil
}

func same(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}

	return *a == *b
}

// diff records one entry per thing that changed between before and the ticket as written:
// title, description, status, assignee, epic (the ticket's parent), milestone, then each
// label added and removed. A write that changes nothing records nothing, so a PUT that
// resends the same state is not history.
func (t *Ticket) diff(ctx context.Context, tx pgx.Tx, actorID string, before snapshot) error {
	text := func(field, was, now string) error {
		if was == now {
			return nil
		}
		f, b, a := field, activity.Clip(was), activity.Clip(now)

		return t.note(ctx, tx, actorID, activity.FieldChanged, &f, &b, &a)
	}
	ref := func(kind string, field *string, was, now *string) error {
		if same(was, now) {
			return nil
		}

		return t.note(ctx, tx, actorID, kind, field, was, now)
	}

	milestone := "milestone"
	for _, step := range []func() error{
		func() error { return text("title", before.title, t.Title) },
		func() error { return text("description", before.description, t.Description) },
		func() error { return ref(activity.StatusChanged, nil, before.status, t.StatusID) },
		func() error { return ref(activity.AssigneeChanged, nil, before.assignee, t.AssigneeID) },
		func() error { return ref(activity.ParentChanged, nil, before.epic, t.EpicID) },
		func() error { return ref(activity.FieldChanged, &milestone, before.milestone, t.MilestoneID) },
	} {
		if err := step(); err != nil {
			return err
		}
	}

	had := make(map[string]bool, len(before.labels))
	for _, id := range before.labels {
		had[id] = true
	}
	has := make(map[string]bool, len(t.LabelIDs))
	for _, id := range t.LabelIDs {
		has[id] = true
	}

	added, removed := []string{}, []string{}
	for id := range has {
		if !had[id] {
			added = append(added, id)
		}
	}
	for id := range had {
		if !has[id] {
			removed = append(removed, id)
		}
	}
	// Sorted, so the entries of one write come out in the same order every time.
	sort.Strings(added)
	sort.Strings(removed)

	for _, id := range added {
		id := id
		if err := t.note(ctx, tx, actorID, activity.LabelAdded, nil, nil, &id); err != nil {
			return err
		}
	}
	for _, id := range removed {
		id := id
		if err := t.note(ctx, tx, actorID, activity.LabelRemoved, nil, &id, nil); err != nil {
			return err
		}
	}

	return nil
}
