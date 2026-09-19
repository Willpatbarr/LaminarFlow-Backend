/*
╔═ sort.go ═════════════════════════════════════════════════════════════════════════════
║  filter · the other untrusted string that reaches SQL
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      Order                 struct
║      Schema.CompileOrder   method
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      internal/ticket  →  the list statement's ORDER BY
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package filter

import "strconv"

/*
┏━ Order ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃  one sort key a caller asked for
┣━ attributes ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      Field    string    a name from the same Schema
┃      Desc     bool
┣━ created by ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
┃      JSON decoding of a request or saved_view.config
*/

// Order lives here, beside the filter, because it is the same whitelist problem with
// one difference that makes it worse: a sort key cannot be a placeholder. Postgres
// parameterises values, never identifiers, so `ORDER BY $1` sorts every row by the
// same constant rather than by that column. The name has to be interpolated, which
// means the whitelist is not a convenience here - it is the only thing between a
// caller's string and the query text.
type Order struct {
	Field string `json:"field" required:"true" doc:"A name from this resource's filter vocabulary."`
	Desc  bool   `json:"desc" doc:"Descending. Nulls sort last either way."`
}

/*
┌─ filter ────────────────────────────────────────
│  compiles sort keys into an ORDER BY clause
├─ in ────────────────────────────────────────────
│      orders     []Order    may be empty
│      tiebreak   string     SQL, always appended last
├─ out ───────────────────────────────────────────
│      string     "ORDER BY …", never empty
│      error      *Error, located at sort[i]
├─ example ───────────────────────────────────────
│      updated_at desc  →  "ORDER BY t.updated_at DESC NULLS LAST, t.id"
*/

// CompileOrder appends tiebreak to whatever the caller asked for, and that is the
// point of the function rather than a detail of it.
//
// Cursor pagination needs a total order. Sorting tickets by status alone leaves every
// ticket sharing a status in an order Postgres may choose differently between two
// queries, so a cursor into that sequence can skip a row or repeat one. A unique
// trailing key removes the ambiguity, and putting it here means LAM-59 cannot forget
// it - an empty orders slice still produces a deterministic clause.
//
// NULLS LAST is spelled out on every key. Postgres defaults to NULLS LAST ascending
// and NULLS FIRST descending, so a caller flipping direction would move the null rows
// from one end of the whole result to the other - and a cursor taken before the flip
// would resume in a sequence that no longer contains it.
func (s Schema) CompileOrder(orders []Order, tiebreak string) (string, error) {
	s.check()

	if tiebreak == "" {
		panic("filter: CompileOrder needs a tiebreak, or the order is not total")
	}

	var (
		faults []Fault
		clause = "ORDER BY "
	)

	for i, o := range orders {
		at := "sort[" + strconv.Itoa(i) + "]"

		f, ok := s[o.Field]
		if !ok {
			faults = append(faults, Fault{Location: at + ".field", Message: "unknown field", Value: o.Field})
			continue
		}

		if f.Kind == UUIDSet {
			// A ticket has many labels, so "sorted by label" has no single
			// meaning. Sorting by the array would order by its first element,
			// which is an answer nobody asked for.
			faults = append(faults, Fault{Location: at + ".field", Message: "not sortable", Value: o.Field})
			continue
		}

		direction := " ASC"
		if o.Desc {
			direction = " DESC"
		}

		clause += f.Expr + direction + " NULLS LAST, "
	}

	if len(faults) > 0 {
		return "", &Error{Faults: faults}
	}

	return clause + tiebreak, nil
}
