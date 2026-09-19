package search_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/document"

	"github.com/jackc/pgx/v5/pgconn"
)

// Ported with the file when search_index's ownership moved to internal/search under
// LAM-45. internal/document keeps its own copy for the tables it still owns; two
// four-line helpers is cheaper than a shared test package that exists only for them.
const (
	checkViolation      = "23514"
	foreignKeyViolation = "23503"
	notNullViolation    = "23502"
	uniqueViolation     = "23505"
)

// wantPgError asserts err is the Postgres error named by code. Asserting on the code
// rather than the message is what keeps these tests readable when Postgres rewords.
func wantPgError(t *testing.T, err error, code, what string) {
	t.Helper()

	if err == nil {
		t.Errorf("%s was accepted, want it rejected with %s", what, code)
		return
	}

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Errorf("%s failed with %v, want a Postgres error %s", what, err, code)
		return
	}
	if pgErr.Code != code {
		t.Errorf("%s failed with %s (%s), want %s", what, pgErr.Code, pgErr.Message, code)
	}
}

// The constraints 0019_search_index.sql claims.
//
// search_index is derived and disposable, which makes it tempting to leave
// unconstrained. The opposite follows: nothing downstream validates a search
// row, so the only thing standing between a bad write and a search result
// that 404s when clicked is the table itself.
func TestSearchIndexConstraints(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	ws := defaultWorkspace(t, pool)

	// Through the owning service rather than a raw INSERT. internal/search reads
	// document and never writes it, and this file lives here now - so a fixture
	// insert would be the guard's job to catch, and was.
	docs := document.NewService(pool)
	newDoc := func(t *testing.T, title string) string {
		t.Helper()

		// An explicit empty body, not nil: Save marshals nil to JSON null, which
		// document_body_is_object rejects. The raw INSERT this replaced leaned on
		// the column default instead.
		id, err := docs.Save(ctx, document.SaveParams{
			WorkspaceID: ws,
			Title:       title,
			Body:        map[string]any{},
		})
		if err != nil {
			t.Fatalf("create document: %v", err)
		}

		return id
	}
	newTicketRow := func(t *testing.T, label string) string {
		t.Helper()

		var teamID string
		if err := pool.QueryRow(ctx,
			`INSERT INTO team (workspace_id, name) VALUES ($1::uuid, $2)
             RETURNING id::text`, ws, "Team si-"+label,
		).Scan(&teamID); err != nil {
			t.Fatalf("create team: %v", err)
		}

		var projectID string
		if err := pool.QueryRow(ctx,
			`INSERT INTO project (team_id, name) VALUES ($1::uuid, 'Apollo')
             RETURNING id::text`, teamID,
		).Scan(&projectID); err != nil {
			t.Fatalf("create project: %v", err)
		}

		var ticketID string
		if err := pool.QueryRow(ctx,
			`INSERT INTO ticket (project_id, title) VALUES ($1::uuid, 'Searchable')
             RETURNING id::text`, projectID,
		).Scan(&ticketID); err != nil {
			t.Fatalf("create ticket: %v", err)
		}

		return ticketID
	}
	// index writes one row directly. The service write path is exercised
	// separately; this is for reaching constraints the service will not.
	index := func(ticket, document, field, comment *string, content string) error {
		_, err := pool.Exec(ctx,
			`INSERT INTO search_index
                 (ticket_id, document_id, field_id, comment_id, content, workspace_id)
             VALUES ($1::uuid, $2::uuid, $3, $4::uuid, $5, $6::uuid)`,
			ticket, document, field, comment, content, ws)
		return err
	}
	u := func(s string) *string { return &s }

	t.Run("accepts a document field row", func(t *testing.T) {
		doc := newDoc(t, "Doc Row")

		if err := index(nil, u(doc), u("f_note"), nil, "hello"); err != nil {
			t.Fatalf("index a document field: %v", err)
		}
	})

	t.Run("accepts a ticket row and a comment row", func(t *testing.T) {
		ticket := newTicketRow(t, "ticket-row")

		if err := index(u(ticket), nil, nil, nil, "ticket content"); err != nil {
			t.Fatalf("index a ticket: %v", err)
		}

		doc := newDoc(t, "Commented")

		var commentID string
		if err := pool.QueryRow(ctx,
			`INSERT INTO comment (document_id, body) VALUES ($1::uuid, 'a remark')
             RETURNING id::text`, doc,
		).Scan(&commentID); err != nil {
			t.Fatalf("create comment: %v", err)
		}
		if err := index(nil, nil, nil, u(commentID), "a remark"); err != nil {
			t.Fatalf("index a comment: %v", err)
		}
	})

	// The arc, which a source_type/source_id pair could express neither half
	// of and no constraint could catch.
	t.Run("rejects a row with two sources", func(t *testing.T) {
		doc := newDoc(t, "Two Sources")
		ticket := newTicketRow(t, "two-sources")

		err := index(u(ticket), u(doc), u("f_note"), nil, "confused")
		wantPgError(t, err, checkViolation, "a search row with two sources")
	})

	t.Run("rejects a row with no source", func(t *testing.T) {
		err := index(nil, nil, nil, nil, "orphan")
		wantPgError(t, err, checkViolation, "a search row with no source")
	})

	// The collapse of LAM-26's "document" and "aspect_field" source types
	// into one: a document row is always a field within that document.
	t.Run("rejects a document row with no field", func(t *testing.T) {
		doc := newDoc(t, "No Field")

		err := index(nil, u(doc), nil, nil, "unfielded")
		wantPgError(t, err, checkViolation, "a document row carrying no field")
	})

	t.Run("rejects a field with no document", func(t *testing.T) {
		ticket := newTicketRow(t, "field-no-doc")

		err := index(u(ticket), nil, u("f_note"), nil, "field on a ticket")
		wantPgError(t, err, checkViolation, "a field_id with no document")
	})

	// What the old composite primary key guaranteed, kept as a unique
	// constraint so delete-then-insert cannot double up.
	t.Run("rejects the same document field twice", func(t *testing.T) {
		doc := newDoc(t, "Duplicate Field")

		if err := index(nil, u(doc), u("f_note"), nil, "first"); err != nil {
			t.Fatalf("first insert: %v", err)
		}

		err := index(nil, u(doc), u("f_note"), nil, "second")
		wantPgError(t, err, uniqueViolation, "the same document field indexed twice")
	})

	t.Run("rejects a row with no workspace", func(t *testing.T) {
		doc := newDoc(t, "No Workspace")

		_, err := pool.Exec(ctx,
			`INSERT INTO search_index (document_id, field_id, content, workspace_id)
             VALUES ($1::uuid, 'f_note', 'unscoped', NULL)`, doc)
		wantPgError(t, err, notNullViolation, "a search row with no workspace")
	})

	t.Run("rejects a source that does not exist", func(t *testing.T) {
		missing := "00000000-0000-0000-0000-000000000000"

		wantPgError(t, index(u(missing), nil, nil, nil, "ghost ticket"),
			foreignKeyViolation, "a ticket that does not exist")
		wantPgError(t, index(nil, nil, nil, u(missing), "ghost comment"),
			foreignKeyViolation, "a comment that does not exist")
	})

	t.Run("deleting a ticket deletes its search rows", func(t *testing.T) {
		ticket := newTicketRow(t, "ticket-cascade")

		if err := index(u(ticket), nil, nil, nil, "doomed"); err != nil {
			t.Fatalf("index: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM ticket WHERE id = $1::uuid`, ticket); err != nil {
			t.Fatalf("delete ticket: %v", err)
		}

		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM search_index WHERE ticket_id = $1::uuid`, ticket,
		).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 0 {
			t.Errorf("%d search rows survived their ticket, want 0", n)
		}
	})

	// The generated column. It exists so ranking and highlighting can select
	// the vector without recomputing it, and so it cannot drift from content.
	t.Run("the search vector is generated from content and tracks it", func(t *testing.T) {
		doc := newDoc(t, "Vector")

		if err := index(nil, u(doc), u("f_note"), nil, "the quick brown foxes are jumping"); err != nil {
			t.Fatalf("index: %v", err)
		}

		var matched bool
		if err := pool.QueryRow(ctx,
			`SELECT search @@ plainto_tsquery('english', 'jump fox')
               FROM search_index WHERE document_id = $1::uuid`, doc,
		).Scan(&matched); err != nil {
			t.Fatalf("query the vector: %v", err)
		}
		if !matched {
			t.Error("the generated vector does not stem - 'jump fox' should match 'jumping foxes'")
		}

		// Rewriting content must move the vector with it.
		if _, err := pool.Exec(ctx,
			`UPDATE search_index SET content = 'entirely different' WHERE document_id = $1::uuid`, doc,
		); err != nil {
			t.Fatalf("rewrite content: %v", err)
		}

		if err := pool.QueryRow(ctx,
			`SELECT search @@ plainto_tsquery('english', 'fox')
               FROM search_index WHERE document_id = $1::uuid`, doc,
		).Scan(&matched); err != nil {
			t.Fatalf("re-query the vector: %v", err)
		}
		if matched {
			t.Error("the vector still matches the old content, so it is not generated")
		}
	})

	t.Run("the search vector has a GIN index", func(t *testing.T) {
		var gin bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (
                 SELECT 1
                   FROM pg_index i
                   JOIN pg_class c  ON c.oid = i.indexrelid
                   JOIN pg_am   am  ON am.oid = c.relam
                   JOIN pg_attribute a
                     ON a.attrelid = i.indrelid AND a.attnum = i.indkey[0]
                  WHERE i.indrelid = 'search_index'::regclass
                    AND am.amname = 'gin'
                    AND a.attname = 'search'
             )`,
		).Scan(&gin); err != nil {
			t.Fatalf("read indexes: %v", err)
		}
		if !gin {
			t.Error("no GIN index on search_index.search, so every full-text query is a sequential scan")
		}
	})

	t.Run("every scope and source column leads an index", func(t *testing.T) {
		for _, column := range []string{"workspace_id", "project_id", "team_id", "ticket_id", "comment_id", "document_id"} {
			var leads bool
			if err := pool.QueryRow(ctx,
				`SELECT EXISTS (
                     SELECT 1
                       FROM pg_index i
                       JOIN pg_attribute a
                         ON a.attrelid = i.indrelid AND a.attnum = i.indkey[0]
                      WHERE i.indrelid = 'search_index'::regclass
                        AND a.attname = $1
                 )`, column,
			).Scan(&leads); err != nil {
				t.Fatalf("read indexes: %v", err)
			}
			if !leads {
				t.Errorf("no index on search_index leads with %s", column)
			}
		}
	})
}

// The service write path against the new shape. IndexDocument denormalises scope and
// title from the document, and getting that wrong would fail no constraint - it would
// quietly produce search rows scoped to the wrong workspace.
func TestIndexedRowsCarryTheDocumentsScope(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	ws := defaultWorkspace(t, pool)
	svc := document.NewService(pool)

	id, err := svc.Save(ctx, document.SaveParams{
		WorkspaceID: ws,
		Title:       "Scoped Document",
		Body:        map[string]any{"f_note": "indexed text"},
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	var workspaceID, title string
	var project, team *string
	if err := pool.QueryRow(ctx,
		`SELECT workspace_id::text, title_or_preview, project_id::text, team_id::text
           FROM search_index WHERE document_id = $1::uuid`, id,
	).Scan(&workspaceID, &title, &project, &team); err != nil {
		t.Fatalf("read the search row: %v", err)
	}

	if workspaceID != ws {
		t.Errorf("search row workspace = %q, want the document's %q", workspaceID, ws)
	}
	if title != "Scoped Document" {
		t.Errorf("title_or_preview = %q, want the document's title", title)
	}
	if project != nil || team != nil {
		t.Errorf("workspace-level document produced project=%v team=%v, want both NULL", project, team)
	}
}
