package ticket_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/filter"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/ticket"
)

// list runs one page and fails the test if it errors.
func (w world) list(t *testing.T, as string, p ticket.ListParams) ticket.Page {
	t.Helper()

	page, err := w.svc.List(context.Background(), as, p)
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	return page
}

func ids(page ticket.Page) []string {
	out := make([]string, len(page.Tickets))
	for i, t := range page.Tickets {
		out[i] = t.ID
	}

	return out
}

// drain pages all the way through and returns every id, in order, with the number of
// requests it took. Pagination bugs show up as a duplicate, a gap, or a loop.
func (w world) drain(t *testing.T, as string, p ticket.ListParams) ([]string, int) {
	t.Helper()

	var (
		all   []string
		pages int
	)

	for {
		page := w.list(t, as, p)
		all = append(all, ids(page)...)
		pages++

		if !page.HasMore {
			return all, pages
		}
		if pages > 50 {
			t.Fatal("pagination did not terminate")
		}

		p.Cursor = page.NextCursor
	}
}

// setUpdated forces updated_at, so a test can build the orderings that matter rather
// than hoping the clock produces them.
func (w world) setUpdated(t *testing.T, id string, at time.Time) {
	t.Helper()

	if _, err := w.pool.Exec(context.Background(),
		`UPDATE ticket SET updated_at = $2 WHERE id = $1::uuid`, id, at); err != nil {
		t.Fatalf("fixture updated_at: %v", err)
	}
}

// A result smaller than one page has no cursor at all. A client that sees one asks for
// another page, so offering one here would cost an extra empty round trip on every
// short list.
func TestOnePageHasNoCursor(t *testing.T) {
	w := newWorld(t)
	w.create(t, "one")
	w.create(t, "two")

	page := w.list(t, w.member, ticket.ListParams{Limit: 10})

	if len(page.Tickets) != 2 {
		t.Fatalf("got %d tickets, want 2", len(page.Tickets))
	}
	if page.HasMore || page.NextCursor != "" {
		t.Errorf("has_more = %v, cursor = %q, want neither", page.HasMore, page.NextCursor)
	}
}

// The boundary an off-by-one lives at: exactly page-size rows is the last page, not a
// full page followed by an empty one.
func TestExactlyPageSizeIsTheLastPage(t *testing.T) {
	w := newWorld(t)
	for i := 0; i < 3; i++ {
		w.create(t, "t")
	}

	page := w.list(t, w.member, ticket.ListParams{Limit: 3})

	if len(page.Tickets) != 3 {
		t.Fatalf("got %d tickets, want 3", len(page.Tickets))
	}
	if page.HasMore {
		t.Error("has_more is true on a result that is exactly one page")
	}
}

func TestPaginationVisitsEveryRowExactlyOnce(t *testing.T) {
	w := newWorld(t)

	want := map[string]bool{}
	for i := 0; i < 7; i++ {
		want[w.create(t, "t").ID] = true
	}

	got, pages := w.drain(t, w.member, ticket.ListParams{Limit: 2})

	if pages != 4 {
		t.Errorf("took %d pages for 7 rows at 2 per page, want 4", pages)
	}
	if len(got) != 7 {
		t.Fatalf("got %d ids, want 7: %v", len(got), got)
	}
	for _, id := range got {
		if !want[id] {
			t.Errorf("id %s appeared twice or was never created", id)
		}
		delete(want, id)
	}
	if len(want) != 0 {
		t.Errorf("%d tickets were never returned", len(want))
	}
}

// Two rows sharing the sort value is the case a cursor on the sort key alone gets
// wrong: without a unique tiebreak one of them is skipped or repeated.
func TestRowsSharingASortValueArePagedThroughExactlyOnce(t *testing.T) {
	w := newWorld(t)

	same := time.Now().UTC().Truncate(time.Second)
	a := w.create(t, "a")
	b := w.create(t, "b")
	c := w.create(t, "c")
	for _, id := range []string{a.ID, b.ID, c.ID} {
		w.setUpdated(t, id, same)
	}

	got, _ := w.drain(t, w.member, ticket.ListParams{Limit: 1})

	if len(got) != 3 {
		t.Fatalf("got %d ids for 3 rows with one timestamp, want 3: %v", len(got), got)
	}
	seen := map[string]bool{}
	for _, id := range got {
		if seen[id] {
			t.Errorf("id %s came back twice", id)
		}
		seen[id] = true
	}
}

