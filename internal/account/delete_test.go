package account_test

import (
	"context"
	"errors"
	"log"
	"os"
	"testing"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/account"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/dbtest"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/migrate"
	"github.com/Willpatbarr/LaminarFlow-Backend/migrations"

	"github.com/jackc/pgx/v5/pgxpool"
)

// An external test package so these can read saved_view, comment and ticket to prove
// what a deletion did and did not take. internal/account names only account.

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

type world struct {
	pool          *pgxpool.Pool
	svc           *account.Service
	leaver, stays string
	team, project string
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

	w := world{pool: pool, svc: account.NewService(pool)}
	ws := one(`SELECT id::text FROM workspace WHERE name = 'Default'`)

	w.leaver = one(`INSERT INTO account (email, password_hash, display_name)
	                VALUES ('leaver-' || gen_random_uuid() || '@x.com', 'h', 'Leaver')
	                RETURNING id::text`)
	w.stays = one(`INSERT INTO account (email, password_hash, display_name)
	               VALUES ('stays-' || gen_random_uuid() || '@x.com', 'h', 'Stays')
	               RETURNING id::text`)

	for _, id := range []string{w.leaver, w.stays} {
		one(`INSERT INTO workspace_member (workspace_id, account_id, role)
		     VALUES ($1::uuid, $2::uuid, 'member') RETURNING account_id::text`, ws, id)
	}

	w.team = one(`INSERT INTO team (workspace_id, name) VALUES ($1::uuid, 'Platform')
	              RETURNING id::text`, ws)
	w.project = one(`INSERT INTO project (team_id, name) VALUES ($1::uuid, 'Core')
	                 RETURNING id::text`, w.team)

	return w
}

func (w world) newView(t *testing.T, owner, name string, shared bool) string {
	t.Helper()

	var id string
	err := w.pool.QueryRow(context.Background(),
		`INSERT INTO saved_view (team_id, owner_account_id, name, layout, is_shared)
		 VALUES ($1::uuid, $2::uuid, $3, 'list', $4) RETURNING id::text`,
		w.team, owner, name, shared).Scan(&id)
	if err != nil {
		t.Fatalf("fixture view %q: %v", name, err)
	}

	return id
}

func (w world) count(t *testing.T, query string, args ...any) int {
	t.Helper()

	var n int
	if err := w.pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}

	return n
}

// LAM-51, and the reason the ticket exists. A private view left behind is owned by
// nobody and visible to nobody: it cannot be listed, edited or deleted through any
// interface, because every one of those is scoped by an owner it no longer has.
func TestDeletingAnAccountTakesItsPrivateViewsWithIt(t *testing.T) {
	w := newWorld(t)

	mine := w.newView(t, w.leaver, "My drafts", false)
	alsoMine := w.newView(t, w.leaver, "My blockers", false)
	theirs := w.newView(t, w.stays, "Their drafts", false)

	removal, err := w.svc.Delete(context.Background(), w.leaver)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}

	if removal.PrivateViewsDeleted != 2 {
		t.Errorf("removal reported %d private views, want 2", removal.PrivateViewsDeleted)
	}

	for _, id := range []string{mine, alsoMine} {
		if n := w.count(t, `SELECT count(*) FROM saved_view WHERE id = $1::uuid`, id); n != 0 {
			t.Errorf("a private view survived its owner: %s", id)
		}
	}

	// Somebody else's private view is untouched, which a sweep keyed on
	// is_shared alone would have taken.
	if n := w.count(t, `SELECT count(*) FROM saved_view WHERE id = $1::uuid`, theirs); n != 1 {
		t.Error("another account's private view was deleted")
	}

	// And no orphan is left anywhere: the state 0027 describes is now
	// unreachable through this path.
	if n := w.count(t,
		`SELECT count(*) FROM saved_view WHERE owner_account_id IS NULL AND NOT is_shared`); n != 0 {
		t.Errorf("%d orphaned private views remain", n)
	}
}

// The other half of 0027's decision, and the reason owner_account_id is SET NULL rather
// than CASCADE: a shared team view must survive its author leaving.
func TestASharedViewSurvivesItsAuthorLeaving(t *testing.T) {
	w := newWorld(t)

	shared := w.newView(t, w.leaver, "Team backlog", true)

	removal, err := w.svc.Delete(context.Background(), w.leaver)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}

	if removal.SharedViewsKept != 1 {
		t.Errorf("removal reported %d shared views kept, want 1", removal.SharedViewsKept)
	}

	var owner *string
	var isShared bool
	if err := w.pool.QueryRow(context.Background(),
		`SELECT owner_account_id::text, is_shared FROM saved_view WHERE id = $1::uuid`, shared,
	).Scan(&owner, &isShared); err != nil {
		t.Fatalf("the shared view went with its author: %v", err)
	}

	if owner != nil {
		t.Errorf("owner = %s, want null", *owner)
	}
	if !isShared {
		t.Error("the shared view was un-shared, which would hide it from the team it belongs to")
	}
}

