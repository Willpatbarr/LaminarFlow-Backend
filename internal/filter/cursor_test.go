package filter_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/filter"
)

func ptr(s string) *string { return &s }

func TestACursorRoundTrips(t *testing.T) {
	want := filter.Cursor{Query: "abc123", Key: []*string{ptr("2026-01-01 00:00:00+00"), nil, ptr(uuidA)}}

	got, err := filter.Decode(want.Encode())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if got.Query != want.Query || len(got.Key) != len(want.Key) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	// The null is the part a []string would have lost: a page boundary can land
	// inside the rows whose sort column is null.
	if got.Key[1] != nil {
		t.Errorf("a null key value came back as %q", *got.Key[1])
	}
	if *got.Key[2] != uuidA {
		t.Errorf("tiebreak = %q", *got.Key[2])
	}
}

func TestAMangledTokenDoesNotDecode(t *testing.T) {
	for _, token := range []string{"", "!!!", "eyJ", "YWJj"} {
		if _, err := filter.Decode(token); !errors.Is(err, filter.ErrBadCursor) {
			t.Errorf("Decode(%q) = %v, want ErrBadCursor", token, err)
		}
	}
}

func TestTheFingerprintTracksTheFilterAndTheSort(t *testing.T) {
	base := filter.Group{
		Op:         filter.And,
		Conditions: []filter.Condition{{Field: "title", Op: filter.Eq, Value: "a"}},
	}
	sort := []filter.Order{{Field: "created_at", Desc: true}}

	same := filter.Fingerprint(base, sort)
	if same != filter.Fingerprint(base, sort) {
		t.Fatal("the same query fingerprinted differently twice")
	}

	other := filter.Group{
		Op:         filter.And,
		Conditions: []filter.Condition{{Field: "title", Op: filter.Eq, Value: "b"}},
	}
	if filter.Fingerprint(other, sort) == same {
		t.Error("a different filter shares a fingerprint")
	}
	if filter.Fingerprint(base, []filter.Order{{Field: "created_at"}}) == same {
		t.Error("flipping the sort direction did not change the fingerprint")
	}
}

func after(t *testing.T, orders []filter.Order, key []*string) (string, []any) {
	t.Helper()

	sql, args, err := schema.CompileAfter(orders, "t.id", filter.Cursor{Query: "x", Key: key}, 1)
	if err != nil {
		t.Fatalf("compile after: %v", err)
	}

	return sql, args
}

// The simplest shape, and the one the default sort produces.
func TestOneAscendingKeyResumesAfterIt(t *testing.T) {
	sql, args := after(t,
		[]filter.Order{{Field: "created_at"}},
		[]*string{ptr("2026-01-01 00:00:00+00"), ptr(uuidA)})

	const want = "((t.created_at > $1::timestamptz OR t.created_at IS NULL) OR " +
		"((t.created_at IS NOT DISTINCT FROM $2::timestamptz) AND t.id > $3::uuid))"
	if sql != want {
		t.Errorf("sql  = %q\nwant = %q", sql, want)
	}

	// The key value is bound twice, once per arm, and the tiebreak once. Order
	// matters: it has to match the order the placeholders appear in.
	if len(args) != 3 || args[0] != args[1] || args[2] != uuidA {
		t.Errorf("args = %v", args)
	}
}

func TestADescendingKeyComparesTheOtherWay(t *testing.T) {
	sql, _ := after(t,
		[]filter.Order{{Field: "created_at", Desc: true}},
		[]*string{ptr("2026-01-01 00:00:00+00"), ptr(uuidA)})

	if !strings.Contains(sql, "t.created_at < $1") {
		t.Errorf("sql = %q, want a < comparison", sql)
	}
	// NULLS LAST in both directions, so a null is still after a value.
	if !strings.Contains(sql, "t.created_at IS NULL") {
		t.Errorf("sql = %q, want the nulls-last arm", sql)
	}
}

// Under NULLS LAST nothing follows a null, so the "after" arm is dead and the whole
// term collapses to the equality branch - which is how a boundary inside the null run
// still advances, on the tiebreak alone.
func TestANullKeyAdvancesOnTheTiebreakAlone(t *testing.T) {
	sql, args := after(t,
		[]filter.Order{{Field: "status"}},
		[]*string{nil, ptr(uuidA)})

	const want = "(t.status_id IS NULL AND t.id > $1::uuid)"
	if sql != want {
		t.Errorf("sql  = %q\nwant = %q", sql, want)
	}
	if len(args) != 1 {
		t.Errorf("args = %v, want only the tiebreak", args)
	}
}