// A row created after page 1 is newer than everything on it, and the default sort is
// newest first - so it belongs before the cursor, not after it. Keyset pagination
// returns a consistent window; it does not promise to show rows that appeared behind
// the reader.
func TestARowInsertedMidPaginationDoesNotDisturbThePage(t *testing.T) {
	w := newWorld(t)

	first := w.create(t, "first")
	second := w.create(t, "second")

	page1 := w.list(t, w.member, ticket.ListParams{Limit: 1})
	if got := ids(page1); len(got) != 1 || got[0] != second.ID {
		t.Fatalf("page 1 = %v, want the newest ticket %s", got, second.ID)
	}

	interloper := w.create(t, "arrived late")

	page2 := w.list(t, w.member, ticket.ListParams{Limit: 1, Cursor: page1.NextCursor})
	got := ids(page2)

	if len(got) != 1 || got[0] != first.ID {
		t.Fatalf("page 2 = %v, want %s", got, first.ID)
	}
	for _, id := range got {
		if id == interloper.ID {
			t.Error("a ticket created after page 1 appeared on page 2")
		}
	}
}

// Scope is not a filter condition and must not be reachable through one.
func TestListShowsOnlyTheCallersTickets(t *testing.T) {
	w := newWorld(t)
	w.create(t, "theirs")

	page := w.list(t, w.outsid, ticket.ListParams{})

	if len(page.Tickets) != 0 {
		t.Errorf("an outsider saw %d tickets", len(page.Tickets))
	}
}

// The archive predicate applies here too, unconditionally. 0029's tax, paid again.
func TestListExcludesArchivedTickets(t *testing.T) {
	w := newWorld(t)
	kept := w.create(t, "kept")
	gone := w.create(t, "archived")

	if err := w.svc.Archive(context.Background(), w.member, gone.ID); err != nil {
		t.Fatalf("archive: %v", err)
	}

	got := ids(w.list(t, w.member, ticket.ListParams{}))
	if len(got) != 1 || got[0] != kept.ID {
		t.Errorf("got %v, want only the live ticket %s", got, kept.ID)
	}
}

func TestFilterNarrowsTheList(t *testing.T) {
	w := newWorld(t)
	w.create(t, "fix the login page")
	w.create(t, "write the changelog")

	page := w.list(t, w.member, ticket.ListParams{
		Filter: filter.Group{
			Op:         filter.And,
			Conditions: []filter.Condition{{Field: "title", Op: filter.Contains, Value: "login"}},
		},
	})

	if len(page.Tickets) != 1 || page.Tickets[0].Title != "fix the login page" {
		t.Errorf("got %d tickets, want the one matching the filter", len(page.Tickets))
	}
}

// Sorting by a nullable column puts the nulls last in both directions, and pagination
// still walks through them - the page boundary can land inside the null run.
func TestSortingByANullableColumnPagesThroughTheNulls(t *testing.T) {
	w := newWorld(t)

	status := w.newStatus(t, "Done")
	withStatus := w.create(t, "has a status")
	w.setStatus(t, withStatus.ID, &status)
	w.create(t, "no status one")
	w.create(t, "no status two")

	for _, desc := range []bool{false, true} {
		p := ticket.ListParams{Limit: 1, Sort: []filter.Order{{Field: "status", Desc: desc}}}

		got, _ := w.drain(t, w.member, p)
		if len(got) != 3 {
			t.Fatalf("desc=%v returned %d ids, want 3: %v", desc, len(got), got)
		}
		if got[0] != withStatus.ID {
			t.Errorf("desc=%v put a null first; NULLS LAST should keep %s ahead", desc, withStatus.ID)
		}
	}
}

