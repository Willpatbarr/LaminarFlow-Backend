package ticket_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/filter"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/ticket"
)

// LAM-65: conflict detection. The service half - the precondition, the atomicity and what
// a refused write is told. The wire half is internal/api's ticket_conflict_test.go.

// colleague is a second member of the member's workspace, so one test can have two people
// editing the same ticket. The outsider cannot do this: they are in another workspace and
// would be refused for being unable to reach the ticket, not for conflicting.
func (w world) colleague(t *testing.T, name string) string {
	t.Helper()

	ctx := context.Background()

	var id string
	if err := w.pool.QueryRow(ctx,
		`INSERT INTO account (email, password_hash, display_name)
		 VALUES ($1 || '-' || gen_random_uuid() || '@x.com', 'h', $1) RETURNING id::text`, name,
	).Scan(&id); err != nil {
		t.Fatalf("colleague: %v", err)
	}
	if _, err := w.pool.Exec(ctx,
		`INSERT INTO workspace_member (workspace_id, account_id, role)
		 SELECT wm.workspace_id, $1::uuid, 'member' FROM workspace_member wm
		  WHERE wm.account_id = $2::uuid`, id, w.member); err != nil {
		t.Fatalf("colleague membership: %v", err)
	}

	return id
}

func version(n int) *int { return &n }

func asConflict(t *testing.T, err error) *ticket.Conflict {
	t.Helper()

	var c *ticket.Conflict
	if !errors.As(err, &c) {
		t.Fatalf("err = %v, want a *ticket.Conflict", err)
	}

	return c
}

