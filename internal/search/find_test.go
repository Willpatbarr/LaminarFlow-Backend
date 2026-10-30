package search_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/account"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/search"

	"github.com/jackc/pgx/v5/pgxpool"
)

// LAM-100. The read path and the project source. The endpoint itself is tested in
// internal/api; this is the SQL and the schema under it.

func member(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()

	reg, err := account.NewService(pool).Register(context.Background(), account.NewAccount{
		Email:        fmt.Sprintf("searcher-%d@example.com", time.Now().UnixNano()),
		PasswordHash: "x",
		DisplayName:  "Searcher",
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	return reg.AccountID
}

func TestFindReachesEverySourceOfTheFixture(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	f := newFixture(t, pool)
	who := member(t, pool)

	svc := search.NewService(pool)
	if _, err := svc.Rebuild(ctx); err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	for _, tc := range []struct{ q, wantType, wantID string }{
		{"login", "ticket", f.ticket},
		{"firefox", "comment", f.ticketComment},
		{"sign in", "document", f.doc},
		{"core", "project", f.project},
	} {
		hits, err := svc.Find(ctx, who, search.FindParams{Q: tc.q})
		if err != nil {
			t.Fatalf("find %q: %v", tc.q, err)
		}
		if len(hits) == 0 || hits[0].Type != tc.wantType || hits[0].ID != tc.wantID {
			t.Errorf("find %q = %+v, want first %s %s", tc.q, hits, tc.wantType, tc.wantID)
		}
	}

	// A document with several matching fields is one result.
	hits, err := svc.Find(ctx, who, search.FindParams{Q: "user service", Type: "document"})
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(hits) != 1 {
		t.Errorf("title-only match returned %d results, want 1", len(hits))
	}
}

func TestFindEscapesBeforeMarking(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	f := newFixture(t, pool)
	who := member(t, pool)

	if _, err := pool.Exec(ctx,
		`UPDATE ticket SET description = $2 WHERE id = $1::uuid`, f.ticket,
		"see <img src=x onerror=1> and &amp; the wombat  "); err != nil {
		t.Fatalf("update ticket: %v", err)
	}

	svc := search.NewService(pool)
	if _, err := svc.Rebuild(ctx); err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	hits, err := svc.Find(ctx, who, search.FindParams{Q: "wombat", Type: "ticket"})
	if err != nil || len(hits) != 1 {
		t.Fatalf("find: %v %+v", err, hits)
	}

	s := hits[0].Snippet
	for _, bad := range []string{"<img", "", ""} {
		if strings.Contains(s, bad) {
			t.Errorf("snippet %q contains %q", s, bad)
		}
	}
	if !strings.Contains(s, "&amp;amp;") || !strings.Contains(s, "<mark>wombat</mark>") {
		t.Errorf("snippet = %q", s)
	}
}

// 0037's fourth arm: a row describes exactly one source, and deleting the project takes
// its row with it.
func TestProjectRowsAreExclusiveAndCascade(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	f := newFixture(t, pool)

	if _, err := search.NewService(pool).Rebuild(ctx); err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	_, err := pool.Exec(ctx,
		`INSERT INTO search_index (project_source_id, ticket_id, content, workspace_id)
		 VALUES ($1::uuid, $2::uuid, 'x', $3::uuid)`, f.project, f.ticket, f.workspace)
	wantPgError(t, err, checkViolation, "a row naming a project and a ticket")

	_, err = pool.Exec(ctx,
		`INSERT INTO search_index (project_source_id, content, workspace_id)
		 VALUES ($1::uuid, 'x', $2::uuid)`, f.project, f.workspace)
	if err != nil {
		t.Errorf("a second project row failed: %v", err)
	}

	var rows int
	count := func() int {
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM search_index WHERE project_source_id = $1::uuid`, f.project).Scan(&rows); err != nil {
			t.Fatalf("count: %v", err)
		}
		return rows
	}
	if count() != 2 {
		t.Fatalf("project rows = %d, want 2", rows)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM project WHERE id = $1::uuid`, f.project); err != nil {
		t.Fatalf("delete project: %v", err)
	}
	if count() != 0 {
		t.Errorf("%d project rows survived the project", rows)
	}
}

// The ticket asks for proof the query is index-driven, not a scan. With enough rows the
// planner picks an index unaided; here the table is tiny, so sequential scans are
// switched off for the transaction, which asks the better question: *can* the statement
// use an index, or does its shape forbid it. A predicate wrapped in a function, say,
// would still scan.
func TestFindIsAnIndexedQuery(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	newFixture(t, pool)
	who := member(t, pool)

	if _, err := search.NewService(pool).Rebuild(ctx); err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatalf("set: %v", err)
	}

	rows, err := tx.Query(ctx, `EXPLAIN `+search.FindSQL, who, "login", "", 20, "", "", "", "", "login")
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	var plan []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan: %v", err)
		}
		plan = append(plan, line)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("plan: %v", err)
	}

	text := strings.Join(plan, "\n")
	if !strings.Contains(text, "search_index_search_idx") && !strings.Contains(text, "search_index_workspace_idx") {
		t.Errorf("plan uses neither search_index_search_idx nor search_index_workspace_idx:\n%s", text)
	}
	if strings.Contains(text, "Seq Scan on search_index") {
		t.Errorf("plan scans search_index:\n%s", text)
	}
}

// The ticket's "p50 under 50 ms". A few thousand rows is far past one person's data and
// still small enough to run on every CI build; the point is to catch a regression to a
// scan or a per-row function call, which costs far more than the margin here.
func TestFindMedianLatency(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	f := newFixture(t, pool)
	who := member(t, pool)

	if _, err := pool.Exec(ctx,
		`INSERT INTO ticket (project_id, title, description)
		 SELECT $1::uuid, 'Task ' || n, 'filler text about widgets and gadgets number ' || n
		   FROM generate_series(1, 3000) n`, f.project); err != nil {
		t.Fatalf("seed tickets: %v", err)
	}

	svc := search.NewService(pool)
	if _, err := svc.Rebuild(ctx); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if _, err := pool.Exec(ctx, `ANALYZE search_index`); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	var took []time.Duration
	for i := 0; i < 21; i++ {
		start := time.Now()
		if _, err := svc.Find(ctx, who, search.FindParams{Q: "login"}); err != nil {
			t.Fatalf("find: %v", err)
		}
		took = append(took, time.Since(start))
	}
	slices.Sort(took)

	if p50 := took[len(took)/2]; p50 > 50*time.Millisecond {
		t.Errorf("p50 = %v, want under 50ms", p50)
	}
}
