/*
╔═ history.go ══════════════════════════════════════════════════════════════════════════
║  document · what a write records in the activity log (LAM-89)
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      Document.note       method
║      Document.diff       method
║      fieldKeys           func
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      insert · update · setArchived  →  inside their transactions
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package document

import (
	"context"
	"sort"
	"strings"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/activity"
)

// note records one entry about this document and keeps it on the Document so the API can
// announce it after the commit. actorID may be empty: a write made for a workspace, with no
// account, has no actor.
func (d *Document) note(ctx context.Context, tx activity.Tx, actorID, kind string, field, before, after *string) error {
	e, err := activity.Record(ctx, tx, activity.Entry{
		DocumentID: &d.ID, ActorID: nilIfEmpty(actorID),
		Kind: kind, Field: field, Before: before, After: after,
	})
	if err != nil {
		return err
	}
	d.Activity = append(d.Activity, e)

	return nil
}

func samePtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}

	return *a == *b
}

// fieldKeys is the sorted, comma-joined set of keys a body holds: for an aspect document
// the ids of the fields that have a value.
func fieldKeys(body map[string]any) string {
	keys := make([]string, 0, len(body))
	for k := range body {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	return strings.Join(keys, ",")
}

// diff records what an update changed, coarsely: the title; the type (a normal document
// made an aspect one, or the other way, or moved to another aspect type); and, when the
// type is the same, which fields have a value. The text of the body is not recorded - a
// body-only save writes no entry - and neither is the new text of a field, only that the
// set of fields changed.
func (d *Document) diff(ctx context.Context, tx activity.Tx, actorID string, before Document) error {
	if before.Title != d.Title {
		f, b, a := "title", activity.Clip(before.Title), activity.Clip(d.Title)
		if err := d.note(ctx, tx, actorID, activity.FieldChanged, &f, &b, &a); err != nil {
			return err
		}
	}

	if !samePtr(before.ProjectID, d.ProjectID) {
		f := "project"
		if err := d.note(ctx, tx, actorID, activity.FieldChanged, &f, before.ProjectID, d.ProjectID); err != nil {
			return err
		}
	}

	if before.Type != d.Type || !samePtr(before.AspectTypeID, d.AspectTypeID) {
		return d.note(ctx, tx, actorID, activity.AspectTypeChanged, nil, before.AspectTypeID, d.AspectTypeID)
	}

	if was, now := fieldKeys(before.Body), fieldKeys(d.Body); was != now {
		f, b, a := "fields", activity.Clip(was), activity.Clip(now)

		return d.note(ctx, tx, actorID, activity.FieldChanged, &f, &b, &a)
	}

	return nil
}