// A view the account owns that is shared, and one that is private, in one deletion.
// The counts are the only record of what it took, so they have to be right.
func TestTheCountsDescribeWhatTheDeletionDid(t *testing.T) {
	w := newWorld(t)

	w.newView(t, w.leaver, "Drafts", false)
	w.newView(t, w.leaver, "Blockers", false)
	w.newView(t, w.leaver, "Team backlog", true)
	w.newView(t, w.stays, "Theirs", false)

	removal, err := w.svc.Delete(context.Background(), w.leaver)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}

	if removal.PrivateViewsDeleted != 2 || removal.SharedViewsKept != 1 {
		t.Errorf("removal = %+v, want {2, 1}", removal)
	}
}

// Work outlives the people who touched it. LAM-24 settled that for comments; the same
// call was made for ticket assignment in 0011, and this deletion must not quietly
// revisit either.
func TestWorkSurvivesTheAccountThatTouchedIt(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	var ticketID string
	if err := w.pool.QueryRow(ctx,
		`INSERT INTO ticket (project_id, title, assignee_account_id)
		 VALUES ($1::uuid, 'Fix login', $2::uuid) RETURNING id::text`,
		w.project, w.leaver).Scan(&ticketID); err != nil {
		t.Fatalf("fixture ticket: %v", err)
	}

	var commentID string
	if err := w.pool.QueryRow(ctx,
		`INSERT INTO comment (ticket_id, author_id, body)
		 VALUES ($1::uuid, $2::uuid, 'a week of discussion') RETURNING id::text`,
		ticketID, w.leaver).Scan(&commentID); err != nil {
		t.Fatalf("fixture comment: %v", err)
	}

	if _, err := w.svc.Delete(ctx, w.leaver); err != nil {
		t.Fatalf("delete: %v", err)
	}

	var assignee, author *string
	if err := w.pool.QueryRow(ctx,
		`SELECT t.assignee_account_id::text, c.author_id::text
		   FROM ticket t JOIN comment c ON c.ticket_id = t.id
		  WHERE t.id = $1::uuid`, ticketID).Scan(&assignee, &author); err != nil {
		t.Fatalf("the ticket or its comment went with the account: %v", err)
	}

	if assignee != nil || author != nil {
		t.Errorf("assignee=%v author=%v, want both null", assignee, author)
	}
}

// A credential for an account that no longer exists is not a credential.
func TestCredentialsGoWithTheAccount(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	if _, err := w.pool.Exec(ctx,
		`INSERT INTO session (account_id, token_hash, expires_at)
		 VALUES ($1::uuid, 'h', now() + interval '1 day')`, w.leaver); err != nil {
		t.Fatalf("fixture session: %v", err)
	}
	if _, err := w.pool.Exec(ctx,
		`INSERT INTO api_token (account_id, label, token_prefix, token_hash)
		 VALUES ($1::uuid, 'ci', 'abcdefgh', 'h')`, w.leaver); err != nil {
		t.Fatalf("fixture token: %v", err)
	}

	if _, err := w.svc.Delete(ctx, w.leaver); err != nil {
		t.Fatalf("delete: %v", err)
	}

	for _, table := range []string{"session", "api_token", "workspace_member"} {
		if n := w.count(t, `SELECT count(*) FROM `+table+` WHERE account_id = $1::uuid`, w.leaver); n != 0 {
			t.Errorf("%d %s rows survived the account", n, table)
		}
	}
}

// The sweep runs first, so a deletion that finds no account must not have swept
// anything - otherwise a wrong id costs somebody their views on the way to a 404.
func TestDeletingAnAbsentAccountSweepsNothing(t *testing.T) {
	w := newWorld(t)

	kept := w.newView(t, w.stays, "Their drafts", false)

	const absent = "11111111-1111-4111-8111-111111111111"
	if _, err := w.svc.Delete(context.Background(), absent); !errors.Is(err, account.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}

	if n := w.count(t, `SELECT count(*) FROM saved_view WHERE id = $1::uuid`, kept); n != 1 {
		t.Error("a failed deletion still swept a view")
	}
}

func TestDeletingTwiceIsNotFound(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	if _, err := w.svc.Delete(ctx, w.leaver); err != nil {
		t.Fatalf("first delete: %v", err)
	}
	if _, err := w.svc.Delete(ctx, w.leaver); !errors.Is(err, account.ErrNotFound) {
		t.Errorf("second delete = %v, want ErrNotFound", err)
	}
}
