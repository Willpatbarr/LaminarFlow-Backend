package search_test

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"os"
	"strings"
	"testing"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/dbtest"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/document"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/migrate"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/search"
	"github.com/Willpatbarr/LaminarFlow-Backend/migrations"

	"github.com/jackc/pgx/v5/pgxpool"
)

// An external test package, because internal/document imports internal/search and
// these tests need document.Save. package search_test is compiled separately, so the
// import runs one way and there is no cycle.

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
	log.Printf("test database: %s", dbtest.Name(dsn))

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

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	if testDatabaseURL == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	pool, err := pgxpool.New(context.Background(), testDatabaseURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	// Tickets cascade from project, comments from either parent, index rows from all
	// three. Deleting the two roots empties everything this file looks at.
	for _, table := range []string{"document", "team"} {
		if _, err := pool.Exec(context.Background(), `DELETE FROM `+table); err != nil {
			t.Fatalf("reset %s: %v", table, err)
		}
	}

	return pool
}

func defaultWorkspace(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()

	var id string
	if err := pool.QueryRow(context.Background(),
		`SELECT id::text FROM workspace WHERE name = 'Default'`).Scan(&id); err != nil {
		t.Fatalf("read default workspace: %v", err)
	}

	return id
}

// snapshot reads the whole index into a comparable map. The key names the source, so
// a ticket row and a document row cannot collide and a missing source shows up as a
// missing key rather than a count that happens to match.
func snapshot(t *testing.T, pool *pgxpool.Pool) map[string]string {
	t.Helper()

	rows, err := pool.Query(context.Background(),
		`SELECT coalesce(document_id::text, '') , coalesce(field_id, ''),
		        coalesce(ticket_id::text, ''), coalesce(comment_id::text, ''),
		        content, coalesce(parent_reference, ''), workspace_id::text
		   FROM search_index`)
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var doc, field, ticket, comment, content, parent, ws string
		if err := rows.Scan(&doc, &field, &ticket, &comment, &content, &parent, &ws); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[fmt.Sprintf("d=%s|f=%s|t=%s|c=%s", doc, field, ticket, comment)] =
			fmt.Sprintf("%s|parent=%s|ws=%s", content, parent, ws)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	return out
}

// fixture builds one of each source and returns their ids.
type fixture struct {
	workspace, team, project, ticket, doc, ticketComment, docComment string
}

func newFixture(t *testing.T, pool *pgxpool.Pool) fixture {
	t.Helper()
	ctx := context.Background()

	f := fixture{workspace: defaultWorkspace(t, pool)}

	must := func(q string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, q, args...).Scan(&id); err != nil {
			t.Fatalf("fixture %q: %v", q, err)
		}
		return id
	}

	f.team = must(`INSERT INTO team (workspace_id, name) VALUES ($1::uuid, 'Platform')
	               RETURNING id::text`, f.workspace)
	f.project = must(`INSERT INTO project (team_id, name) VALUES ($1::uuid, 'Core')
	                  RETURNING id::text`, f.team)
	f.ticket = must(`INSERT INTO ticket (project_id, title, description)
	                 VALUES ($1::uuid, 'Fix login', 'The button does nothing')
	                 RETURNING id::text`, f.project)

	docID, err := document.NewService(pool).Save(ctx, document.SaveParams{
		WorkspaceID: f.workspace,
		Title:       "User Service",
		Body:        map[string]any{"f_summary": "Handles sign in"},
	})
	if err != nil {
		t.Fatalf("fixture save document: %v", err)
	}
	f.doc = docID

	f.ticketComment = must(`INSERT INTO comment (ticket_id, body)
	                        VALUES ($1::uuid, 'Reproduced on Firefox') RETURNING id::text`, f.ticket)
	f.docComment = must(`INSERT INTO comment (document_id, body)
	                     VALUES ($1::uuid, 'Needs a sequence diagram') RETURNING id::text`, f.doc)

	return f
}

// LAM-45's central assertion, and the reason the rebuild exists at all: if the rebuild
// disagrees with the live index, one of them is wrong. Now covering three sources
// rather than one - a rebuild that silently indexed no tickets would pass the old
// version of this test.
func TestRebuildMatchesLiveIndex(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	newFixture(t, pool)

	svc := search.NewService(pool)

	// Tickets and comments have no live write path yet - nothing calls IndexTicket
	// outside a rebuild, because LAM-57 owns ticket writes. So the live index is
	// built by one rebuild, and the comparison is against a second.
	if _, err := svc.Rebuild(ctx); err != nil {
		t.Fatalf("first rebuild: %v", err)
	}

	live := snapshot(t, pool)
	if len(live) == 0 {
		t.Fatal("index is empty, nothing to compare")
	}

	counts, err := svc.Rebuild(ctx)
	if err != nil {
		t.Fatalf("second rebuild: %v", err)
	}

	if rebuilt := snapshot(t, pool); !maps.Equal(live, rebuilt) {
		t.Errorf("rebuild disagrees with the previous index\n live = %v\n  new = %v", live, rebuilt)
	}

	if counts.Documents != 1 || counts.Tickets != 1 || counts.Comments != 2 {
		t.Errorf("counts = %+v, want 1 document, 1 ticket, 2 comments", counts)
	}
}