func (w world) titleOf(t *testing.T, id string) string {
	t.Helper()

	got, err := w.svc.Get(context.Background(), w.member, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	return got.Title
}

// A new ticket is version 1, written by whoever created it.
func TestCreateStartsAtVersionOneAndNamesTheCreator(t *testing.T) {
	w := newWorld(t)
	made := w.create(t, "Fix login")

	if made.Version != 1 {
		t.Errorf("version = %d, want 1", made.Version)
	}
	if made.UpdatedBy == nil || *made.UpdatedBy != w.member {
		t.Errorf("updated_by = %v, want the creator %s", made.UpdatedBy, w.member)
	}
}

// "Exactly 1 on every successful update, archive and unarchive" - and the actor is the
// account that did the write, not the one that made the ticket.
func TestEveryWriteRaisesTheVersionByOneAndNamesItsActor(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	sarah := w.colleague(t, "Sarah")
	made := w.create(t, "Fix login")

	updated, err := w.svc.Update(ctx, sarah, ticket.UpdateParams{ID: made.ID, Title: "Fix login properly"})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Version != 2 || updated.UpdatedBy == nil || *updated.UpdatedBy != sarah {
		t.Errorf("after update: version %d by %v, want 2 by Sarah", updated.Version, updated.UpdatedBy)
	}

	archived, err := w.svc.Archive(ctx, w.member, made.ID, nil)
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	if archived.Version != 3 || archived.UpdatedBy == nil || *archived.UpdatedBy != w.member {
		t.Errorf("after archive: version %d by %v, want 3 by the member", archived.Version, archived.UpdatedBy)
	}

	restored, err := w.svc.Unarchive(ctx, sarah, made.ID, nil)
	if err != nil {
		t.Fatalf("unarchive: %v", err)
	}
	if restored.Version != 4 || restored.UpdatedBy == nil || *restored.UpdatedBy != sarah {
		t.Errorf("after unarchive: version %d by %v, want 4 by Sarah", restored.Version, restored.UpdatedBy)
	}

	// A refused or absent write moves nothing: the version is a count of writes.
	if _, err := w.svc.Update(ctx, w.outsid, ticket.UpdateParams{ID: made.ID, Title: "x"}); !errors.Is(err, ticket.ErrNotFound) {
		t.Fatalf("outsider update = %v, want ErrNotFound", err)
	}
	got, _ := w.svc.Get(ctx, w.member, made.ID)
	if got.Version != 4 {
		t.Errorf("version = %d after a refused write, want it unmoved at 4", got.Version)
	}
}

// The ticket's first test: someone else edited it, so the stale write is refused with the
// editor's copy and name, and nothing of the loser's is written.
func TestStaleUpdateIsRefusedWithTheCurrentCopyAndWhoChangedIt(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	sarah := w.colleague(t, "Sarah")
	made := w.create(t, "Fix login")

	// Sarah edits at version 1; the member, who also saw version 1, then tries to.
	theirs, err := w.svc.Update(ctx, sarah, ticket.UpdateParams{
		ID: made.ID, Title: "Sarah's title", Description: "Sarah's words", IfVersion: version(1),
	})
	if err != nil {
		t.Fatalf("sarah's update: %v", err)
	}

	_, err = w.svc.Update(ctx, w.member, ticket.UpdateParams{
		ID: made.ID, Title: "Member's title", IfVersion: version(1),
	})
	c := asConflict(t, err)

	if c.Archived {
		t.Error("Archived = true for a ticket that was only edited")
	}
	if c.Current.Version != 2 || c.Current.Title != "Sarah's title" || c.Current.Description != "Sarah's words" {
		t.Errorf("current = %+v, want Sarah's version 2", c.Current)
	}
	if c.ChangedBy == nil || *c.ChangedBy != sarah {
		t.Errorf("changed by %v, want Sarah %s", c.ChangedBy, sarah)
	}
	if !c.ChangedAt.Equal(theirs.UpdatedAt) {
		t.Errorf("changed at %v, want %v", c.ChangedAt, theirs.UpdatedAt)
	}

	if got := w.titleOf(t, made.ID); got != "Sarah's title" {
		t.Errorf("title = %q, the refused write got through", got)
	}
}

// A matching version is just a write.
func TestUpdateAtTheCurrentVersionSucceeds(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	made := w.create(t, "Fix login")

	first, err := w.svc.Update(ctx, w.member, ticket.UpdateParams{ID: made.ID, Title: "One", IfVersion: version(1)})
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := w.svc.Update(ctx, w.member, ticket.UpdateParams{ID: made.ID, Title: "Two", IfVersion: &first.Version})
	if err != nil {
		t.Fatalf("second, at the version the first returned: %v", err)
	}
	if second.Version != 3 || second.Title != "Two" {
		t.Errorf("got version %d %q, want 3 %q", second.Version, second.Title, "Two")
	}
}

// The ticket's second test: archived underneath the editor. The answer says archived - not
// merely stale - and describes the archive, and still hands back the row that no read can.
func TestUpdateAfterArchiveIsRefusedAsArchived(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	sarah := w.colleague(t, "Sarah")
	made := w.create(t, "Fix login")

	archived, err := w.svc.Archive(ctx, sarah, made.ID, version(1))
	if err != nil {
		t.Fatalf("archive: %v", err)
	}

	_, err = w.svc.Update(ctx, w.member, ticket.UpdateParams{ID: made.ID, Title: "edited offline", IfVersion: version(1)})
	c := asConflict(t, err)

	if !c.Archived {
		t.Error("Archived = false for an archived ticket")
	}
	if c.Current.ID != made.ID || c.Current.Title != "Fix login" || c.Current.Version != archived.Version {
		t.Errorf("current = %+v, want the archived row at version %d", c.Current, archived.Version)
	}
	if c.ChangedBy == nil || *c.ChangedBy != sarah {
		t.Errorf("changed by %v, want the archiver Sarah", c.ChangedBy)
	}
	if !c.ChangedAt.Equal(archived.UpdatedAt) {
		t.Errorf("changed at %v, want the archive's %v", c.ChangedAt, archived.UpdatedAt)
	}

	// Whatever version was sent: a client at the archived version is no less late.
	_, err = w.svc.Update(ctx, w.member, ticket.UpdateParams{ID: made.ID, Title: "x", IfVersion: &archived.Version})
	if !asConflict(t, err).Archived {
		t.Error("the archived version also needs to be refused as archived")
	}

	// And reads are unchanged: the 409 is the one door, not a hole.
	if _, err := w.svc.Get(ctx, w.member, made.ID); !errors.Is(err, ticket.ErrNotFound) {
		t.Errorf("get after archive = %v, want ErrNotFound", err)
	}
}

// The existing behaviour, pinned from this side: with no precondition, nothing is
// refused for being stale, and an archived ticket is still just not found.
func TestNoPreconditionStillMeansLastWriteWins(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	sarah := w.colleague(t, "Sarah")
	made := w.create(t, "Fix login")

	if _, err := w.svc.Update(ctx, sarah, ticket.UpdateParams{ID: made.ID, Title: "Sarah's"}); err != nil {
		t.Fatalf("sarah: %v", err)
	}
	got, err := w.svc.Update(ctx, w.member, ticket.UpdateParams{ID: made.ID, Title: "Member's"})
	if err != nil {
		t.Fatalf("an update with no precondition after someone else's: %v", err)
	}
	if got.Title != "Member's" || got.Version != 3 {
		t.Errorf("got %q at %d, want the overwrite at version 3", got.Title, got.Version)
	}

	if _, err := w.svc.Archive(ctx, sarah, made.ID, nil); err != nil {
		t.Fatalf("archive: %v", err)
	}
	_, err = w.svc.Update(ctx, w.member, ticket.UpdateParams{ID: made.ID, Title: "late"})
	if !errors.Is(err, ticket.ErrNotFound) {
		t.Errorf("update of an archived ticket with no precondition = %v, want ErrNotFound as before", err)
	}
	var c *ticket.Conflict
	if errors.As(err, &c) {
		t.Error("a request with no precondition was handed a Conflict")
	}
}

// The ticket's third test: the check and the write are one statement. N writers all hold
// version 1; exactly one may win, every other is told it lost, and the row ends at 2.
func TestConcurrentUpdatesFromTheSameVersionLetExactlyOneWin(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	made := w.create(t, "Fix login")

	const writers = 16

	var (
		start   = make(chan struct{})
		wg      sync.WaitGroup
		results = make([]error, writers)
		written = make([]ticket.Ticket, writers)
	)

	for i := range writers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			<-start
			written[i], results[i] = w.svc.Update(ctx, w.member, ticket.UpdateParams{
				ID: made.ID, Title: fmt.Sprintf("writer %d", i), IfVersion: version(1),
			})
		}()
	}

	close(start)
	wg.Wait()

	winner := -1

	for i, err := range results {
		switch {
		case err == nil:
			if winner != -1 {
				t.Fatalf("writers %d and %d both won from version 1", winner, i)
			}
			winner = i
		default:
			var c *ticket.Conflict
			if !errors.As(err, &c) {
				t.Fatalf("writer %d: %v, want a conflict or the win", i, err)
			}
			if c.Archived || c.Current.Version != 2 {
				t.Errorf("writer %d was told %+v, want a plain conflict against version 2", i, c)
			}
		}
	}
	if winner == -1 {
		t.Fatal("no writer won")
	}

	got, err := w.svc.Get(ctx, w.member, made.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Version != 2 {
		t.Errorf("version = %d, want 2: exactly one write landed", got.Version)
	}
	if want := fmt.Sprintf("writer %d", winner); got.Title != want {
		t.Errorf("title = %q, want the winner's %q", got.Title, want)
	}
	// The losers returned before reindexing; the winner's index row is the only one.
	if n := w.indexRows(t, made.ID); n != 1 {
		t.Errorf("index rows = %d, want 1", n)
	}
}

