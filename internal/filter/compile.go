/*
╔═ compile.go ══════════════════════════════════════════════════════════════════════════
║  filter · group in, WHERE fragment and arguments out
╠═ declares ════════════════════════════════════════════════════════════════════════════
║      Schema.Compile    method
╠═ reached from ════════════════════════════════════════════════════════════════════════
║      internal/ticket  →  the list statement's WHERE
╚═══════════════════════════════════════════════════════════════════════════════════════
*/

package filter

import (
	"strconv"
	"strings"
)

// casts is the SQL type each kind's placeholder is cast to.
//
// Explicit rather than left to inference. pgx sends a Go string in text format with no
// type OID, and `t.status_id = $1` does infer uuid from context - but `$1 = ANY(...)`
// and `$1 || ...` do not always, and the failure is a runtime type error rather than a
// compile one. Casting every placeholder makes the inference irrelevant.
var casts = map[Kind]string{
	Text:    "",
	UUID:    "::uuid",
	Time:    "::timestamptz",
	Number:  "::numeric",
	Bool:    "::boolean",
	UUIDSet: "::uuid",
}

// arrayCasts is the same table for `in`, whose placeholder is a list. Text needs an
// explicit ::text[] where the scalar case needed no cast at all: a bare $1[] is not
// SQL, so the two tables cannot be one with "[]" appended.
var arrayCasts = map[Kind]string{
	Text:    "::text[]",
	UUID:    "::uuid[]",
	Number:  "::numeric[]",
	UUIDSet: "::uuid[]",
}

// comparisons is the scalar operator set that is a bare infix comparison.
var comparisons = map[Operator]string{
	Gt:  ">",
	Gte: ">=",
	Lt:  "<",
	Lte: "<=",
}

/*
┌─ filter ────────────────────────────────────────
│  compiles a group into SQL and its arguments
├─ in ────────────────────────────────────────────
│      g          Group     the caller's filter
│      firstArg   int       first free placeholder number
├─ out ───────────────────────────────────────────
│      string     a WHERE fragment, already parenthesised
│      []any      arguments, in placeholder order
│      error      *Error, one fault per problem
├─ example ───────────────────────────────────────
│      and{status eq X}  →  "t.status_id = $3::uuid"
*/

// Compile validates against the schema and then emits SQL. Both halves are here rather
// than in a separate Validate because they walk the same tree and would otherwise
// disagree: a validator that permits what the compiler cannot emit is a 500.
//
// firstArg lets the fragment drop into a statement that already has parameters - the
// ticket list has $1 for the caller's account before any filter argument. Returned
// arguments are in placeholder order, so a caller appends them to its own.
//
// An empty root group compiles to TRUE rather than failing. "No filter" is the normal
// case for a list endpoint, and a caller should not have to branch on whether to AND
// the fragment in. An empty *nested* group is a fault: nothing sends one deliberately,
// and swallowing it silently widens the result set, which is the wrong direction to
// fail anywhere near a permission boundary.
func (s Schema) Compile(g Group, firstArg int) (string, []any, error) {
	s.check()

	c := &compiler{schema: s, next: firstArg}
	sql := c.group(g, "", 1)

	if len(c.faults) > 0 {
		return "", nil, &Error{Faults: c.faults}
	}

	return sql, c.args, nil
}

type compiler struct {
	schema Schema
	args   []any
	next   int
	faults []Fault
	seen   int
}

func (c *compiler) fault(location, message string, value any) {
	c.faults = append(c.faults, Fault{Location: location, Message: message, Value: value})
}

// arg records one value and returns the placeholder standing for it. Every value in
// this package goes through here. Nothing is ever formatted into SQL text, including
// the values that "cannot" contain a quote - a uuid that is validated today is a
// uuid-shaped string that some later caller supplies unvalidated.
func (c *compiler) arg(v any) string {
	c.args = append(c.args, v)
	n := c.next
	c.next++

	return "$" + strconv.Itoa(n)
}

// path builds a fault location. The root group's location is empty, so its first
// condition is "conditions[0]" rather than ".conditions[0]".
func path(base, segment string) string {
	if base == "" {
		return segment
	}

	return base + "." + segment
}

func (c *compiler) group(g Group, loc string, depth int) string {
	if depth > MaxDepth {
		c.fault(loc, "filter nests deeper than "+strconv.Itoa(MaxDepth)+" levels", nil)
		return "TRUE"
	}

	var join string
	switch g.Op {
	case And:
		join = " AND "
	case Or:
		join = " OR "
	default:
		c.fault(path(loc, "op"), "expected and or or", string(g.Op))
		join = " AND "
	}

	terms := make([]string, 0, len(g.Conditions)+len(g.Groups))

	for i, cond := range g.Conditions {
		at := path(loc, "conditions["+strconv.Itoa(i)+"]")

		c.seen++
		if c.seen > MaxConditions {
			// Once, at the condition that crosses the line. Reporting every
			// condition past it would be a hundred faults saying one thing.
			if c.seen == MaxConditions+1 {
				c.fault(at, "filter holds more than "+strconv.Itoa(MaxConditions)+" conditions", nil)
			}
			continue
		}

		if term := c.condition(cond, at); term != "" {
			terms = append(terms, term)
		}
	}

	for i, sub := range g.Groups {
		at := path(loc, "groups["+strconv.Itoa(i)+"]")

		if len(sub.Conditions) == 0 && len(sub.Groups) == 0 {
			c.fault(at, "empty group", nil)
			continue
		}

		terms = append(terms, c.group(sub, at, depth+1))
	}

	switch len(terms) {
	case 0:
		// Only reachable at the root, or under a group whose every member
		// faulted - in which case an Error is already being returned and this
		// string is discarded.
		return "TRUE"
	case 1:
		// No parentheses around a single term. They would be harmless, and their
		// absence is what makes a compiled fragment readable in a test.
		return terms[0]
	}

	return "(" + strings.Join(terms, join) + ")"
}

