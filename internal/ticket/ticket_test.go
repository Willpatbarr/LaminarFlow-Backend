package ticket_test

import (
	"context"
	"errors"
	"log"
	"os"
	"strings"
	"testing"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/dbtest"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/migrate"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/ticket"
	"github.com/Willpatbarr/LaminarFlow-Backend/migrations"

	"github.com/jackc/pgx/v5/pgxpool"
)

// An external test package so these can read search_index to prove archiving takes a
// ticket out of search. internal/ticket itself never touches that table.

var testDatabaseURL string

func TestMain(m *testing.M) { os.Exit(run(m)) }

func run(m *testing.M) int {
	ctx := context.Background()

	dsn, cleanup, err := dbtest.Create(ctx)
	if errors.Is(err, dbtest.ErrNoDatabase) {
		return m.Run()
	}
	if err != nil {
		log.Printf("%v", err)
		return 1
	}
	defer cleanup()

	testDatabaseURL = dsn

	loaded, err := migrate.Load(migrations.FS)
	if err != nil {
		log.Printf("load migrations: %v", err)
		return 1
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Printf("connect: %v", err)
		return 1
	}
	if _, err := migrate.Up(ctx, pool, loaded); err != nil {
		pool.Close()
		log.Printf("apply migrations: %v", err)
		return 1
	}
	pool.Close()

	return m.Run()
}

// world is two workspaces with a member each, which is what every scoping assertion
// below needs: a caller who should see a ticket and one who should not.
type world struct {
	pool           *pgxpool.Pool
	svc            *ticket.Service
	member, outsid string
	project        string
}

func newWorld(t *testing.T) world {
	t.Helper()

	if testDatabaseURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testDatabaseURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	for _, table := range []string{"team", "account"} {
		if _, err := pool.Exec(ctx, `DELETE FROM `+table); err != nil {
			t.Fatalf("reset %s: %v", table, err)
		}
	}

	one := func(q string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, q, args...).Scan(&id); err != nil {
			t.Fatalf("fixture: %v", err)
		}
		return id
	}

	ws := one(`SELECT id::text FROM workspace WHERE name = 'Default'`)
	other := one(`INSERT INTO workspace (name) VALUES ('Other') RETURNING id::text`)

	w := world{pool: pool, svc: ticket.NewService(pool)}
	w.member = one(`INSERT INTO account (email, password_hash, display_name)
	                VALUES ('member-' || gen_random_uuid() || '@x.com', 'h', 'Member')
	                RETURNING id::text`)
	w.outsid = one(`INSERT INTO account (email, password_hash, display_name)
	                VALUES ('outsider-' || gen_random_uuid() || '@x.com', 'h', 'Outsider')
	                RETURNING id::text`)

	one(`INSERT INTO workspace_member (workspace_id, account_id, role)
	     VALUES ($1::uuid, $2::uuid, 'member') RETURNING account_id::text`, ws, w.member)
	// The outsider is a real member of somewhere else, so these tests prove scoping
	// rather than merely proving that an account with no memberships sees nothing.
	one(`INSERT INTO workspace_member (workspace_id, account_id, role)
	     VALUES ($1::uuid, $2::uuid, 'member') RETURNING account_id::text`, other, w.outsid)

	team := one(`INSERT INTO team (workspace_id, name) VALUES ($1::uuid, 'Platform')
	             RETURNING id::text`, ws)
	w.project = one(`INSERT INTO project (team_id, name) VALUES ($1::uuid, 'Core')
	                 RETURNING id::text`, team)

	return w
}

