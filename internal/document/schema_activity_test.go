package document

import (
	"context"
	"testing"
)

// The constraints 0043_activity.sql claims. Here for the reason schema_comment_test.go is:
// the table points at document, and asserting against that half means SQL on document,
// which TestNoSQLOutsideThisPackage bars every other package from.
func TestActivityConstraints(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	ws := defaultWorkspace(t, pool)

	var teamID, projectID, ticketID, docID, accountID string
	for _, step := range []struct {
		dest  *string
		query string
		args  []any
	}{
		{&teamID, `INSERT INTO team (workspace_id, name) VALUES ($1::uuid, 'Act') RETURNING id::text`, []any{ws}},
		{&projectID, `INSERT INTO project (team_id, name) VALUES ($1::uuid, 'Act') RETURNING id::text`, []any{&teamID}},
		{&ticketID, `INSERT INTO ticket (project_id, title) VALUES ($1::uuid, 'T') RETURNING id::text`, []any{&projectID}},
		{&docID, `INSERT INTO document (workspace_id, title) VALUES ($1::uuid, 'D') RETURNING id::text`, []any{ws}},
		{&accountID, `INSERT INTO account (email, password_hash, display_name) VALUES ('act@example.com', 'h', 'act') RETURNING id::text`, nil},
	} {
		args := make([]any, len(step.args))
		for i, a := range step.args {
			if p, ok := a.(*string); ok {
				args[i] = *p
			} else {
				args[i] = a
			}
		}
		if err := pool.QueryRow(ctx, step.query, args...).Scan(step.dest); err != nil {
			t.Fatalf("scaffold: %v", err)
		}
	}

	insert := func(ticket, doc *string, kind string, field *string) error {
		_, err := pool.Exec(ctx,
			`INSERT INTO activity (ticket_id, document_id, actor_id, kind, field) VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5)`,
			ticket, doc, accountID, kind, field)

		return err
	}
	u := func(s string) *string { return &s }

	for name, c := range map[string]struct {
		ticket, doc *string
		kind        string
		field       *string
		ok          bool
	}{
		"a ticket entry":                   {u(ticketID), nil, "status_changed", nil, true},
		"a document entry":                 {nil, u(docID), "archived", nil, true},
		"a field change names its field":   {u(ticketID), nil, "field_changed", u("title"), true},
		"a field change without a field":   {u(ticketID), nil, "field_changed", nil, false},
		"a field on another kind":          {u(ticketID), nil, "archived", u("title"), false},
		"no entity":                        {nil, nil, "created", nil, false},
		"both entities":                    {u(ticketID), u(docID), "created", nil, false},
		"an unknown kind":                  {u(ticketID), nil, "renamed", nil, false},
		"a ticket-only kind on a document": {nil, u(docID), "status_changed", nil, false},
		"a document type change":           {nil, u(docID), "aspect_type_changed", nil, true},
	} {
		if err := insert(c.ticket, c.doc, c.kind, c.field); (err == nil) != c.ok {
			t.Errorf("%s: err = %v, want ok=%v", name, err, c.ok)
		}
	}

	count := func(column, id string) int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM activity WHERE `+column+` = $1::uuid`, id).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}

		return n
	}

	// Deleting the account keeps the history and loses the attribution.
	if _, err := pool.Exec(ctx, `DELETE FROM account WHERE id = $1::uuid`, accountID); err != nil {
		t.Fatalf("delete account: %v", err)
	}
	var withActor int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM activity WHERE actor_id IS NOT NULL`).Scan(&withActor); err != nil || withActor != 0 {
		t.Errorf("after the account went: %d rows still name it (%v), want 0", withActor, err)
	}
	if n := count("ticket_id", ticketID); n != 2 {
		t.Errorf("ticket rows after the account went = %d, want 2", n)
	}

	// Deleting the entity takes its history with it.
	if _, err := pool.Exec(ctx, `DELETE FROM document WHERE id = $1::uuid`, docID); err != nil {
		t.Fatalf("delete document: %v", err)
	}
	if n := count("document_id", docID); n != 0 {
		t.Errorf("document rows after the document went = %d, want 0", n)
	}
}
