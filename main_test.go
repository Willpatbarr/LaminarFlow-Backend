package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/dbtest"

	"github.com/jackc/pgx/v5/pgxpool"
)

// bundle stands in for the built frontend: a shell plus one hashed asset.
var bundle = fstest.MapFS{
	"index.html":             {Data: []byte("<!doctype html><div id=root></div>")},
	"assets/index-AbC123.js": {Data: []byte("console.log(1)")},
}

// testMux builds the real mux against a throwaway database.
//
// A real pool rather than a fake: /healthz/db exists to prove the backend can
// reach Postgres, so a test of it that never opens a connection would assert
// nothing worth asserting.
func testMux(t *testing.T) *http.ServeMux {
	t.Helper()

	ctx := context.Background()

	dsn, cleanup, err := dbtest.Create(ctx)
	if errors.Is(err, dbtest.ErrNoDatabase) {
		t.Skip("TEST_DATABASE_URL not set")
	}
	if err != nil {
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(cleanup)

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	return newMux(pool, bundle)
}

func get(t *testing.T, mux *http.ServeMux, path string) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

	return rec
}

// Liveness must not touch Postgres. A supervisor restarting a healthy process
// because the database blipped is worse than the blip.
func TestHealthzDoesNotNeedTheDatabase(t *testing.T) {
	ctx := context.Background()

	dsn, cleanup, err := dbtest.Create(ctx)
	if errors.Is(err, dbtest.ErrNoDatabase) {
		t.Skip("TEST_DATABASE_URL not set")
	}
	if err != nil {
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(cleanup)

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	mux := newMux(pool, bundle)

	// Close the pool out from under the mux, then ask for liveness anyway.
	pool.Close()

	rec := get(t, mux, "/healthz")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 with the database unreachable", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Errorf("body = %q, want ok", rec.Body.String())
	}
}

func TestHealthzDbReportsTheDatabase(t *testing.T) {
	mux := testMux(t)

	rec := get(t, mux, "/healthz/db")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Errorf("body = %q, want ok", rec.Body.String())
	}
}

// Readiness must fail, not lie, when Postgres is gone.
func TestHealthzDbFailsWhenTheDatabaseIsGone(t *testing.T) {
	ctx := context.Background()

	dsn, cleanup, err := dbtest.Create(ctx)
	if errors.Is(err, dbtest.ErrNoDatabase) {
		t.Skip("TEST_DATABASE_URL not set")
	}
	if err != nil {
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(cleanup)

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	mux := newMux(pool, bundle)
	pool.Close()

	rec := get(t, mux, "/healthz/db")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"status":"unavailable"`) {
		t.Errorf("body = %q, want unavailable", rec.Body.String())
	}
}

// The reservation this ticket exists for. Delete the /api/ registration in
// newMux and this is the only test that notices - every unknown API endpoint
// would otherwise start returning the HTML app shell with status 200, which a
// fetch() reports as a JSON parse error three layers from the cause.
func TestUnknownAPIRouteReturnsJSONNotTheAppShell(t *testing.T) {
	mux := testMux(t)

	for _, path := range []string{"/api/", "/api/nope", "/api/documents/42"} {
		t.Run(path, func(t *testing.T) {
			rec := get(t, mux, path)

			if rec.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 404", rec.Code)
			}
			if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
				t.Errorf("Content-Type = %q, want JSON", got)
			}
			if strings.Contains(rec.Body.String(), "<div id=root>") {
				t.Error("an unknown API route returned the app shell")
			}
		})
	}
}

// The frontend catch-all still works through the real mux, not just through
// the handler's own tests: a route pattern registered in the wrong order would
// break this without breaking internal/frontend.
func TestFrontendRoutesReachTheBundle(t *testing.T) {
	mux := testMux(t)

	for _, tc := range []struct{ path, want string }{
		{"/", "<div id=root>"},
		{"/index.html", "<div id=root>"},
		{"/assets/index-AbC123.js", "console.log(1)"},
		{"/projects/42", "<div id=root>"}, // SPA fallback
	} {
		t.Run(tc.path, func(t *testing.T) {
			rec := get(t, mux, tc.path)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Errorf("body = %q, want it to contain %q", rec.Body.String(), tc.want)
			}
		})
	}
}

// /healthz must not be swallowed by the frontend catch-all, and must not be
// reachable by a method it does not implement.
func TestHealthzIsNotShadowedByTheCatchAll(t *testing.T) {
	mux := testMux(t)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/healthz", nil))

	if strings.Contains(rec.Body.String(), "<div id=root>") {
		t.Error("POST /healthz fell through to the frontend handler")
	}
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}
