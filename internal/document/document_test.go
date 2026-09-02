package document
import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)


// testPool connects to the database named by TEST_DATABASE_URL and empties it.
// The test is skipped rather than failed when that variable is unset, so
// `go test ./...` still passes on a machine with no database.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	// Deleting documents clears search_index too, via ON DELETE CASCADE.
	if _, err := pool.Exec(context.Background(), `DELETE FROM document`); err != nil {
		t.Fatalf("reset: %v", err)
	}

	return pool
}


// defaultWorkspace returns the ID of the workspace the tests write into.
// Migration 0003 seeds exactly one row, and testPool's DELETE FROM document
// leaves workspace rows untouched, so this is stable across runs.
func defaultWorkspace(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()

	var id string
	if err := pool.QueryRow(context.Background(),
		`SELECT id::text FROM workspace WHERE name = 'Default'`,
	).Scan(&id); err != nil {
		t.Fatalf("read default workspace: %v", err)
	}

	return id
}


// indexSnapshot reads the whole search_index into a comparable map keyed by
// "documentID|fieldID".
func indexSnapshot(t *testing.T, pool *pgxpool.Pool) map[string]string {
	t.Helper()

	rows, err := pool.Query(context.Background(),
		`SELECT document_id::text, field_id, content FROM search_index`)
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	defer rows.Close()

	out := make(map[string]string)
	for rows.Next() {
		var docID, fieldID, content string
		if err := rows.Scan(&docID, &fieldID, &content); err != nil {
			t.Fatalf("scan index: %v", err)
		}
		out[docID+"|"+fieldID] = content
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read index: %v", err)
	}

	return out
}


func TestSaveWritesBlobAndIndex(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	svc := NewService(pool)
	ws := defaultWorkspace(t, pool)

	id, err := svc.Save(ctx, ws, "", map[string]any{
		"f_title": "Quarterly Report",
		"f_year":  2026,
		"f_tags":  []any{"alpha", "beta"},
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	var raw string
	if err := pool.QueryRow(ctx,
		`SELECT body::text FROM document WHERE id = $1::uuid`, id,
	).Scan(&raw); err != nil {
		t.Fatalf("read body: %v", err)
	}

	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got := body["f_title"]; got != "Quarterly Report" {
		t.Errorf("blob f_title = %v, want %q", got, "Quarterly Report")
	}

	want := map[string]string{
		id + "|f_title": "Quarterly Report",
		id + "|f_year":  "2026",
		id + "|f_tags":  "alpha beta",
	}
	if got := indexSnapshot(t, pool); !maps.Equal(got, want) {
		t.Errorf("index = %v, want %v", got, want)
	}
}


// A field removed from the body must lose its index row. This is the test that
// fails if Save is ever changed from delete-then-insert to a bare upsert.
func TestSaveRemovesStaleIndexRows(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	svc := NewService(pool)

	ws := defaultWorkspace(t, pool)

	id, err := svc.Save(ctx, ws, "", map[string]any{"f_title": "Notes", "f_note": "draft"})
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	if _, err := svc.Save(ctx, ws, id, map[string]any{"f_title": "Notes"}); err != nil {
		t.Fatalf("resave: %v", err)
	}

	want := map[string]string{id + "|f_title": "Notes"}
	if got := indexSnapshot(t, pool); !maps.Equal(got, want) {
		t.Errorf("index = %v, want %v", got, want)
	}
}


// The safety net named in LAM-3: an index rebuilt from the blobs must be
// byte-identical to the index the live write path produced. If these ever
// disagree, the two code paths have drifted.
func TestRebuildMatchesLiveIndex(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	svc := NewService(pool)

	ws := defaultWorkspace(t, pool)

	for _, body := range []map[string]any{
		{"f_title": "Quarterly Report", "f_year": 2026, "f_draft": false},
		{"f_title": "Notes", "f_tags": []any{"alpha", "beta"}},
		{"f_title": "Nested", "f_meta": map[string]any{"b": "second", "a": "first"}},
	} {
		if _, err := svc.Save(ctx, ws, "", body); err != nil {
			t.Fatalf("save: %v", err)
		}
	}

	live := indexSnapshot(t, pool)
	// Guard against a vacuous pass: two empty maps are trivially equal.
	if len(live) == 0 {
		t.Fatal("live index is empty, nothing to compare")
	}

	if _, err := svc.RebuildIndex(ctx); err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	if rebuilt := indexSnapshot(t, pool); !maps.Equal(live, rebuilt) {
		t.Errorf("rebuilt index differs from live index:\n live     = %v\n rebuilt  = %v", live, rebuilt)
	}
}