// LAM-59 decision 4. Resuming under a different filter would return rows from neither
// sequence, with nothing for a client to notice.
func TestACursorRefusesADifferentFilter(t *testing.T) {
	w := newWorld(t)
	w.create(t, "a")
	w.create(t, "b")

	page := w.list(t, w.member, ticket.ListParams{Limit: 1})
	if page.NextCursor == "" {
		t.Fatal("no cursor to replay")
	}

	_, err := w.svc.List(context.Background(), w.member, ticket.ListParams{
		Limit:  1,
		Cursor: page.NextCursor,
		Filter: filter.Group{
			Op:         filter.And,
			Conditions: []filter.Condition{{Field: "title", Op: filter.Eq, Value: "a"}},
		},
	})

	if !errors.Is(err, filter.ErrCursorMismatch) {
		t.Errorf("err = %v, want ErrCursorMismatch", err)
	}
}

func TestACursorRefusesADifferentSort(t *testing.T) {
	w := newWorld(t)
	w.create(t, "a")
	w.create(t, "b")

	page := w.list(t, w.member, ticket.ListParams{Limit: 1})

	_, err := w.svc.List(context.Background(), w.member, ticket.ListParams{
		Limit:  1,
		Cursor: page.NextCursor,
		Sort:   []filter.Order{{Field: "title"}},
	})

	if !errors.Is(err, filter.ErrCursorMismatch) {
		t.Errorf("err = %v, want ErrCursorMismatch", err)
	}
}

// Omitting the sort and spelling out the default are the same query, so a cursor
// minted by one has to be accepted by the other.
func TestTheDefaultSortAndSpellingItOutShareACursor(t *testing.T) {
	w := newWorld(t)
	w.create(t, "a")
	w.create(t, "b")

	page := w.list(t, w.member, ticket.ListParams{Limit: 1})

	next := w.list(t, w.member, ticket.ListParams{
		Limit:  1,
		Cursor: page.NextCursor,
		Sort:   ticket.DefaultSort,
	})

	if len(next.Tickets) != 1 {
		t.Errorf("got %d tickets, want the second page", len(next.Tickets))
	}
}

func TestAMangledCursorIsRejected(t *testing.T) {
	w := newWorld(t)
	w.create(t, "a")

	_, err := w.svc.List(context.Background(), w.member, ticket.ListParams{Cursor: "not-a-cursor"})
	if !errors.Is(err, filter.ErrBadCursor) {
		t.Errorf("err = %v, want ErrBadCursor", err)
	}
}

// Rejected, not clamped. LAM-59 decision 3.
func TestAnOversizePageIsRejected(t *testing.T) {
	w := newWorld(t)

	_, err := w.svc.List(context.Background(), w.member,
		ticket.ListParams{Limit: filter.MaxLimit + 1})

	var fe *filter.Error
	if !errors.As(err, &fe) {
		t.Fatalf("err = %v, want a filter.Error", err)
	}
	if fe.Faults[0].Location != "body.limit" {
		t.Errorf("location = %q, want body.limit", fe.Faults[0].Location)
	}
}

// Faults arrive already qualified against the request body, because only this layer
// knows where in the body the filter sat.
func TestFilterFaultsAreLocatedInsideTheBody(t *testing.T) {
	w := newWorld(t)

	_, err := w.svc.List(context.Background(), w.member, ticket.ListParams{
		Filter: filter.Group{
			Op:         filter.And,
			Conditions: []filter.Condition{{Field: "assignee_name", Op: filter.Eq, Value: "x"}},
		},
		Sort: []filter.Order{{Field: "nonsense"}},
	})

	var fe *filter.Error
	if !errors.As(err, &fe) {
		t.Fatalf("err = %v, want a filter.Error", err)
	}
	if want := "body.filter.conditions[0].field"; fe.Faults[0].Location != want {
		t.Errorf("location = %q, want %q", fe.Faults[0].Location, want)
	}
}

func TestSortFaultsAreLocatedInsideTheBody(t *testing.T) {
	w := newWorld(t)

	_, err := w.svc.List(context.Background(), w.member, ticket.ListParams{
		Sort: []filter.Order{{Field: "nonsense"}},
	})

	var fe *filter.Error
	if !errors.As(err, &fe) {
		t.Fatalf("err = %v, want a filter.Error", err)
	}
	if want := "body.sort[0].field"; fe.Faults[0].Location != want {
		t.Errorf("location = %q, want %q", fe.Faults[0].Location, want)
	}
}
