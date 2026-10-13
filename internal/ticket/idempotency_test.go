package ticket_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/ticket"
)

// LAM-75. Idempotent create at the service: one ticket per (creator, key), whatever the
// order or the concurrency. The wire half - the header, the change feed - is internal/api's
// idempotency_test.go.

func (w world) keyed(key string) ticket.CreateParams {
	return ticket.CreateParams{ProjectID: w.project, Title: "Fix login", Description: "d", IdempotencyKey: &key}
}

func TestTheSameKeyTwiceIsOneTicket(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	first, replayed, err := w.svc.CreateIdempotent(ctx, w.member, w.keyed("k1"))
	if err != nil || replayed {
		t.Fatalf("first: replayed=%v err=%v", replayed, err)
	}

	second, replayed, err := w.svc.CreateIdempotent(ctx, w.member, w.keyed("k1"))
	if err != nil || !replayed {
		t.Fatalf("second: replayed=%v err=%v", replayed, err)
	}
	if second.ID != first.ID {
		t.Errorf("second id %s, want the original %s", second.ID, first.ID)
	}
	if n := w.rows(t, `SELECT count(*) FROM ticket`); n != 1 {
		t.Errorf("%d tickets, want 1", n)
	}
	if n := w.indexRows(t, first.ID); n == 0 {
		t.Error("the original is not indexed")
	}
}

// A replay writes nothing: not the version, not the search row.
func TestAReplayDoesNotReindexOrTouchTheTicket(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	first, _, _ := w.svc.CreateIdempotent(ctx, w.member, w.keyed("k1"))
	before := w.indexRows(t, first.ID)

	// Change the title behind the service's back, so the index (which was built from the
	// old one) and the row disagree. Anything that reindexed would now write the new text
	// into the index; a replay must leave it exactly as the first create made it.
	if _, err := w.pool.Exec(ctx, `UPDATE ticket SET title = 'changed behind the index' WHERE id = $1::uuid`, first.ID); err != nil {
		t.Fatalf("change: %v", err)
	}

	second, replayed, err := w.svc.CreateIdempotent(ctx, w.member, w.keyed("k1"))
	if err != nil || !replayed {
		t.Fatalf("replay: %v %v", replayed, err)
	}
	if second.Version != 1 || !second.UpdatedAt.Equal(first.UpdatedAt) {
		t.Errorf("replay changed the ticket: %+v -> %+v", first, second)
	}

	if after := w.indexRows(t, first.ID); after != before {
		t.Errorf("%d index rows after the replay, %d before", after, before)
	}
	if n := w.rows(t, `SELECT count(*) FROM search_index WHERE ticket_id = $1::uuid AND content = 'changed behind the index'`, first.ID); n != 0 {
		t.Errorf("%d index rows carry the new title: the replay reindexed", n)
	}
}

// The original, as it is now - not as it was.
func TestAReplayReturnsTheCurrentStateAndIgnoresItsOwnBody(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	first, _, _ := w.svc.CreateIdempotent(ctx, w.member, w.keyed("k1"))
	if _, err := w.svc.Update(ctx, w.member, ticket.UpdateParams{ID: first.ID, Title: "Edited since"}); err != nil {
		t.Fatalf("update: %v", err)
	}

	different := w.keyed("k1")
	different.Title = "A different title entirely"
	got, replayed, err := w.svc.CreateIdempotent(ctx, w.member, different)
	if err != nil || !replayed {
		t.Fatalf("replay: %v %v", replayed, err)
	}
	if got.ID != first.ID || got.Title != "Edited since" || got.Version != 2 {
		t.Errorf("replay = %+v, want the original ticket at its current state", got)
	}
}

// A used key answers before the body is looked at, so a retry of a create that succeeded
// is not turned into a 422 by a status that was deleted in between.
func TestAReplayWinsOverAStatusThatNoLongerExists(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	status := w.statusIn(t, w.project)

	p := w.keyed("k1")
	p.StatusID = &status
	first, _, err := w.svc.CreateIdempotent(ctx, w.member, p)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := w.pool.Exec(ctx, `DELETE FROM status WHERE id = $1::uuid`, status); err != nil {
		t.Fatalf("delete status: %v", err)
	}

	got, replayed, err := w.svc.CreateIdempotent(ctx, w.member, p)
	if err != nil || !replayed || got.ID != first.ID {
		t.Errorf("replay: %+v replayed=%v err=%v", got, replayed, err)
	}
}

func TestTheSameKeyFromAnotherAccountIsAnotherTicket(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	colleague := w.colleague(t, "sarah")

	mine, _, err := w.svc.CreateIdempotent(ctx, w.member, w.keyed("shared"))
	if err != nil {
		t.Fatalf("mine: %v", err)
	}
	theirs, replayed, err := w.svc.CreateIdempotent(ctx, colleague, w.keyed("shared"))
	if err != nil || replayed {
		t.Fatalf("theirs: replayed=%v err=%v", replayed, err)
	}
	if theirs.ID == mine.ID {
		t.Error("one account's key returned another account's ticket")
	}
	if n := w.rows(t, `SELECT count(*) FROM ticket`); n != 2 {
		t.Errorf("%d tickets, want 2", n)
	}
}

