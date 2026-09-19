package filter_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/filter"
)

// A stand-in vocabulary. Deliberately not internal/ticket's: these tests pin the
// compiler's behaviour, and importing the real schema would make them fail when a
// field is renamed for reasons that have nothing to do with this package.
var schema = filter.Schema{
	"title":      {Expr: "t.title", Kind: filter.Text},
	"status":     {Expr: "t.status_id", Kind: filter.UUID},
	"created_at": {Expr: "t.created_at", Kind: filter.Time},
	"points":     {Expr: "t.points", Kind: filter.Number},
	"label":      {Expr: "ARRAY(SELECT tl.label_id FROM ticket_label tl WHERE tl.ticket_id = t.id)", Kind: filter.UUIDSet},
}

const (
	uuidA = "11111111-1111-4111-8111-111111111111"
	uuidB = "22222222-2222-4222-8222-222222222222"
)

func compile(t *testing.T, g filter.Group) (string, []any) {
	t.Helper()

	sql, args, err := schema.Compile(g, 1)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	return sql, args
}

func faults(t *testing.T, g filter.Group) []filter.Fault {
	t.Helper()

	_, _, err := schema.Compile(g, 1)
	if err == nil {
		t.Fatal("expected the filter to be rejected, but it compiled")
	}

	var e *filter.Error
	if !errors.As(err, &e) {
		t.Fatalf("expected *filter.Error, got %T", err)
	}

	return e.Faults
}

// An empty root group is "no filter", and a list endpoint should not have to branch on
// whether to AND the fragment in.
func TestEmptyRootGroupMatchesEverything(t *testing.T) {
	sql, args := compile(t, filter.Group{Op: filter.And})

	if sql != "TRUE" {
		t.Errorf("empty root compiled to %q, want TRUE", sql)
	}
	if len(args) != 0 {
		t.Errorf("empty root produced %d arguments, want none", len(args))
	}
}

// An empty group *inside* a filter is different: nothing sends one on purpose, and
// treating it as TRUE widens the result set silently.
func TestEmptyNestedGroupIsRejected(t *testing.T) {
	got := faults(t, filter.Group{
		Op:     filter.And,
		Groups: []filter.Group{{Op: filter.Or}},
	})

	if len(got) != 1 || got[0].Location != "groups[0]" {
		t.Fatalf("faults = %+v, want one at groups[0]", got)
	}
}

func TestGroupOfOneHasNoParentheses(t *testing.T) {
	sql, args := compile(t, filter.Group{
		Op:         filter.And,
		Conditions: []filter.Condition{{Field: "status", Op: filter.Eq, Value: uuidA}},
	})

	if sql != "t.status_id = $1::uuid" {
		t.Errorf("sql = %q", sql)
	}
	if len(args) != 1 || args[0] != uuidA {
		t.Errorf("args = %v, want [%s]", args, uuidA)
	}
}

// or inside and inside or. The shape that breaks a compiler which joins a flat list
// and hopes precedence works out.
func TestNestingKeepsEachGroupParenthesised(t *testing.T) {
	sql, args := compile(t, filter.Group{
		Op:         filter.Or,
		Conditions: []filter.Condition{{Field: "title", Op: filter.Eq, Value: "a"}},
		Groups: []filter.Group{{
			Op:         filter.And,
			Conditions: []filter.Condition{{Field: "title", Op: filter.Eq, Value: "b"}},
			Groups: []filter.Group{{
				Op: filter.Or,
				Conditions: []filter.Condition{
					{Field: "title", Op: filter.Eq, Value: "c"},
					{Field: "title", Op: filter.Eq, Value: "d"},
				},
			}},
		}},
	})

	const want = "(t.title = $1 OR (t.title = $2 AND (t.title = $3 OR t.title = $4)))"
	if sql != want {
		t.Errorf("sql  = %q\nwant = %q", sql, want)
	}

	// Argument order follows placeholder order, which is what lets a caller append
	// this slice to its own.
	if len(args) != 4 || args[0] != "a" || args[3] != "d" {
		t.Errorf("args = %v", args)
	}
}

