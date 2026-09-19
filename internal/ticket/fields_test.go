package ticket_test

import (
	"context"
	"testing"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/filter"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/ticket"
)

// internal/filter's own tests compare compiled SQL against expected strings, which
// proves the compiler is consistent and proves nothing about whether Postgres accepts
// what it emits. A missing cast, a wrong array type or a malformed ESCAPE all pass a
// string comparison and fail at execution.
//
// These run the real vocabulary against the real table, which is why they live here:
// internal/filter may not name ticket at all.

// sample is one value of each kind, good enough to put in a placeholder.
var sample = map[filter.Kind]any{
	filter.Text:    "x",
	filter.UUID:    "11111111-1111-4111-8111-111111111111",
	filter.Time:    "2026-01-01T00:00:00Z",
	filter.Number:  float64(1),
	filter.Bool:    true,
	filter.UUIDSet: "11111111-1111-4111-8111-111111111111",
}

var everyOperator = []filter.Operator{
	filter.Eq, filter.Neq, filter.Gt, filter.Gte, filter.Lt, filter.Lte,
	filter.Contains, filter.In, filter.IsNull, filter.IsNotNull,
}

// Every fragment the compiler can emit for this schema, executed. A combination the
// compiler rejects is skipped - that half is internal/filter's to test - so what this
// asserts is the other half: nothing that compiles fails to run.
func TestEveryCompilableConditionIsValidSQL(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	ran := 0

	for name, f := range ticket.Fields {
		for _, op := range everyOperator {
			cond := filter.Condition{Field: name, Op: op}

			switch op {
			case filter.IsNull, filter.IsNotNull:
			case filter.In:
				cond.Value = []any{sample[f.Kind]}
			default:
				cond.Value = sample[f.Kind]
			}

			where, args, err := ticket.Fields.Compile(
				filter.Group{Op: filter.And, Conditions: []filter.Condition{cond}}, 1)
			if err != nil {
				continue
			}

			var n int
			query := `SELECT count(*) FROM ticket t WHERE ` + where
			if err := w.pool.QueryRow(ctx, query, args...).Scan(&n); err != nil {
				t.Errorf("%s %s did not run:\n  %s\n  %v", name, op, query, err)
			}

			ran++
		}
	}

	// A guard against the loop covering nothing - a renamed field or an empty
	// Fields map would otherwise make this test pass by testing zero fragments.
	if ran < 40 {
		t.Fatalf("only %d combinations ran, which is too few to mean anything", ran)
	}
}

// The same for sorting, where the field name is interpolated rather than bound.
func TestEverySortableFieldProducesValidSQL(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	for name, f := range ticket.Fields {
		for _, desc := range []bool{false, true} {
			clause, err := ticket.Fields.CompileOrder([]filter.Order{{Field: name, Desc: desc}}, ticket.Tiebreak)
			if err != nil {
				if f.Kind != filter.UUIDSet {
					t.Errorf("%s is not sortable: %v", name, err)
				}
				continue
			}

			query := `SELECT t.id FROM ticket t ` + clause + ` LIMIT 1`
			if _, err := w.pool.Exec(ctx, query); err != nil {
				t.Errorf("sort by %s did not run:\n  %s\n  %v", name, query, err)
			}
		}
	}
}

