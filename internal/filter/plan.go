/*
╔═ plan.go ═════════════════════════════════════════════════════════════════════════════
║  filter · the half of a list every resource repeats
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      Request       struct
║      Plan          struct
║      Schema.Plan   method
║      Plan.Cursor   method
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      internal/ticket   →  POST /api/v1/tickets/list
║      internal/team     →  POST /api/v1/teams/list
║      internal/project  →  POST /api/v1/projects/list
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package filter

import (
	"errors"
	"strconv"
)

/*
┏━ Request ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  what one page request asks for, whatever the resource
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Filter    Group      empty means everything
┃      Sort      []Order    empty means the resource's default
┃      Limit     int        0 means DefaultLimit
┃      Cursor    string     "" means the first page
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      each resource's ListParams        one per request
*/

type Request struct {
	Filter Group
	Sort   []Order
	Limit  int
	Cursor string
}

/*
┏━ Plan ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  the SQL fragments a list statement is assembled from
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Where     string    filter, then the cursor predicate
┃      Order     string    "ORDER BY …", total
┃      Keys      []string  extra SELECT columns, all ::text
┃      Args      []any     for Where, from the first free placeholder
┃      Limit     int       rows wanted; the statement asks for one more
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Schema.Plan                       one per request
*/

type Plan struct {
	Where string
	Order string
	Keys  []string
	Args  []any
	Limit int

	group Group
	sort  []Order
}

/*
┌─ filter ────────────────────────────────────────
│  validates a page request and compiles its parts
├─ in ────────────────────────────────────────────
│      r             Request
│      tiebreak      string     SQL, unique, last sort key
│      defaultSort   []Order    used when r.Sort is empty
│      firstArg      int        first placeholder number free
├─ out ───────────────────────────────────────────
│      Plan
│      error         *Error · ErrBadCursor · ErrCursorMismatch
├─ example ───────────────────────────────────────
│      limit 0, firstArg 2  →  Plan{Limit: 50}, args from $2
*/

// Plan is the part of a list that does not differ per resource: the page-size bound,
// the empty-filter normalisation, the cursor check and the fault locations. A resource
// keeps its own scoping predicate, its own columns and its own row scan, and hands this
// its Schema - which is how a list stays a registration and not a copy (conventions §4).
//
// The order is the order faults are discovered in, and the order ticket.List has always
// run them: filter, then cursor, then sort. A request wrong in two places reports the
// same first fault it always did.
//
// Faults are located against the request body ("body.filter…", "body.sort[0]…",
// "body.limit"), because every list body has that shape and this is the one place that
// knows where in it the filter sat.
func (s Schema) Plan(r Request, tiebreak string, defaultSort []Order, firstArg int) (Plan, error) {
	sort := r.Sort
	if len(sort) == 0 {
		sort = defaultSort
	}

	limit := r.Limit
	if limit == 0 {
		limit = DefaultLimit
	}
	if limit < 1 || limit > MaxLimit {
		return Plan{}, &Error{Faults: []Fault{{
			Location: "body.limit",
			Message:  "must be between 1 and " + strconv.Itoa(MaxLimit),
			Value:    limit,
		}}}
	}

	// A body that omits the filter decodes to a zero Group, whose Op is neither and
	// nor or. Normalising here keeps the fingerprint the same whether a caller
	// omitted the filter or sent an empty one.
	group := r.Filter
	if group.Op == "" && len(group.Conditions) == 0 && len(group.Groups) == 0 {
		group.Op = And
	}

	where, args, err := s.Compile(group, firstArg)
	if err != nil {
		return Plan{}, locate(err, "body.filter")
	}

	if r.Cursor != "" {
		c, err := Decode(r.Cursor)
		if err != nil {
			return Plan{}, err
		}

		// The fingerprint is checked before the key is used. A cursor minted under
		// one filter and replayed under another describes a position in a sequence
		// that no longer exists, and resuming from it returns rows belonging to
		// neither - LAM-59 decision 4.
		if c.Query != Fingerprint(group, sort) {
			return Plan{}, ErrCursorMismatch
		}

		after, afterArgs, err := s.CompileAfter(sort, tiebreak, c, firstArg+len(args))
		if err != nil {
			return Plan{}, err
		}

		where += " AND " + after
		args = append(args, afterArgs...)
	}

	order, err := s.CompileOrder(sort, tiebreak)
	if err != nil {
		return Plan{}, locate(err, "body")
	}

	keys, err := s.SortKeys(sort, tiebreak)
	if err != nil {
		return Plan{}, locate(err, "body")
	}

	return Plan{
		Where: where, Order: order, Keys: keys, Args: args, Limit: limit,
		group: group, sort: sort,
	}, nil
}

/*
┌─ filter ────────────────────────────────────────
│  the cursor for the page after the one just read
├─ in ────────────────────────────────────────────
│      last    []*string    Keys of the final row returned
├─ out ───────────────────────────────────────────
│      string               opaque token
*/

// Cursor is only asked for when the sentinel row existed, so the caller passes the keys
// of the last row it kept, not of the sentinel.
func (p Plan) Cursor(last []*string) string {
	return Cursor{Query: Fingerprint(p.group, p.sort), Key: last}.Encode()
}

// locate qualifies a fault's location against the request body, which is the only place
// that knows where in the body the filter sat.
func locate(err error, at string) error {
	var e *Error
	if !errors.As(err, &e) {
		return err
	}

	out := make([]Fault, len(e.Faults))
	for i, f := range e.Faults {
		out[i] = f
		if f.Location == "" {
			out[i].Location = at
			continue
		}
		out[i].Location = at + "." + f.Location
	}

	return &Error{Faults: out}
}