// firstArg is what lets the fragment drop into a statement that already has
// parameters - the list query holds the caller's account in $1 before any filter.
func TestPlaceholdersStartWhereTheCallerSaysTheyDo(t *testing.T) {
	sql, _, err := schema.Compile(filter.Group{
		Op: filter.And,
		Conditions: []filter.Condition{
			{Field: "title", Op: filter.Eq, Value: "a"},
			{Field: "title", Op: filter.Eq, Value: "b"},
		},
	}, 7)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	if want := "(t.title = $7 AND t.title = $8)"; sql != want {
		t.Errorf("sql = %q, want %q", sql, want)
	}
}

func TestUnknownFieldIsRejectedRatherThanDropped(t *testing.T) {
	got := faults(t, filter.Group{
		Op: filter.And,
		Conditions: []filter.Condition{
			{Field: "title", Op: filter.Eq, Value: "keep"},
			{Field: "assignee_name", Op: filter.Eq, Value: "x"},
		},
	})

	if len(got) != 1 {
		t.Fatalf("faults = %+v, want exactly one", got)
	}
	if got[0].Location != "conditions[1].field" {
		t.Errorf("location = %q, want conditions[1].field", got[0].Location)
	}
	if got[0].Value != "assignee_name" {
		t.Errorf("value = %v, want the rejected name", got[0].Value)
	}
}

// A real field with an operator its type cannot support. Letting it through means
// Postgres raises the type error, which reaches the caller as a 500.
func TestOperatorMustFitTheFieldType(t *testing.T) {
	got := faults(t, filter.Group{
		Op:         filter.And,
		Conditions: []filter.Condition{{Field: "created_at", Op: filter.Contains, Value: "x"}},
	})

	if len(got) != 1 || got[0].Location != "conditions[0].op" {
		t.Fatalf("faults = %+v, want one at conditions[0].op", got)
	}
}

// Every problem, not the first. One round trip per mistake is what the errors array
// exists to avoid.
func TestEveryFaultIsReportedAtItsOwnPosition(t *testing.T) {
	got := faults(t, filter.Group{
		Op: filter.Or,
		Conditions: []filter.Condition{
			{Field: "nope", Op: filter.Eq, Value: "x"},
			{Field: "status", Op: filter.Eq, Value: "not-a-uuid"},
		},
		Groups: []filter.Group{{
			Op:         filter.And,
			Conditions: []filter.Condition{{Field: "points", Op: filter.Eq, Value: "seven"}},
		}},
	})

	want := []string{"conditions[0].field", "conditions[1].value", "groups[0].conditions[0].value"}
	if len(got) != len(want) {
		t.Fatalf("faults = %+v, want %d", got, len(want))
	}

	for i, location := range want {
		if got[i].Location != location {
			t.Errorf("fault %d at %q, want %q", i, got[i].Location, location)
		}
	}
}

// A malformed uuid reaching $1::uuid is a 22P02 at execution time, reported as this
// server's fault with no position attached.
func TestMalformedUUIDIsCaughtHereNotByPostgres(t *testing.T) {
	got := faults(t, filter.Group{
		Op:         filter.And,
		Conditions: []filter.Condition{{Field: "status", Op: filter.Eq, Value: "11111111-1111-4111-8111"}},
	})

	if len(got) != 1 || got[0].Message != "not a uuid" {
		t.Fatalf("faults = %+v", got)
	}
}

// <> on a nullable column silently hides null rows: "status is not Done" would drop
// every unstatused ticket and report nothing wrong.
func TestNotEqualsUsesIsDistinctFrom(t *testing.T) {
	sql, _ := compile(t, filter.Group{
		Op:         filter.And,
		Conditions: []filter.Condition{{Field: "status", Op: filter.Neq, Value: uuidA}},
	})

	if !strings.Contains(sql, "IS DISTINCT FROM") {
		t.Errorf("sql = %q, want IS DISTINCT FROM", sql)
	}
}