// An archive racing an edit from the same version is the same contest across two verbs:
// one lands, the other is told what the winner did.
func TestAnArchiveAndAnUpdateFromTheSameVersionCannotBothLand(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	made := w.create(t, "Fix login")

	var (
		start      = make(chan struct{})
		wg         sync.WaitGroup
		updateErr  error
		archiveErr error
	)

	wg.Add(2)

	go func() {
		defer wg.Done()
		<-start
		_, updateErr = w.svc.Update(ctx, w.member, ticket.UpdateParams{ID: made.ID, Title: "edited", IfVersion: version(1)})
	}()
	go func() {
		defer wg.Done()
		<-start
		_, archiveErr = w.svc.Archive(ctx, w.member, made.ID, version(1))
	}()

	close(start)
	wg.Wait()

	if (updateErr == nil) == (archiveErr == nil) {
		t.Fatalf("update err = %v, archive err = %v, want exactly one to win", updateErr, archiveErr)
	}

	var final int
	if err := w.pool.QueryRow(ctx, `SELECT version FROM ticket WHERE id = $1::uuid`, made.ID).Scan(&final); err != nil {
		t.Fatalf("read version: %v", err)
	}
	if final != 2 {
		t.Errorf("version = %d, want 2", final)
	}

	if updateErr != nil {
		// The archive won, so the edit is told it is archived.
		if !asConflict(t, updateErr).Archived {
			t.Error("the losing update was not told the ticket is archived")
		}
	} else if asConflict(t, archiveErr).Archived {
		// The edit won, so the archive is told the ticket changed, not that it is archived.
		t.Error("the losing archive was told the ticket is archived, but it was edited")
	}
}