func (c *compiler) condition(cond Condition, at string) string {
	f, ok := c.schema[cond.Field]
	if !ok {
		// Rejecting, never dropping. A dropped condition returns more rows than
		// were asked for, and a filter is often the last thing between a caller
		// and a row they should not see.
		c.fault(path(at, "field"), "unknown field", cond.Field)
		return ""
	}

	if !legal[f.Kind][cond.Op] {
		c.fault(path(at, "op"), "operator not valid for a "+string(f.Kind)+" field", string(cond.Op))
		return ""
	}

	if cond.Op == IsNull || cond.Op == IsNotNull {
		return c.nullTest(f, cond.Op)
	}

	if cond.Value == nil {
		c.fault(path(at, "value"), "required for operator "+string(cond.Op), nil)
		return ""
	}

	if cond.Op == In {
		return c.anyOf(f, cond.Value, at)
	}

	v, problem := coerce(f.Kind, cond.Value)
	if problem != "" {
		c.fault(path(at, "value"), problem, cond.Value)
		return ""
	}

	if f.Kind == UUIDSet {
		return c.setTest(f, cond.Op, c.arg(v))
	}

	return c.scalarTest(f, cond.Op, c.arg(v))
}

// nullTest handles the two operators that take no value.
//
// On a set field the words change meaning: a join table cannot hold a null, so
// is_null asks whether the set is empty. "Tickets with no label" is the query, and it
// is the reason the operator is legal on a set at all.
func (c *compiler) nullTest(f Field, op Operator) string {
	if f.Kind == UUIDSet {
		if op == IsNull {
			return "cardinality(" + f.Expr + ") = 0"
		}
		return "cardinality(" + f.Expr + ") > 0"
	}

	if op == IsNull {
		return f.Expr + " IS NULL"
	}

	return f.Expr + " IS NOT NULL"
}

// scalarTest emits a comparison against a column.
func (c *compiler) scalarTest(f Field, op Operator, ph string) string {
	cast := casts[f.Kind]

	switch op {
	case Eq:
		return f.Expr + " = " + ph + cast

	case Neq:
		// IS DISTINCT FROM, not <>. On a nullable column `status <> $1` is NULL
		// where status is null, so the row fails the filter - and "status is not
		// Done" silently hides every unstatused ticket. That is a wrong answer
		// with no error attached, which is the worst kind this package can give.
		return "(" + f.Expr + " IS DISTINCT FROM " + ph + cast + ")"

	case Contains:
		// Escaped before it is wrapped, or a value containing % or _ becomes a
		// wildcard and matches rows it should not. ESCAPE is spelled out rather
		// than relying on LIKE's default backslash, which a server setting can
		// change. Text is the only kind this operator is legal on.
		escaped := "replace(replace(replace(" + ph + ", '\\', '\\\\'), '%', '\\%'), '_', '\\_')"
		return f.Expr + " ILIKE '%' || " + escaped + " || '%' ESCAPE '\\'"
	}

	return f.Expr + " " + comparisons[op] + " " + ph + cast
}

// setTest emits a membership test against an array-valued expression.
//
// This is the answer to the join problem. Filtering on label with a join produces a
// row per label, so `and` over two labels needs a different query from `or` over two
// labels - a self-join versus one join with an IN. As a correlated array subquery the
// field is scalar-shaped again: both conjunctions are the same query with a different
// keyword, which is exactly what the rest of this compiler assumes.
func (c *compiler) setTest(f Field, op Operator, ph string) string {
	member := ph + "::uuid = ANY(" + f.Expr + ")"

	if op == Neq {
		// ARRAY() is never null, so NOT is safe here in a way it would not be
		// against a nullable column.
		return "NOT (" + member + ")"
	}

	return member
}

// anyOf compiles `in`, whose value is a list rather than a scalar.
func (c *compiler) anyOf(f Field, value any, at string) string {
	list, ok := value.([]any)
	if !ok {
		c.fault(path(at, "value"), "expected a list", value)
		return ""
	}

	if len(list) == 0 {
		// An empty list matches nothing, which no caller means. Rejecting it is
		// the same call as rejecting an empty nested group.
		c.fault(path(at, "value"), "empty list", value)
		return ""
	}

	// A typed slice, not []any: pgx maps []string to text[] and []float64 to
	// numeric[], and has nothing to send for a []any.
	var (
		strs  []string
		nums  []float64
		bad   bool
		texts = f.Kind != Number
	)

	for i, raw := range list {
		v, problem := coerce(f.Kind, raw)
		if problem != "" {
			c.fault(path(at, "value["+strconv.Itoa(i)+"]"), problem, raw)
			bad = true
			continue
		}

		if texts {
			strs = append(strs, v.(string))
			continue
		}
		nums = append(nums, v.(float64))
	}

	if bad {
		return ""
	}

	if f.Kind == UUIDSet {
		// Array overlap: the ticket's labels intersect the ones asked for. The
		// set analogue of = ANY, and it stays one query for any list length.
		return f.Expr + " && " + c.arg(strs) + arrayCasts[UUIDSet]
	}

	if texts {
		return f.Expr + " = ANY(" + c.arg(strs) + arrayCasts[f.Kind] + ")"
	}

	return f.Expr + " = ANY(" + c.arg(nums) + arrayCasts[f.Kind] + ")"
}