// A value holding % or _ must not become a wildcard.
func TestContainsEscapesLikeMetacharacters(t *testing.T) {
	sql, args := compile(t, filter.Group{
		Op:         filter.And,
		Conditions: []filter.Condition{{Field: "title", Op: filter.Contains, Value: "50%_off"}},
	})

	if !strings.Contains(sql, "ESCAPE") || !strings.Contains(sql, "replace(") {
		t.Errorf("sql = %q, want an escaped ILIKE", sql)
	}

	// The value is still an argument. Escaping it in SQL rather than in Go is what
	// keeps that true.
	if len(args) != 1 || args[0] != "50%_off" {
		t.Errorf("args = %v, want the raw value", args)
	}
}

// The property the whole package rests on.
func TestNoValueIsEverFormattedIntoSQL(t *testing.T) {
	const injection = "'; DROP TABLE ticket; --"

	sql, args := compile(t, filter.Group{
		Op: filter.And,
		Conditions: []filter.Condition{
			{Field: "title", Op: filter.Eq, Value: injection},
			{Field: "title", Op: filter.Contains, Value: injection},
			{Field: "title", Op: filter.In, Value: []any{injection}},
		},
	})

	if strings.Contains(sql, "DROP") {
		t.Fatalf("a value reached the SQL text: %q", sql)
	}
	if len(args) != 3 {
		t.Fatalf("args = %v, want three", args)
	}
}

func TestInCompilesToAnyOverATypedArray(t *testing.T) {
	sql, args := compile(t, filter.Group{
		Op:         filter.And,
		Conditions: []filter.Condition{{Field: "status", Op: filter.In, Value: []any{uuidA, uuidB}}},
	})

	if want := "t.status_id = ANY($1::uuid[])"; sql != want {
		t.Errorf("sql = %q, want %q", sql, want)
	}

	// pgx has nothing to send for a []any, so the slice has to arrive typed.
	if _, ok := args[0].([]string); !ok {
		t.Errorf("argument is %T, want []string", args[0])
	}
}

func TestInRejectsAnEmptyList(t *testing.T) {
	got := faults(t, filter.Group{
		Op:         filter.And,
		Conditions: []filter.Condition{{Field: "status", Op: filter.In, Value: []any{}}},
	})

	if len(got) != 1 || got[0].Message != "empty list" {
		t.Fatalf("faults = %+v", got)
	}
}

// A set field is scalar-shaped to the compiler, which is what keeps and/or over two
// labels one query rather than two query shapes.
func TestSetFieldCompilesToArrayMembership(t *testing.T) {
	sql, _ := compile(t, filter.Group{
		Op: filter.And,
		Conditions: []filter.Condition{
			{Field: "label", Op: filter.Eq, Value: uuidA},
			{Field: "label", Op: filter.Eq, Value: uuidB},
		},
	})

	if strings.Count(sql, "= ANY(ARRAY(") != 2 || !strings.Contains(sql, " AND ") {
		t.Errorf("sql = %q", sql)
	}
}

func TestSetFieldIsNullAsksWhetherTheSetIsEmpty(t *testing.T) {
	sql, args := compile(t, filter.Group{
		Op:         filter.And,
		Conditions: []filter.Condition{{Field: "label", Op: filter.IsNull}},
	})

	if !strings.HasPrefix(sql, "cardinality(") || !strings.HasSuffix(sql, ") = 0") {
		t.Errorf("sql = %q", sql)
	}
	if len(args) != 0 {
		t.Errorf("args = %v, want none - is_null takes no value", args)
	}
}

func TestNestingDeeperThanTheLimitIsRejected(t *testing.T) {
	g := filter.Group{
		Op:         filter.And,
		Conditions: []filter.Condition{{Field: "title", Op: filter.Eq, Value: "x"}},
	}
	for i := 0; i < filter.MaxDepth; i++ {
		g = filter.Group{Op: filter.And, Groups: []filter.Group{g}}
	}

	got := faults(t, g)
	if len(got) != 1 || !strings.Contains(got[0].Message, "nests deeper") {
		t.Fatalf("faults = %+v", got)
	}
}