func (w world) create(t *testing.T, title string) ticket.Ticket {
	t.Helper()

	got, err := w.svc.Create(context.Background(), w.member, ticket.CreateParams{
		ProjectID: w.project, Title: title, Description: "why it matters",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	return got
}

func (w world) indexRows(t *testing.T, ticketID string) int {
	t.Helper()

	var n int
	if err := w.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM search_index WHERE ticket_id = $1::uuid`, ticketID).Scan(&n); err != nil {
		t.Fatalf("count index rows: %v", err)
	}

	return n
}

func TestCreateThenGet(t *testing.T) {
	w := newWorld(t)
	made := w.create(t, "Fix login")

	got, err := w.svc.Get(context.Background(), w.member, made.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Title != "Fix login" || got.ProjectID != w.project {
		t.Errorf("got %+v, want the ticket just created", got)
	}
	// Null is the normal case for both, per 0011 - not an edge case to be surprised by.
	if got.StatusID != nil || got.AssigneeID != nil {
		t.Errorf("status %v assignee %v, want both null", got.StatusID, got.AssigneeID)
	}
}

// The scoping predicate, from both sides. A member sees it; someone who is a member of
// a different workspace does not, and is told only that it is absent.
func TestScopingHidesOtherWorkspacesTickets(t *testing.T) {
	w := newWorld(t)
	made := w.create(t, "Fix login")
	ctx := context.Background()

	if _, err := w.svc.Get(ctx, w.outsid, made.ID); !errors.Is(err, ticket.ErrNotFound) {
		t.Errorf("get as an outsider = %v, want ErrNotFound", err)
	}
	if _, err := w.svc.Update(ctx, w.outsid, ticket.UpdateParams{ID: made.ID, Title: "hijacked"}); !errors.Is(err, ticket.ErrNotFound) {
		t.Errorf("update as an outsider = %v, want ErrNotFound", err)
	}
	if err := w.svc.Archive(ctx, w.outsid, made.ID); !errors.Is(err, ticket.ErrNotFound) {
		t.Errorf("archive as an outsider = %v, want ErrNotFound", err)
	}

	// And the ticket is untouched by all that.
	got, err := w.svc.Get(ctx, w.member, made.ID)
	if err != nil {
		t.Fatalf("get as the member: %v", err)
	}
	if got.Title != "Fix login" {
		t.Errorf("title = %q, an outsider changed it", got.Title)
	}
}

// Creating into a project the caller cannot reach must not create anything.
func TestCreateIntoAnUnreachableProjectCreatesNothing(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	if _, err := w.svc.Create(ctx, w.outsid, ticket.CreateParams{
		ProjectID: w.project, Title: "should not exist",
	}); !errors.Is(err, ticket.ErrNotFound) {
		t.Fatalf("create as an outsider = %v, want ErrNotFound", err)
	}

	var n int
	if err := w.pool.QueryRow(ctx,
		`SELECT count(*) FROM ticket WHERE title = 'should not exist'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("%d tickets created by a caller who cannot reach the project", n)
	}
}

// The column has a DEFAULT and no trigger, so nothing moves updated_at unless Update
// sets it. Without that every updated_at is a creation timestamp under another name.
func TestUpdateMovesUpdatedAt(t *testing.T) {
	w := newWorld(t)
	made := w.create(t, "Fix login")

	got, err := w.svc.Update(context.Background(), w.member, ticket.UpdateParams{
		ID: made.ID, Title: "Fix login properly",
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	if !got.UpdatedAt.After(made.UpdatedAt) {
		t.Errorf("updated_at %v did not move past %v", got.UpdatedAt, made.UpdatedAt)
	}
	if !got.CreatedAt.Equal(made.CreatedAt) {
		t.Errorf("created_at moved from %v to %v", made.CreatedAt, got.CreatedAt)
	}
}

// PUT semantics: an omitted field is cleared, not left alone. Asserted because the
// opposite is what a reader assumes, and the difference silently unassigns people.
func TestUpdateReplacesRatherThanPatches(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	made := w.create(t, "Fix login")

	assignee := w.member
	if _, err := w.svc.Update(ctx, w.member, ticket.UpdateParams{
		ID: made.ID, Title: "Fix login", Description: "d", AssigneeID: &assignee,
	}); err != nil {
		t.Fatalf("assign: %v", err)
	}

	got, err := w.svc.Update(ctx, w.member, ticket.UpdateParams{ID: made.ID, Title: "Fix login"})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if got.AssigneeID != nil {
		t.Errorf("assignee = %v, want cleared - this is a replace, not a patch", *got.AssigneeID)
	}
	if got.Description != "" {
		t.Errorf("description = %q, want cleared", got.Description)
	}
}

// The whole argument for archive over delete: the row and its comments survive.
func TestArchiveKeepsTheRowAndItsComments(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	made := w.create(t, "Fix login")

	if _, err := w.pool.Exec(ctx,
		`INSERT INTO comment (ticket_id, body) VALUES ($1::uuid, 'Reproduced')`, made.ID); err != nil {
		t.Fatalf("comment: %v", err)
	}

	if err := w.svc.Archive(ctx, w.member, made.ID); err != nil {
		t.Fatalf("archive: %v", err)
	}

	var rows, comments int
	if err := w.pool.QueryRow(ctx,
		`SELECT count(*) FROM ticket WHERE id = $1::uuid`, made.ID).Scan(&rows); err != nil {
		t.Fatalf("count ticket: %v", err)
	}
	if err := w.pool.QueryRow(ctx,
		`SELECT count(*) FROM comment WHERE ticket_id = $1::uuid`, made.ID).Scan(&comments); err != nil {
		t.Fatalf("count comments: %v", err)
	}

	if rows != 1 {
		t.Errorf("ticket rows = %d, want the row kept", rows)
	}
	if comments != 1 {
		t.Errorf("comments = %d, want the discussion kept - this is the reason for archive", comments)
	}
}

// ...and it is gone from every read.
func TestArchivedTicketsAreInvisible(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	made := w.create(t, "Fix login")

	if err := w.svc.Archive(ctx, w.member, made.ID); err != nil {
		t.Fatalf("archive: %v", err)
	}

	if _, err := w.svc.Get(ctx, w.member, made.ID); !errors.Is(err, ticket.ErrNotFound) {
		t.Errorf("get = %v, want an archived ticket hidden", err)
	}
	if _, err := w.svc.Update(ctx, w.member, ticket.UpdateParams{ID: made.ID, Title: "x"}); !errors.Is(err, ticket.ErrNotFound) {
		t.Errorf("update = %v, want an archived ticket unwritable", err)
	}
}

// The half of 0029 that is easy to miss. Archiving sets a column; without the index
// write the ticket keeps answering searches.
func TestArchivingRemovesTheTicketFromSearch(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	made := w.create(t, "Fix login")

	if n := w.indexRows(t, made.ID); n != 1 {
		t.Fatalf("index rows after create = %d, want 1", n)
	}

	if err := w.svc.Archive(ctx, w.member, made.ID); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if n := w.indexRows(t, made.ID); n != 0 {
		t.Errorf("index rows after archive = %d, want 0 - an archived ticket is still searchable", n)
	}

	if err := w.svc.Unarchive(ctx, w.member, made.ID); err != nil {
		t.Fatalf("unarchive: %v", err)
	}
	if n := w.indexRows(t, made.ID); n != 1 {
		t.Errorf("index rows after unarchive = %d, want 1", n)
	}
}

// Unarchive is what makes archive an undo rather than a slower delete.
func TestUnarchiveRestoresTheTicket(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	made := w.create(t, "Fix login")

	if err := w.svc.Archive(ctx, w.member, made.ID); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if err := w.svc.Unarchive(ctx, w.member, made.ID); err != nil {
		t.Fatalf("unarchive: %v", err)
	}

	got, err := w.svc.Get(ctx, w.member, made.ID)
	if err != nil {
		t.Fatalf("get after unarchive: %v", err)
	}
	if got.Title != "Fix login" {
		t.Errorf("title = %q, want it restored intact", got.Title)
	}
}

// Both directions refuse a no-op rather than silently moving the timestamp, so a
// double-archive cannot quietly overwrite when the first one happened.
func TestArchiveAndUnarchiveRefuseNoOps(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	made := w.create(t, "Fix login")

	if err := w.svc.Unarchive(ctx, w.member, made.ID); !errors.Is(err, ticket.ErrNotFound) {
		t.Errorf("unarchive a live ticket = %v, want ErrNotFound", err)
	}
	if err := w.svc.Archive(ctx, w.member, made.ID); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if err := w.svc.Archive(ctx, w.member, made.ID); !errors.Is(err, ticket.ErrNotFound) {
		t.Errorf("archive twice = %v, want ErrNotFound", err)
	}
}

// Editing a title has to reindex, or search keeps answering with the old one - title
// is denormalised into search_index.
func TestUpdatingATitleReindexesIt(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	made := w.create(t, "Fix login")

	if _, err := w.svc.Update(ctx, w.member, ticket.UpdateParams{
		ID: made.ID, Title: "Fix logout",
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	var title, content string
	if err := w.pool.QueryRow(ctx,
		`SELECT title_or_preview, content FROM search_index WHERE ticket_id = $1::uuid`,
		made.ID).Scan(&title, &content); err != nil {
		t.Fatalf("read index: %v", err)
	}

	if title != "Fix logout" {
		t.Errorf("indexed title = %q, want the edited one", title)
	}
	if !strings.Contains(content, "Fix logout") {
		t.Errorf("indexed content = %q, want the edited title", content)
	}
	if n := w.indexRows(t, made.ID); n != 1 {
		t.Errorf("index rows = %d, want exactly 1 - the old row was not cleared", n)
	}
}