// Archive carries the same precondition, and still does the search work.
func TestArchiveWithAStaleVersionIsRefusedAndAnArchiveAtTheRightOneWorks(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	sarah := w.colleague(t, "Sarah")
	made := w.create(t, "Fix login")

	if _, err := w.svc.Update(ctx, sarah, ticket.UpdateParams{ID: made.ID, Title: "Sarah's"}); err != nil {
		t.Fatalf("update: %v", err)
	}

	_, err := w.svc.Archive(ctx, w.member, made.ID, version(1))
	c := asConflict(t, err)
	if c.Archived || c.Current.Version != 2 || c.ChangedBy == nil || *c.ChangedBy != sarah {
		t.Errorf("stale archive was told %+v, want an edit by Sarah at version 2", c)
	}
	if n := w.indexRows(t, made.ID); n != 1 {
		t.Errorf("index rows after a refused archive = %d, want the ticket still searchable", n)
	}

	// Archiving at the right version clears the search row, and the restore puts it back:
	// the archive tax is unchanged by the precondition.
	archived, err := w.svc.Archive(ctx, w.member, made.ID, version(2))
	if err != nil {
		t.Fatalf("archive at version 2: %v", err)
	}
	if n := w.indexRows(t, made.ID); n != 0 {
		t.Errorf("index rows after archive = %d, want 0", n)
	}

	restored, err := w.svc.Unarchive(ctx, w.member, made.ID, &archived.Version)
	if err != nil {
		t.Fatalf("unarchive at the archived version: %v", err)
	}
	if restored.Version != 4 {
		t.Errorf("version = %d, want 4", restored.Version)
	}
	if n := w.indexRows(t, made.ID); n != 1 {
		t.Errorf("index rows after unarchive = %d, want 1", n)
	}
}

// Archiving a ticket twice with a version says archived (and who), which is also what a
// client sees when its own archive landed and the response did not.
func TestArchivingAnArchivedTicketWithAVersionNamesTheArchiver(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	made := w.create(t, "Fix login")

	if _, err := w.svc.Archive(ctx, w.member, made.ID, version(1)); err != nil {
		t.Fatalf("archive: %v", err)
	}

	_, err := w.svc.Archive(ctx, w.member, made.ID, version(1))
	c := asConflict(t, err)
	if !c.Archived || c.ChangedBy == nil || *c.ChangedBy != w.member {
		t.Errorf("retry was told %+v, want archived by the member", c)
	}

	// Without a version it is the not-found it has always been.
	if _, err := w.svc.Archive(ctx, w.member, made.ID, nil); !errors.Is(err, ticket.ErrNotFound) {
		t.Errorf("archive twice with no version = %v, want ErrNotFound", err)
	}
}

func TestUnarchiveWithAVersionIsConsistentWithTheOthers(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	sarah := w.colleague(t, "Sarah")
	made := w.create(t, "Fix login")

	archived, err := w.svc.Archive(ctx, w.member, made.ID, nil)
	if err != nil {
		t.Fatalf("archive: %v", err)
	}

	// Sarah restored it and edited it while the member was offline looking at the archived
	// copy: the member's restore is stale, and is told what is there.
	restored, err := w.svc.Unarchive(ctx, sarah, made.ID, &archived.Version)
	if err != nil {
		t.Fatalf("sarah's unarchive: %v", err)
	}
	if _, err := w.svc.Update(ctx, sarah, ticket.UpdateParams{ID: made.ID, Title: "Sarah's"}); err != nil {
		t.Fatalf("sarah's update: %v", err)
	}

	_, err = w.svc.Unarchive(ctx, w.member, made.ID, &archived.Version)
	c := asConflict(t, err)
	if c.Archived || c.Current.Version != restored.Version+1 || c.ChangedBy == nil || *c.ChangedBy != sarah {
		t.Errorf("stale unarchive was told %+v, want Sarah's edit at version %d", c, restored.Version+1)
	}

	// A live ticket at exactly the version sent has nothing to restore: the same answer as
	// the request without a version.
	if _, err := w.svc.Unarchive(ctx, w.member, made.ID, &c.Current.Version); !errors.Is(err, ticket.ErrNotFound) {
		t.Errorf("unarchive of a live ticket at its own version = %v, want ErrNotFound", err)
	}
}