// Depth alone does not bound size: one flat group of ten thousand conditions is the
// same denial of service on a Raspberry Pi.
func TestTooManyConditionsIsRejectedOnce(t *testing.T) {
	g := filter.Group{Op: filter.And}
	for i := 0; i <= filter.MaxConditions+10; i++ {
		g.Conditions = append(g.Conditions, filter.Condition{Field: "title", Op: filter.Eq, Value: "x"})
	}

	got := faults(t, g)
	if len(got) != 1 || !strings.Contains(got[0].Message, "more than") {
		t.Fatalf("faults = %+v, want exactly one", got)
	}
}

func TestGroupNeedsARealConjunction(t *testing.T) {
	got := faults(t, filter.Group{
		Op:         filter.Conjunction("xor"),
		Conditions: []filter.Condition{{Field: "title", Op: filter.Eq, Value: "x"}},
	})

	if len(got) != 1 || got[0].Location != "op" {
		t.Fatalf("faults = %+v, want one at op", got)
	}
}

// saved_view.config persists this shape, so it has to survive a round trip through
// JSON unchanged - including the recursion, which is the part a hand-written
// unmarshaller would be needed for if the encoding were a discriminated union.
func TestTheWireShapeRoundTrips(t *testing.T) {
	const wire = `{"op":"and","conditions":[{"field":"title","op":"contains","value":"log"}],` +
		`"groups":[{"op":"or","conditions":[{"field":"status","op":"is_null"}]}]}`

	var g filter.Group
	if err := json.Unmarshal([]byte(wire), &g); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	out, err := json.Marshal(g)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if string(out) != wire {
		t.Errorf("round trip changed the shape:\n got %s\nwant %s", out, wire)
	}
}

func TestSortAlwaysEndsWithTheTiebreak(t *testing.T) {
	clause, err := schema.CompileOrder(nil, "t.id")
	if err != nil {
		t.Fatalf("compile order: %v", err)
	}

	if clause != "ORDER BY t.id" {
		t.Errorf("clause = %q", clause)
	}
}

func TestSortSpellsOutNullsLastInBothDirections(t *testing.T) {
	clause, err := schema.CompileOrder([]filter.Order{
		{Field: "status"},
		{Field: "created_at", Desc: true},
	}, "t.id")
	if err != nil {
		t.Fatalf("compile order: %v", err)
	}

	const want = "ORDER BY t.status_id ASC NULLS LAST, t.created_at DESC NULLS LAST, t.id"
	if clause != want {
		t.Errorf("clause = %q\nwant   = %q", clause, want)
	}
}

// A sort key cannot be a placeholder, so the whitelist is the only thing between a
// caller's string and the query text.
func TestSortRejectsAnUnknownField(t *testing.T) {
	_, err := schema.CompileOrder([]filter.Order{{Field: "t.id; DROP TABLE ticket"}}, "t.id")
	if err == nil {
		t.Fatal("an unknown sort field compiled")
	}

	var e *filter.Error
	if !errors.As(err, &e) || e.Faults[0].Location != "sort[0].field" {
		t.Fatalf("err = %v", err)
	}
}

func TestSortRejectsASetField(t *testing.T) {
	_, err := schema.CompileOrder([]filter.Order{{Field: "label"}}, "t.id")
	if err == nil {
		t.Fatal("a ticket has many labels, so sorting by label should be rejected")
	}
}

// A Schema this repository got wrong is a programming error, not a caller's - it would
// otherwise surface as malformed SQL at request time.
func TestAMisconfiguredSchemaPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("a field with no Kind compiled")
		}
	}()

	bad := filter.Schema{"x": {Expr: "t.x"}}
	bad.Compile(filter.Group{Op: filter.And}, 1) //nolint:errcheck // it panics first
}
