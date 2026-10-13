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
	// $1 is the caller. Everything the body contributes is numbered after it.
	plan, err := Fields.Plan(filter.Request(p), Tiebreak, DefaultSort, 2)
	if err != nil {
		return Page{}, err
	}

	args := append([]any{accountID}, plan.Args...)

	// One more row than asked for, which is how HasMore is known without a second
	// query. The extra row is dropped before returning.
	limit := plan.Limit
	args = append(args, limit+1)

	query := expand(`SELECT `+columns+`, `+strings.Join(plan.Keys, ", ")+`
	                   FROM ticket t
	                  WHERE `+liveOnly+` AND `+callerCanReach+` AND (`+plan.Where+`) `+
		plan.Order+` LIMIT $`+strconv.Itoa(len(args)), "$1")

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
			key = make([]*string, len(plan.Keys))
		)

		dest := t.dest()
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
		page.NextCursor = plan.Cursor(last)
	}

	return page, nil
}