// A 409 hands out a ticket, so it must not hand out one the caller cannot reach.
func TestAConflictNeverRevealsATicketTheCallerCannotReach(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	made := w.create(t, "Fix login")

	if _, err := w.svc.Update(ctx, w.member, ticket.UpdateParams{ID: made.ID, Title: "edited"}); err != nil {
		t.Fatalf("update: %v", err)
	}

	// Stale version, and from another workspace.
	if _, err := w.svc.Update(ctx, w.outsid, ticket.UpdateParams{ID: made.ID, Title: "x", IfVersion: version(1)}); !errors.Is(err, ticket.ErrNotFound) {
		t.Errorf("outsider update = %v, want ErrNotFound and no copy of the ticket", err)
	}
	if _, err := w.svc.Archive(ctx, w.outsid, made.ID, version(1)); !errors.Is(err, ticket.ErrNotFound) {
		t.Errorf("outsider archive = %v, want ErrNotFound", err)
	}
}

// A ticket from before the migration has no recorded writer. The conflict says so with a
// nil rather than a made-up name, and an account that has since left reads the same way.
func TestAConflictWithAnUnrecordedWriterHasNoName(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	sarah := w.colleague(t, "Sarah")
	made := w.create(t, "Fix login")

	if _, err := w.svc.Update(ctx, sarah, ticket.UpdateParams{ID: made.ID, Title: "Sarah's"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, err := w.pool.Exec(ctx, `DELETE FROM account WHERE id = $1::uuid`, sarah); err != nil {
		t.Fatalf("delete sarah: %v", err)
	}

	_, err := w.svc.Update(ctx, w.member, ticket.UpdateParams{ID: made.ID, Title: "x", IfVersion: version(1)})
	c := asConflict(t, err)
	if c.ChangedBy != nil {
		t.Errorf("changed by %v, want nil for a writer who no longer exists", *c.ChangedBy)
	}
	if c.Current.UpdatedBy != nil {
		t.Errorf("current.updated_by = %v, want nil", *c.Current.UpdatedBy)
	}
	if c.Current.Title != "Sarah's" {
		t.Errorf("current title = %q: the copy must survive its author", c.Current.Title)
	}
}

// List shares Get's columns, so a client hydrating from it has versions to send. And since
// "changed since" and "version" are on the vocabulary, they filter.
func TestListCarriesVersionsAndFiltersOnThem(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	untouched := w.create(t, "Untouched")
	edited := w.create(t, "Edited")

	if _, err := w.svc.Update(ctx, w.member, ticket.UpdateParams{ID: edited.ID, Title: "Edited again"}); err != nil {
		t.Fatalf("update: %v", err)
	}

	page, err := w.svc.List(ctx, w.member, ticket.ListParams{
		Filter: filter.Group{Op: filter.And, Conditions: []filter.Condition{
			{Field: "version", Op: filter.Gt, Value: float64(1)},
		}},
		Sort: []filter.Order{{Field: "version", Desc: true}},
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.Tickets) != 1 || page.Tickets[0].ID != edited.ID {
		t.Fatalf("version > 1 returned %+v, want only the edited ticket", page.Tickets)
	}
	if got := page.Tickets[0]; got.Version != 2 || got.UpdatedBy == nil || *got.UpdatedBy != w.member {
		t.Errorf("listed %+v, want version 2 by the member", got)
	}

	// Changed since: everything touched after the untouched ticket was created.
	page, err = w.svc.List(ctx, w.member, ticket.ListParams{
		Filter: filter.Group{Op: filter.And, Conditions: []filter.Condition{
			{Field: "updated_at", Op: filter.Gt, Value: untouched.UpdatedAt.Format("2006-01-02T15:04:05.999999999Z07:00")},
		}},
	})
	if err != nil {
		t.Fatalf("list by updated_at: %v", err)
	}
	if len(page.Tickets) != 1 || page.Tickets[0].ID != edited.ID {
		t.Errorf("updated_at > untouched.updated_at returned %d tickets, want just the edited one", len(page.Tickets))
	}
}
