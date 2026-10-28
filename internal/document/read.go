package document

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/filter"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/search"

	"github.com/jackc/pgx/v5"
)

// Get returns one live document in a workspace the caller belongs to. Absent, archived
// and not-yours are all ErrNotFound.
func (s *Service) Get(ctx context.Context, accountID, id string) (Document, error) {
	var d Document
	err := d.scan(s.pool.QueryRow(ctx,
		`SELECT `+columns+` FROM document d
		  WHERE d.id = $1::uuid AND d.archived_at IS NULL AND `+fmt.Sprintf(memberOf, "$2"),
		id, accountID,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return Document{}, ErrNotFound
	}
	if err != nil {
		return Document{}, fmt.Errorf("document: get: %w", err)
	}

	return d, nil
}

// Archive soft-deletes a document and removes it from search. It returns the document as
// archived, because the caller needs the new version and a read of an archived document is
// ErrNotFound. ifVersion nil means unconditional.
//
// Errors: ErrNotFound, *Conflict.
func (s *Service) Archive(ctx context.Context, accountID, id string, ifVersion *int) (Document, error) {
	return s.setArchived(ctx, accountID, id, true, ifVersion)
}

// Unarchive restores an archived document and puts it back in search.
//
// Errors: ErrNotFound (including a document that is not archived), *Conflict.
func (s *Service) Unarchive(ctx context.Context, accountID, id string, ifVersion *int) (Document, error) {
	return s.setArchived(ctx, accountID, id, false, ifVersion)
}

// setArchived is the one place archive state changes, so Save stays the only writer of the
// body: nothing here touches it. The index is the exception that is not one - the rows are
// derived from the body, cleared on the way out and rebuilt from the stored body on the way
// back, in the transaction that flips the column.
func (s *Service) setArchived(ctx context.Context, accountID, id string, archive bool, ifVersion *int) (Document, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Document{}, fmt.Errorf("document: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	cur, err := s.lock(ctx, tx, id, "", accountID)
	if err != nil {
		return Document{}, err
	}
	if err := cur.refuse(ifVersion, !archive); err != nil {
		return Document{}, err
	}

	var d Document
	err = d.scan(tx.QueryRow(ctx,
		`WITH d AS (
		     UPDATE document
		        SET archived_at = CASE WHEN $2 THEN now() END,
		            updated_at = now(), version = version + 1
		      WHERE id = $1::uuid
		  RETURNING *)
		 SELECT `+columns+` FROM d`,
		id, archive,
	))
	if err != nil {
		return Document{}, fmt.Errorf("document: set archived: %w", err)
	}

	if err := search.ClearDocument(ctx, tx, d.ID); err != nil {
		return Document{}, err
	}
	if !archive {
		if err := search.IndexDocument(ctx, tx, d.ID, d.Body); err != nil {
			return Document{}, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return Document{}, fmt.Errorf("document: commit: %w", err)
	}

	return d, nil
}

// Fields is the document filter vocabulary. The list endpoint takes its filters as named
// members rather than a filter tree, so these are what they compile to; they are still a
// filter.Schema because the keyset cursor and the sort come from the same machinery as
// every other list.
var Fields = filter.Schema{
	"project":     {Expr: "d.project_id", Kind: filter.UUID},
	"team":        {Expr: "d.team_id", Kind: filter.UUID},
	"type":        {Expr: "d.type", Kind: filter.Text},
	"aspect_type": {Expr: "d.aspect_type_id", Kind: filter.UUID},
	"title":       {Expr: "d.title", Kind: filter.Text},
	"created_at":  {Expr: "d.created_at", Kind: filter.Time},
	"updated_at":  {Expr: "d.updated_at", Kind: filter.Time},
	"version":     {Expr: "d.version", Kind: filter.Number},
}

// Tiebreak completes every sort into a total order, which keyset pagination needs.
const Tiebreak = "d.id"

// DefaultSort is most recently touched first, as tickets are.
var DefaultSort = []filter.Order{{Field: "updated_at", Desc: true}}

// ListParams is one page request. Empty strings are "any".
type ListParams struct {
	ProjectID    string
	TeamID       string
	Type         string
	AspectTypeID string
	// Archived lists archived documents instead of live ones. There is no way to list both:
	// a client that wants the archive asks for it.
	Archived bool
	Limit    int
	Cursor   string
}

// Page is one page and how to ask for the next. No total, for the reason ticket's has none.
type Page struct {
	Documents  []Document
	NextCursor string
	HasMore    bool
}

// group is the request's named filters as the AND of equality conditions.
func (p ListParams) group() filter.Group {
	g := filter.Group{Op: filter.And}
	for _, c := range []struct{ field, value string }{
		{"project", p.ProjectID}, {"team", p.TeamID}, {"type", p.Type}, {"aspect_type", p.AspectTypeID},
	} {
		if c.value != "" {
			g.Conditions = append(g.Conditions, filter.Condition{Field: c.field, Op: filter.Eq, Value: c.value})
		}
	}

	return g
}

// List returns one page of the documents in the caller's workspaces. Scope and the archive
// predicate come first and unconditionally, as in ticket.List: no parameter can widen them.
//
// Errors: *filter.Error, filter.ErrBadCursor, filter.ErrCursorMismatch.
func (s *Service) List(ctx context.Context, accountID string, p ListParams) (Page, error) {
	// $1 is the caller and $2 whether to list the archive; the filter is numbered after.
	plan, err := Fields.Plan(filter.Request{Filter: p.group(), Limit: p.Limit, Cursor: p.Cursor},
		Tiebreak, DefaultSort, 3)
	if err != nil {
		return Page{}, err
	}

	args := append([]any{accountID, p.Archived}, plan.Args...)
	// One more row than asked for: how HasMore is known without a second query.
	args = append(args, plan.Limit+1)

	rows, err := s.pool.Query(ctx,
		`SELECT `+columns+`, `+strings.Join(plan.Keys, ", ")+`
		   FROM document d
		  WHERE (d.archived_at IS NOT NULL) = $2 AND `+fmt.Sprintf(memberOf, "$1")+` AND (`+plan.Where+`) `+
			plan.Order+` LIMIT $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return Page{}, fmt.Errorf("document: list: %w", err)
	}
	defer rows.Close()

	var (
		page Page
		last []*string
	)
	for rows.Next() {
		var (
			d   Document
			raw string
			key = make([]*string, len(plan.Keys))
		)
		dest := []any{&d.ID, &d.WorkspaceID, &d.Title, &d.Type, &d.AspectTypeID, &d.TeamID, &d.ProjectID,
			&raw, &d.Version, &d.CreatedAt, &d.UpdatedAt, &d.ArchivedAt}
		for i := range key {
			dest = append(dest, &key[i])
		}
		if err := rows.Scan(dest...); err != nil {
			return Page{}, fmt.Errorf("document: list scan: %w", err)
		}

		if len(page.Documents) == plan.Limit {
			page.HasMore = true
			break
		}

		if err := d.decode(raw); err != nil {
			return Page{}, err
		}
		page.Documents = append(page.Documents, d)
		last = key
	}
	if err := rows.Err(); err != nil {
		return Page{}, fmt.Errorf("document: list rows: %w", err)
	}

	if page.HasMore {
		page.NextCursor = plan.Cursor(last)
	}

	return page, nil
}

// CountFieldUsage is how many documents of an aspect type hold a value under a field's id,
// archived ones included: an archived document comes back with its values, so a warning
// about removing the field has to count it. The caller has already proved the type and the
// field exist and are the caller's; this only scopes the count to the caller's workspaces.
func (s *Service) CountFieldUsage(ctx context.Context, accountID, aspectTypeID, fieldID string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM document d
		  WHERE d.aspect_type_id = $1::uuid AND d.body ? $2 AND `+fmt.Sprintf(memberOf, "$3"),
		aspectTypeID, fieldID, accountID,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("document: count field usage: %w", err)
	}

	return n, nil
}