// count runs a one-condition filter and returns how many tickets matched.
func (w world) count(t *testing.T, cond filter.Condition) int {
	t.Helper()

	where, args, err := ticket.Fields.Compile(
		filter.Group{Op: filter.And, Conditions: []filter.Condition{cond}}, 1)
	if err != nil {
		t.Fatalf("compile %+v: %v", cond, err)
	}

	var n int
	if err := w.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM ticket t WHERE `+where, args...).Scan(&n); err != nil {
		t.Fatalf("run %+v: %v", cond, err)
	}

	return n
}

// The escaping, against real rows. A naive ILIKE '%' || $1 || '%' passes every string
// test in internal/filter and matches the wrong row here.
func TestContainsTreatsWildcardsAsText(t *testing.T) {
	w := newWorld(t)
	w.create(t, "50% off")
	w.create(t, "50x off")

	if n := w.count(t, filter.Condition{Field: "title", Op: filter.Contains, Value: "50%"}); n != 1 {
		t.Errorf("contains %%50%%%% matched %d tickets, want only the literal one", n)
	}
	// _ is LIKE's single-character wildcard, so unescaped this matches both.
	if n := w.count(t, filter.Condition{Field: "title", Op: filter.Contains, Value: "50_"}); n != 0 {
		t.Errorf("contains \"50_\" matched %d tickets, want none", n)
	}
	// And it still finds text, case-insensitively.
	if n := w.count(t, filter.Condition{Field: "title", Op: filter.Contains, Value: "OFF"}); n != 2 {
		t.Errorf("contains \"OFF\" matched %d tickets, want both", n)
	}
}

// neq on a nullable column has to include the nulls, or "status is not Done" hides
// every unstatused ticket and says nothing about it.
func TestNotEqualsIncludesNullRows(t *testing.T) {
	w := newWorld(t)
	w.create(t, "no status")

	status := w.newStatus(t, "Done")
	done := w.create(t, "done")
	w.setStatus(t, done.ID, &status)

	other := "33333333-3333-4333-8333-333333333333"
	if n := w.count(t, filter.Condition{Field: "status", Op: filter.Neq, Value: other}); n != 2 {
		t.Errorf("status neq matched %d tickets, want both including the unstatused one", n)
	}
	if n := w.count(t, filter.Condition{Field: "status", Op: filter.Neq, Value: status}); n != 1 {
		t.Errorf("status neq Done matched %d, want the unstatused ticket", n)
	}
}

// The set field, against the join table it stands for. and over two labels means a
// ticket wearing both; or means either - and both are one query.
func TestLabelSetMatchesThroughTheJoinTable(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	both := w.create(t, "both labels")
	one := w.create(t, "one label")
	w.create(t, "no labels")

	bug := w.newLabel(t, "bug")
	urgent := w.newLabel(t, "urgent")
	w.tag(t, both.ID, bug)
	w.tag(t, both.ID, urgent)
	w.tag(t, one.ID, bug)

	and, args, err := ticket.Fields.Compile(filter.Group{
		Op: filter.And,
		Conditions: []filter.Condition{
			{Field: "label", Op: filter.Eq, Value: bug},
			{Field: "label", Op: filter.Eq, Value: urgent},
		},
	}, 1)
	if err != nil {
		t.Fatalf("compile and: %v", err)
	}

	var n int
	if err := w.pool.QueryRow(ctx, `SELECT count(*) FROM ticket t WHERE `+and, args...).Scan(&n); err != nil {
		t.Fatalf("run and: %v", err)
	}
	if n != 1 {
		t.Errorf("both labels matched %d tickets, want 1", n)
	}

	if n := w.count(t, filter.Condition{Field: "label", Op: filter.In, Value: []any{bug, urgent}}); n != 2 {
		t.Errorf("either label matched %d tickets, want 2", n)
	}
	if n := w.count(t, filter.Condition{Field: "label", Op: filter.IsNull}); n != 1 {
		t.Errorf("unlabelled matched %d tickets, want 1", n)
	}
	if n := w.count(t, filter.Condition{Field: "label", Op: filter.Neq, Value: urgent}); n != 2 {
		t.Errorf("not-urgent matched %d tickets, want 2 - a ticket with no labels is not urgent", n)
	}
}

// Fixtures. These name tables this package does not own, which is what the fixture
// allowance in each owner's boundary test covers - and the ones they do not name are
// unowned so far.

func (w world) newStatus(t *testing.T, name string) string {
	t.Helper()

	var id string
	err := w.pool.QueryRow(context.Background(),
		`INSERT INTO status (team_id, name, color, position, category)
		 SELECT p.team_id, $2, '#00ff00', 1, 'in_progress' FROM project p WHERE p.id = $1::uuid
		 RETURNING id::text`, w.project, name).Scan(&id)
	if err != nil {
		t.Fatalf("fixture status: %v", err)
	}

	return id
}

func (w world) setStatus(t *testing.T, ticketID string, status *string) {
	t.Helper()

	if _, err := w.pool.Exec(context.Background(),
		`UPDATE ticket SET status_id = $2::uuid WHERE id = $1::uuid`, ticketID, status); err != nil {
		t.Fatalf("fixture set status: %v", err)
	}
}

func (w world) newLabel(t *testing.T, name string) string {
	t.Helper()

	var id string
	err := w.pool.QueryRow(context.Background(),
		`INSERT INTO label (team_id, name, color)
		 SELECT p.team_id, $2, '#ff0000' FROM project p WHERE p.id = $1::uuid
		 RETURNING id::text`, w.project, name).Scan(&id)
	if err != nil {
		t.Fatalf("fixture label: %v", err)
	}

	return id
}

func (w world) tag(t *testing.T, ticketID, labelID string) {
	t.Helper()

	_, err := w.pool.Exec(context.Background(),
		`INSERT INTO ticket_label (ticket_id, label_id, project_id, team_id)
		 SELECT $1::uuid, $2::uuid, p.id, p.team_id FROM project p WHERE p.id = $3::uuid`,
		ticketID, labelID, w.project)
	if err != nil {
		t.Fatalf("fixture ticket_label: %v", err)
	}
}