// Mixed directions are why this is written out lexicographically: Postgres has no row
// comparison that mixes ASC and DESC.
func TestTwoKeysNestLexicographically(t *testing.T) {
	sql, args := after(t,
		[]filter.Order{{Field: "status"}, {Field: "created_at", Desc: true}},
		[]*string{ptr(uuidB), ptr("2026-01-01 00:00:00+00"), ptr(uuidA)})

	if !strings.Contains(sql, "t.status_id > $1::uuid") {
		t.Errorf("sql = %q, want the first key ascending", sql)
	}
	if !strings.Contains(sql, "t.created_at < $3::timestamptz") {
		t.Errorf("sql = %q, want the second key descending", sql)
	}
	// Innermost: the tiebreak is only reached when every key above it is equal.
	if !strings.Contains(sql, "t.id > $5::uuid)") || strings.Contains(sql, "$6") {
		t.Errorf("sql = %q, want the tiebreak last and innermost", sql)
	}
	if len(args) != 5 {
		t.Errorf("args = %v, want two per key plus the tiebreak", args)
	}
}

func TestPlaceholdersInACursorStartWhereTheCallerSaysTheyDo(t *testing.T) {
	sql, _, err := schema.CompileAfter([]filter.Order{{Field: "created_at"}}, "t.id",
		filter.Cursor{Query: "x", Key: []*string{ptr("2026-01-01 00:00:00+00"), ptr(uuidA)}}, 4)
	if err != nil {
		t.Fatalf("compile after: %v", err)
	}

	if !strings.Contains(sql, "$4") || !strings.Contains(sql, "$6::uuid") {
		t.Errorf("sql = %q, want placeholders from $4", sql)
	}
}

// A key of the wrong length is a cursor from a different sort, whatever its
// fingerprint says. Checked here so the service cannot forget to.
func TestAKeyOfTheWrongLengthIsAMismatch(t *testing.T) {
	_, _, err := schema.CompileAfter([]filter.Order{{Field: "created_at"}}, "t.id",
		filter.Cursor{Query: "x", Key: []*string{ptr(uuidA)}}, 1)

	if !errors.Is(err, filter.ErrCursorMismatch) {
		t.Errorf("err = %v, want ErrCursorMismatch", err)
	}
}

// The tiebreak is a primary key, so a null or malformed one is corruption rather than
// a position.
func TestACorruptTiebreakIsRejected(t *testing.T) {
	for _, key := range [][]*string{
		{ptr("2026-01-01 00:00:00+00"), nil},
		{ptr("2026-01-01 00:00:00+00"), ptr("not-a-uuid")},
	} {
		_, _, err := schema.CompileAfter([]filter.Order{{Field: "created_at"}}, "t.id",
			filter.Cursor{Query: "x", Key: key}, 1)

		if !errors.Is(err, filter.ErrBadCursor) {
			t.Errorf("key %v gave %v, want ErrBadCursor", key, err)
		}
	}
}

// A cursor over a field that is not sortable should fail the same way the ORDER BY
// would, not compile into a comparison against an array.
func TestACursorOverAnUnsortableFieldIsRejected(t *testing.T) {
	_, _, err := schema.CompileAfter([]filter.Order{{Field: "label"}}, "t.id",
		filter.Cursor{Query: "x", Key: []*string{ptr(uuidB), ptr(uuidA)}}, 1)

	var e *filter.Error
	if !errors.As(err, &e) {
		t.Fatalf("err = %v, want a filter.Error", err)
	}
}

// The columns the cursor is minted from have to be the columns the ORDER BY uses, or
// the page boundary is in a different place from the sort.
func TestSortKeysCoverEverySortColumnAndTheTiebreak(t *testing.T) {
	keys, err := schema.SortKeys([]filter.Order{{Field: "status"}, {Field: "title"}}, "t.id")
	if err != nil {
		t.Fatalf("sort keys: %v", err)
	}

	want := []string{"(t.status_id)::text", "(t.title)::text", "(t.id)::text"}
	if len(keys) != len(want) {
		t.Fatalf("keys = %v, want %v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Errorf("key %d = %q, want %q", i, keys[i], want[i])
		}
	}
}
