/*
╔═ list.go ═════════════════════════════════════════════════════════════════════════════
║  ticket · one page of a filtered, sorted list
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      ListParams        struct
║      Page              struct
║      Service.List      method
║      DefaultSort       var
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      internal/api  →  POST /api/v1/tickets/list
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package ticket

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/filter"
)

// DefaultSort is what a caller gets for asking for nothing: most recently touched
// first, which is what a table view opens to.
//
// 0030 indexes exactly this order under the archive predicate. A different default
// would need a different index, which is why the two are written down together.
var DefaultSort = []filter.Order{{Field: "updated_at", Desc: true}}

/*
┏━ ListParams ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  what one page request asks for
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Filter    filter.Group     empty means everything
┃      Sort      []filter.Order   empty means DefaultSort
┃      Limit     int              0 means DefaultLimit
┃      Cursor    string           "" means the first page
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      internal/api  →  the list request body
*/

type ListParams struct {
	Filter filter.Group
	Sort   []filter.Order
	Limit  int
	Cursor string
}

/*
┏━ Page ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  one page, and how to ask for the next
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Tickets       []Ticket
┃      NextCursor    string      "" on the last page
┃      HasMore       bool
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      List                      one per request
*/

// Page carries no total count, which is LAM-59 decision 5.
//
// A COUNT(*) over the same filter is a second full scan on every page, and the filter
// is the expensive part. Nothing in the product needs an exact total - a table shows
// what it has and whether there is more - so the count is not computed rather than
// computed and ignored.
type Page struct {
	Tickets    []Ticket
	NextCursor string
	HasMore    bool
}

/*
┌─ ticket ────────────────────────────────────────
│  one page of the tickets a caller may see
├─ in ────────────────────────────────────────────
│      ctx          context.Context
│      accountID    string
│      p            ListParams
├─ out ───────────────────────────────────────────
│      Page
│      error        *filter.Error · ErrCursorMismatch
├─ example ───────────────────────────────────────
│      status eq X, limit 50  →  50 tickets + cursor
*/

// List composes three predicates, and the order they are written in is the point.
//
// Scope and the archive predicate come first and unconditionally. They are not filter
// conditions and cannot be expressed as any - Fields has no field for either - so no
// body can widen them, and a filter that compiles to TRUE still sees only the caller's
// live tickets. LAM-59 step 4.
func (s *Service) List(ctx context.Context, accountID string, p ListParams) (Page, error) {
	sort := p.Sort
	if len(sort) == 0 {
		sort = DefaultSort
	}

	limit := p.Limit
	if limit == 0 {
		limit = filter.DefaultLimit
	}
	if limit < 1 || limit > filter.MaxLimit {
		return Page{}, &filter.Error{Faults: []filter.Fault{{
			Location: "body.limit",
			Message:  "must be between 1 and " + strconv.Itoa(filter.MaxLimit),
			Value:    limit,
		}}}
	}

	// A body that omits the filter decodes to a zero Group, whose Op is neither and
	// nor or. Normalising here rather than at the handler keeps the fingerprint the
	// same whether a caller omitted the filter or sent an empty one.
	group := p.Filter
	if group.Op == "" && len(group.Conditions) == 0 && len(group.Groups) == 0 {
		group.Op = filter.And
	}

	// $1 is the caller. Everything the body contributes is numbered after it.
	args := []any{accountID}

	where, filterArgs, err := Fields.Compile(group, len(args)+1)
	if err != nil {
		return Page{}, prefix(err, "body.filter")
	}
	args = append(args, filterArgs...)

	if p.Cursor != "" {
		after, cursorArgs, err := s.resume(p.Cursor, group, sort, len(args)+1)
		if err != nil {
			return Page{}, err
		}
		where += " AND " + after
		args = append(args, cursorArgs...)
	}

	order, err := Fields.CompileOrder(sort, Tiebreak)
	if err != nil {
		return Page{}, prefix(err, "body")
	}

	keys, err := Fields.SortKeys(sort, Tiebreak)
	if err != nil {
		return Page{}, prefix(err, "body")
	}

	// One more row than asked for, which is how HasMore is known without a second
	// query. The extra row is dropped before returning.
	args = append(args, limit+1)

	query := expand(`SELECT `+columns+`, `+strings.Join(keys, ", ")+`
	                   FROM ticket t
	                  WHERE `+liveOnly+` AND `+callerCanReach+` AND (`+where+`) `+
		order+` LIMIT $`+strconv.Itoa(len(args)), "$1")

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return Page{}, fmt.Errorf("ticket: list: %w", err)
	}
	defer rows.Close()

	var (
		page Page
		last []*string
	)

	for rows.Next() {
		var (
			t   Ticket
			key = make([]*string, len(keys))
		)

		dest := []any{&t.ID, &t.ProjectID, &t.Title, &t.Description,
			&t.StatusID, &t.AssigneeID, &t.CreatedAt, &t.UpdatedAt}
		for i := range key {
			dest = append(dest, &key[i])
		}

		if err := rows.Scan(dest...); err != nil {
			return Page{}, fmt.Errorf("ticket: list scan: %w", err)
		}

		if len(page.Tickets) == limit {
			// The sentinel row. Its existence is the whole answer; its
			// contents are not returned.
			page.HasMore = true
			break
		}

		page.Tickets = append(page.Tickets, t)
		last = key
	}
	if err := rows.Err(); err != nil {
		return Page{}, fmt.Errorf("ticket: list rows: %w", err)
	}

	if page.HasMore {
		page.NextCursor = filter.Cursor{
			Query: filter.Fingerprint(group, sort),
			Key:   last,
		}.Encode()
	}

	return page, nil
}

// resume turns a cursor token into the predicate that continues after it.
//
// The fingerprint is checked before the key is used. A cursor minted under one filter
// and replayed under another describes a position in a sequence that no longer exists,
// and resuming from it returns rows belonging to neither - LAM-59 decision 4.
func (s *Service) resume(token string, group filter.Group, sort []filter.Order, firstArg int) (string, []any, error) {
	c, err := filter.Decode(token)
	if err != nil {
		return "", nil, err
	}

	if c.Query != filter.Fingerprint(group, sort) {
		return "", nil, filter.ErrCursorMismatch
	}

	return Fields.CompileAfter(sort, Tiebreak, c, firstArg)
}

// prefix qualifies a filter package's fault locations against the request body, which
// is the only place that knows where in the body the filter sat.
func prefix(err error, at string) error {
	var e *filter.Error
	if !errors.As(err, &e) {
		return err
	}

	out := make([]filter.Fault, len(e.Faults))
	for i, f := range e.Faults {
		out[i] = f
		if f.Location == "" {
			out[i].Location = at
			continue
		}
		out[i].Location = at + "." + f.Location
	}

	return &filter.Error{Faults: out}
}
