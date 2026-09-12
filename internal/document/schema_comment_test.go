package document

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The constraints 0017_comment.sql claims.
//
// These live here rather than in internal/migrate beside most tables'
// constraint tests for the reason LAM-23's did: comment targets document, so
// asserting anything about that half means issuing SQL against document, and
// TestNoSQLOutsideThisPackage bars every other package from doing so. The
// whole file is kept together rather than split across two packages, because
// a table's constraints are easier to read in one place than to reassemble.
func TestCommentConstraints(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	ws := defaultWorkspace(t, pool)

	// One scaffold for the whole file: a team, a project, a ticket, a
	// document, an aspect field and an author. Subtests below reuse them,
	// since none of them mutate these rows except the delete tests, which
	// build their own.
	newTicket := func(t *testing.T, label string) string {
		t.Helper()

		var teamID string
		if err := pool.QueryRow(ctx,
			`INSERT INTO team (workspace_id, name) VALUES ($1::uuid, $2)
             RETURNING id::text`, ws, "Team "+label,
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
			`INSERT INTO ticket (project_id, title) VALUES ($1::uuid, 'Build it')
             RETURNING id::text`, projectID,
		).Scan(&ticketID); err != nil {
			t.Fatalf("create ticket: %v", err)
		}

		return ticketID
	}
	newDocument := func(t *testing.T, title string) string {
		t.Helper()

		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO document (workspace_id, title) VALUES ($1::uuid, $2)
             RETURNING id::text`, ws, title,
		).Scan(&id); err != nil {
			t.Fatalf("create document: %v", err)
		}

		return id
	}
	newAuthor := func(t *testing.T, email string) string {
		t.Helper()

		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO account (email, password_hash, display_name)
             VALUES ($1, 'hash', $1) RETURNING id::text`, email,
		).Scan(&id); err != nil {
			t.Fatalf("create account: %v", err)
		}

		return id
	}
	// comment writes one row. Nil pointers become SQL NULL.
	comment := func(ticketID, documentID, fieldID, author *string, start, end *int, status *string) error {
		_, err := pool.Exec(ctx,
			`INSERT INTO comment
                 (ticket_id, document_id, field_id, author_id,
                  region_start, region_end, body, review_status)
             VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, $5, $6, 'looks good', $7)`,
			ticketID, documentID, fieldID, author, start, end, status)
		return err
	}
	u := func(s string) *string { return &s }
	n := func(i int) *int { return &i }
	countOn := func(t *testing.T, column, id string) int {
		t.Helper()

		var c int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM comment WHERE `+column+` = $1::uuid`, id,
		).Scan(&c); err != nil {
			t.Fatalf("count comments: %v", err)
		}

		return c
	}

	t.Run("accepts a comment on a ticket", func(t *testing.T) {
		ticket := newTicket(t, "on-ticket")

		if err := comment(u(ticket), nil, nil, nil, nil, nil, nil); err != nil {
			t.Fatalf("comment on a ticket: %v", err)
		}
	})

	t.Run("accepts a comment on a document", func(t *testing.T) {
		doc := newDocument(t, "On Document")

		if err := comment(nil, u(doc), nil, nil, nil, nil, nil); err != nil {
			t.Fatalf("comment on a document: %v", err)
		}
	})

	// The arc. Both would make the target ambiguous, neither would make the
	// comment belong to nothing - and a polymorphic target_type/target_id
	// could express both mistakes with no constraint able to catch either.
	t.Run("rejects a comment on both a ticket and a document", func(t *testing.T) {
		ticket := newTicket(t, "both-targets")
		doc := newDocument(t, "Both Targets")

		err := comment(u(ticket), u(doc), nil, nil, nil, nil, nil)
		wantPgError(t, err, checkViolation, "a comment targeting a ticket and a document at once")
	})

	t.Run("rejects a comment on nothing", func(t *testing.T) {
		err := comment(nil, nil, nil, nil, nil, nil, nil)
		wantPgError(t, err, checkViolation, "a comment with no target")
	})

	t.Run("rejects a target that does not exist", func(t *testing.T) {
		missing := "00000000-0000-0000-0000-000000000000"

		wantPgError(t, comment(u(missing), nil, nil, nil, nil, nil, nil),
			foreignKeyViolation, "a ticket that does not exist")
		wantPgError(t, comment(nil, u(missing), nil, nil, nil, nil, nil),
			foreignKeyViolation, "a document that does not exist")
	})

	t.Run("rejects an empty body", func(t *testing.T) {
		ticket := newTicket(t, "null-body")

		_, err := pool.Exec(ctx,
			`INSERT INTO comment (ticket_id, body) VALUES ($1::uuid, NULL)`, ticket)
		wantPgError(t, err, notNullViolation, "a null body")
	})

	// review_status is the decorator. Both kinds live in one table, which is
	// what LAM-24 step 3 asks to see.
	t.Run("a plain comment and a review request share the table", func(t *testing.T) {
		doc := newDocument(t, "Shared Table")

		if err := comment(nil, u(doc), nil, nil, nil, nil, nil); err != nil {
			t.Fatalf("plain comment: %v", err)
		}
		if err := comment(nil, u(doc), nil, nil, nil, nil, u("pending")); err != nil {
			t.Fatalf("review request: %v", err)
		}

		var plain, review int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FILTER (WHERE review_status IS NULL),
                    count(*) FILTER (WHERE review_status IS NOT NULL)
               FROM comment WHERE document_id = $1::uuid`, doc,
		).Scan(&plain, &review); err != nil {
			t.Fatalf("read both back: %v", err)
		}
		if plain != 1 || review != 1 {
			t.Errorf("got %d plain and %d review rows, want 1 and 1", plain, review)
		}
	})

	t.Run("accepts every known review status", func(t *testing.T) {
		doc := newDocument(t, "All Statuses")

		for _, status := range []string{"pending", "approved", "changes_requested"} {
			if err := comment(nil, u(doc), nil, nil, nil, nil, u(status)); err != nil {
				t.Fatalf("review status %q: %v", status, err)
			}
		}
	})

	t.Run("rejects an unknown review status", func(t *testing.T) {
		doc := newDocument(t, "Bad Status")

		err := comment(nil, u(doc), nil, nil, nil, nil, u("aproved"))
		wantPgError(t, err, checkViolation, "a misspelled review status")
	})

	t.Run("accepts a field anchor on a document comment", func(t *testing.T) {
		doc := newDocument(t, "Field Anchored")
		field := newAspectTypeField(t, pool, ws, "comment-field")

		if err := comment(nil, u(doc), u(field), nil, nil, nil, nil); err != nil {
			t.Fatalf("field-anchored comment: %v", err)
		}
	})

	// A field anchor on a ticket comment points at a field the ticket does
	// not have and cannot render.
	t.Run("rejects a field anchor on a ticket comment", func(t *testing.T) {
		ticket := newTicket(t, "field-on-ticket")
		field := newAspectTypeField(t, pool, ws, "comment-field-ticket")

		err := comment(u(ticket), nil, u(field), nil, nil, nil, nil)
		wantPgError(t, err, checkViolation, "a field anchor on a ticket comment")
	})

	t.Run("accepts a whole region and no region at all", func(t *testing.T) {
		doc := newDocument(t, "Regions")

		if err := comment(nil, u(doc), nil, nil, n(10), n(25), nil); err != nil {
			t.Fatalf("a whole region: %v", err)
		}
		if err := comment(nil, u(doc), nil, nil, nil, nil, nil); err != nil {
			t.Fatalf("no region: %v", err)
		}
	})

	// A caret - a zero-length insertion point - is a real thing to comment
	// on. Using > instead of >= would look right and silently ban it.
	t.Run("accepts a zero-length caret", func(t *testing.T) {
		doc := newDocument(t, "Caret")

		if err := comment(nil, u(doc), nil, nil, n(7), n(7), nil); err != nil {
			t.Fatalf("a zero-length region: %v", err)
		}
	})

	t.Run("rejects half a region", func(t *testing.T) {
		doc := newDocument(t, "Half Region")

		wantPgError(t, comment(nil, u(doc), nil, nil, n(3), nil, nil),
			checkViolation, "a region with a start and no end")
		wantPgError(t, comment(nil, u(doc), nil, nil, nil, n(3), nil),
			checkViolation, "a region with an end and no start")
	})

	t.Run("rejects an inverted or negative region", func(t *testing.T) {
		doc := newDocument(t, "Bad Region")

		wantPgError(t, comment(nil, u(doc), nil, nil, n(20), n(10), nil),
			checkViolation, "a region ending before it starts")
		wantPgError(t, comment(nil, u(doc), nil, nil, n(-1), n(5), nil),
			checkViolation, "a negative region start")
	})

	t.Run("deleting a ticket deletes its comments", func(t *testing.T) {
		ticket := newTicket(t, "ticket-cascade")

		if err := comment(u(ticket), nil, nil, nil, nil, nil, nil); err != nil {
			t.Fatalf("insert: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM ticket WHERE id = $1::uuid`, ticket); err != nil {
			t.Fatalf("delete ticket: %v", err)
		}

		if got := countOn(t, "ticket_id", ticket); got != 0 {
			t.Errorf("%d comments survived their ticket, want 0", got)
		}
	})

	t.Run("deleting a document deletes its comments", func(t *testing.T) {
		doc := newDocument(t, "Doomed")

		if err := comment(nil, u(doc), nil, nil, nil, nil, nil); err != nil {
			t.Fatalf("insert: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM document WHERE id = $1::uuid`, doc); err != nil {
			t.Fatalf("delete document: %v", err)
		}

		if got := countOn(t, "document_id", doc); got != 0 {
			t.Errorf("%d comments survived their document, want 0", got)
		}
	})

	// Work outlives the people who touched it. The comment stays and loses
	// its attribution; everything rendering comments carries that case.
	t.Run("deleting an author leaves the comment standing", func(t *testing.T) {
		doc := newDocument(t, "Authored")
		author := newAuthor(t, "author-delete@example.com")

		if err := comment(nil, u(doc), nil, u(author), nil, nil, nil); err != nil {
			t.Fatalf("insert: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM account WHERE id = $1::uuid`, author); err != nil {
			t.Fatalf("delete account: %v", err)
		}

		var authorID *string
		if err := pool.QueryRow(ctx,
			`SELECT author_id::text FROM comment WHERE document_id = $1::uuid`, doc,
		).Scan(&authorID); err != nil {
			t.Fatalf("read the comment back: %v", err)
		}
		if authorID != nil {
			t.Errorf("author_id = %q after the account was deleted, want NULL", *authorID)
		}
	})

	// Deleting an aspect field unanchors the conversation rather than
	// deleting it.
	t.Run("deleting a field leaves the comment standing, unanchored", func(t *testing.T) {
		doc := newDocument(t, "Anchored Then Not")
		field := newAspectTypeField(t, pool, ws, "field-delete")

		if err := comment(nil, u(doc), u(field), nil, nil, nil, nil); err != nil {
			t.Fatalf("insert: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`DELETE FROM aspect_type_field WHERE id = $1::uuid`, field,
		); err != nil {
			t.Fatalf("delete field: %v", err)
		}

		var fieldID *string
		if err := pool.QueryRow(ctx,
			`SELECT field_id::text FROM comment WHERE document_id = $1::uuid`, doc,
		).Scan(&fieldID); err != nil {
			t.Fatalf("read the comment back: %v", err)
		}
		if fieldID != nil {
			t.Errorf("field_id = %q after the field was deleted, want NULL", *fieldID)
		}
	})

	t.Run("every foreign key and the review queue lead an index", func(t *testing.T) {
		for _, column := range []string{"ticket_id", "document_id", "field_id", "author_id", "review_status"} {
			if !commentColumnLeadsAnIndex(t, pool, column) {
				t.Errorf("no index on comment leads with %s", column)
			}
		}
	})
}

// commentColumnLeadsAnIndex reports whether any index on comment has column
// as its leading column.
func commentColumnLeadsAnIndex(t *testing.T, pool *pgxpool.Pool, column string) bool {
	t.Helper()

	var leads bool
	if err := pool.QueryRow(context.Background(),
		`SELECT EXISTS (
             SELECT 1
               FROM pg_index i
               JOIN pg_attribute a
                 ON a.attrelid = i.indrelid
                AND a.attnum = i.indkey[0]
              WHERE i.indrelid = 'comment'::regclass
                AND a.attname = $1
         )`, column,
	).Scan(&leads); err != nil {
		t.Fatalf("read indexes: %v", err)
	}

	return leads
}