// The regression LAM-45 closes: two of three source arms had a schema and no writer.
func TestRebuildIndexesEverySource(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	f := newFixture(t, pool)

	if _, err := search.NewService(pool).Rebuild(ctx); err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	index := snapshot(t, pool)

	for _, tc := range []struct{ name, key, wantContains string }{
		{"document field", fmt.Sprintf("d=%s|f=f_summary|t=|c=", f.doc), "Handles sign in"},
		{"ticket", fmt.Sprintf("d=|f=|t=%s|c=", f.ticket), "Fix login"},
		{"ticket description", fmt.Sprintf("d=|f=|t=%s|c=", f.ticket), "The button does nothing"},
		{"ticket comment", fmt.Sprintf("d=|f=|t=|c=%s", f.ticketComment), "Reproduced on Firefox"},
		{"document comment", fmt.Sprintf("d=|f=|t=|c=%s", f.docComment), "Needs a sequence diagram"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := index[tc.key]
			if !ok {
				t.Fatalf("no index row for %s; index holds %v", tc.key, index)
			}
			if !strings.Contains(got, tc.wantContains) {
				t.Errorf("row = %q, want it to contain %q", got, tc.wantContains)
			}
		})
	}
}

// parent_reference is what lets the UI render "Comment on Fix login" without a join.
func TestCommentsCarryTheirParentReference(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	f := newFixture(t, pool)

	if _, err := search.NewService(pool).Rebuild(ctx); err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	index := snapshot(t, pool)

	for _, tc := range []struct{ name, key, wantParent string }{
		{"on a ticket", fmt.Sprintf("d=|f=|t=|c=%s", f.ticketComment), "parent=Fix login"},
		{"on a document", fmt.Sprintf("d=|f=|t=|c=%s", f.docComment), "parent=User Service"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := index[tc.key]
			if !strings.Contains(got, tc.wantParent) {
				t.Errorf("row = %q, want %q", got, tc.wantParent)
			}
		})
	}
}

// Scope is denormalised at write time so a read never joins. A ticket reaches its
// workspace through project and team; getting that chain wrong would silently scope
// every ticket row to nothing, and workspace_id being NOT NULL is what turns that into
// a failed insert instead.
func TestEveryRowCarriesItsWorkspace(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	f := newFixture(t, pool)

	if _, err := search.NewService(pool).Rebuild(ctx); err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	for key, row := range snapshot(t, pool) {
		if !strings.Contains(row, "ws="+f.workspace) {
			t.Errorf("row %s = %q, want workspace %s", key, row, f.workspace)
		}
	}
}

// search_index_document_rows_carry_a_field is a biconditional, so a ticket or comment
// row carrying a field_id fails the check rather than being ignored. Asserted because
// the insert statements are the only thing keeping it true.
func TestOnlyDocumentRowsCarryAFieldID(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	newFixture(t, pool)

	if _, err := search.NewService(pool).Rebuild(ctx); err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	var bad int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM search_index
		  WHERE (document_id IS NOT NULL) <> (field_id IS NOT NULL)`).Scan(&bad); err != nil {
		t.Fatalf("count: %v", err)
	}
	if bad != 0 {
		t.Errorf("%d rows break the document/field biconditional", bad)
	}
}

// Rebuild replaces rather than accumulates. Running it twice must not double the rows.
func TestRebuildIsIdempotent(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	newFixture(t, pool)

	svc := search.NewService(pool)
	if _, err := svc.Rebuild(ctx); err != nil {
		t.Fatalf("first: %v", err)
	}

	var first int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM search_index`).Scan(&first); err != nil {
		t.Fatalf("count: %v", err)
	}

	if _, err := svc.Rebuild(ctx); err != nil {
		t.Fatalf("second: %v", err)
	}

	var second int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM search_index`).Scan(&second); err != nil {
		t.Fatalf("count: %v", err)
	}

	if first != second {
		t.Errorf("row count went %d -> %d across two rebuilds", first, second)
	}
}