func TestNoKeyIsNeverAReplay(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	for range 2 {
		if _, replayed, err := w.svc.CreateIdempotent(ctx, w.member,
			ticket.CreateParams{ProjectID: w.project, Title: "same"}); err != nil || replayed {
			t.Fatalf("replayed=%v err=%v", replayed, err)
		}
	}
	if n := w.rows(t, `SELECT count(*) FROM ticket`); n != 2 {
		t.Errorf("%d tickets, want 2", n)
	}
}

// 16 requests, one key, released together: exactly one ticket, and all 16 got its id.
func TestConcurrentCreatesWithOneKeyMakeOneTicket(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	const n = 16
	var (
		wg       sync.WaitGroup
		start    = make(chan struct{})
		mu       sync.Mutex
		ids      = map[string]int{}
		replays  int
		failures []error
	)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			got, replayed, err := w.svc.CreateIdempotent(ctx, w.member, w.keyed("race"))

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failures = append(failures, err)
				return
			}
			ids[got.ID]++
			if replayed {
				replays++
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(failures) > 0 {
		t.Fatalf("%d of %d creates failed: %v", len(failures), n, failures[0])
	}
	if len(ids) != 1 {
		t.Errorf("%d distinct ticket ids returned: %v", len(ids), ids)
	}
	if replays != n-1 {
		t.Errorf("%d replays, want %d (exactly one create wrote)", replays, n-1)
	}
	if got := w.rows(t, `SELECT count(*) FROM ticket`); got != 1 {
		t.Errorf("%d tickets stored, want 1", got)
	}
	if got := w.rows(t, `SELECT count(*) FROM search_index`); got != 1 {
		t.Errorf("%d search rows, want 1", got)
	}
}

// Archived since: a Conflict that carries the ticket, never a second ticket and never
// the archived row passed off as live.
func TestAReplayAfterArchiveIsAConflictCarryingTheTicket(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	colleague := w.colleague(t, "sarah")

	first, _, _ := w.svc.CreateIdempotent(ctx, w.member, w.keyed("k1"))
	archived, err := w.svc.Archive(ctx, colleague, first.ID, nil)
	if err != nil {
		t.Fatalf("archive: %v", err)
	}

	_, replayed, err := w.svc.CreateIdempotent(ctx, w.member, w.keyed("k1"))
	c := asConflict(t, err)
	if replayed || !c.Archived || !c.Replay {
		t.Errorf("replayed=%v conflict=%+v, want an archived replay conflict", replayed, c)
	}
	if c.Current.ID != first.ID || c.Current.Version != archived.Version {
		t.Errorf("current = %+v, want the archived ticket at version %d", c.Current, archived.Version)
	}
	if c.ChangedBy == nil || *c.ChangedBy != colleague {
		t.Errorf("changed_by = %v, want the archiver", c.ChangedBy)
	}
	if n := w.rows(t, `SELECT count(*) FROM ticket`); n != 1 {
		t.Errorf("%d tickets, want 1", n)
	}

	// Restored, the same key replays as live again.
	if _, err := w.svc.Unarchive(ctx, w.member, first.ID, nil); err != nil {
		t.Fatalf("unarchive: %v", err)
	}
	if got, replayed, err := w.svc.CreateIdempotent(ctx, w.member, w.keyed("k1")); err != nil || !replayed || got.ID != first.ID {
		t.Errorf("after restore: %+v replayed=%v err=%v", got, replayed, err)
	}
}

// The key never lets an account reach a ticket it otherwise could not.
func TestAReplayByAnAccountThatLostAccessIsNotFound(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	colleague := w.colleague(t, "sarah")

	if _, _, err := w.svc.CreateIdempotent(ctx, colleague, w.keyed("k1")); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := w.pool.Exec(ctx, `DELETE FROM workspace_member WHERE account_id = $1::uuid`, colleague); err != nil {
		t.Fatalf("remove: %v", err)
	}

	if _, _, err := w.svc.CreateIdempotent(ctx, colleague, w.keyed("k1")); !errors.Is(err, ticket.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// Deleting the creator must still work, and leaves the ticket and its key behind. This is
// the trap in migrations/README.md: a CHECK tying the key to the creator would refuse it.
func TestDeletingTheCreatorLeavesTheTicket(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	colleague := w.colleague(t, "sarah")

	first, _, err := w.svc.CreateIdempotent(ctx, colleague, w.keyed("k1"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := w.pool.Exec(ctx, `DELETE FROM account WHERE id = $1::uuid`, colleague); err != nil {
		t.Fatalf("delete account: %v", err)
	}

	var createdBy *string
	if err := w.pool.QueryRow(ctx, `SELECT created_by::text FROM ticket WHERE id = $1::uuid`, first.ID).Scan(&createdBy); err != nil {
		t.Fatalf("read: %v", err)
	}
	if createdBy != nil {
		t.Errorf("created_by = %v, want null after the account is gone", *createdBy)
	}
}

// created_by is recorded on every ticket, keyed or not.
func TestEveryNewTicketRecordsItsCreator(t *testing.T) {
	w := newWorld(t)
	made := w.create(t, "Fix login")

	var createdBy string
	if err := w.pool.QueryRow(context.Background(),
		`SELECT created_by::text FROM ticket WHERE id = $1::uuid`, made.ID).Scan(&createdBy); err != nil || createdBy != w.member {
		t.Errorf("created_by = %q (%v), want %s", createdBy, err, w.member)
	}
}
