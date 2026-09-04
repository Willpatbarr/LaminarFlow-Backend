package frontend

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// bundle mimics what Vite emits: a shell plus content-hashed assets.
var bundle = fstest.MapFS{
	"index.html":              {Data: []byte("<!doctype html><div id=root></div>")},
	"assets/index-AbC123.js":  {Data: []byte("console.log(1)")},
	"assets/index-AbC123.css": {Data: []byte("body{}")},
	"favicon.svg":             {Data: []byte("<svg/>")},
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

	return rec
}

func TestServesRealFiles(t *testing.T) {
	h := New(bundle)

	for _, tc := range []struct {
		path     string
		wantBody string
		wantType string
	}{
		{"/", "<div id=root>", "text/html"},
		{"/index.html", "<div id=root>", "text/html"},
		{"/assets/index-AbC123.js", "console.log(1)", "javascript"},
		{"/assets/index-AbC123.css", "body{}", "text/css"},
		{"/favicon.svg", "<svg/>", "image/svg+xml"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			rec := get(t, h, tc.path)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Errorf("body = %q, want it to contain %q", rec.Body.String(), tc.wantBody)
			}
			if got := rec.Header().Get("Content-Type"); !strings.Contains(got, tc.wantType) {
				t.Errorf("Content-Type = %q, want it to contain %q", got, tc.wantType)
			}
		})
	}
}

// The reason this handler exists rather than a bare http.FileServer: a browser
// asking for a client-side route directly must get the app shell, not a 404.
func TestSPAFallbackServesShell(t *testing.T) {
	h := New(bundle)

	for _, path := range []string{"/projects", "/projects/42", "/settings/deep/nested"} {
		t.Run(path, func(t *testing.T) {
			rec := get(t, h, path)

			if rec.Code != http.StatusOK {
				t.Errorf("status = %d, want 200 - a refresh on a client route must not 404", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), "<div id=root>") {
				t.Errorf("body = %q, want the app shell", rec.Body.String())
			}
		})
	}
}

// A missing asset must NOT fall back to the shell. Returning HTML for a .js
// request produces "Unexpected token '<'" in the console, which points nowhere
// near the actual problem.
func TestMissingAssetDoesNotFallBack(t *testing.T) {
	h := New(bundle)

	rec := get(t, h, "/assets/index-OldHash.js")
	body := rec.Body.String()

	if strings.Contains(body, "<div id=root>") {
		t.Error("a missing asset returned the app shell; a stale hash would surface as a JS syntax error")
	}
}

// Hashed assets can be cached forever because a change renames them. The shell
// cannot, because its name never changes and it is what points at the hashes.
func TestCacheHeaders(t *testing.T) {
	h := New(bundle)

	if got := get(t, h, "/assets/index-AbC123.js").Header().Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Errorf("asset Cache-Control = %q, want immutable", got)
	}

	for _, path := range []string{"/", "/index.html", "/some/client/route"} {
		if got := get(t, h, path).Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("%s Cache-Control = %q, want no-cache", path, got)
		}
	}
}

// A path that escapes the bundle must be refused rather than resolved. Doing
// the lookup by hand means doing this by hand too - http.FileServer would have
// handled it.
func TestRejectsTraversal(t *testing.T) {
	h := New(bundle)

	// httptest.NewRequest keeps the raw path, so the handler sees the dots.
	rec := get(t, h, "/../../etc/passwd")

	if strings.Contains(rec.Body.String(), "root:") {
		t.Fatal("traversal escaped the bundle")
	}
}

// A fresh clone has never run the build script, so the embedded bundle holds
// only the directory marker. A blank 404 there is the confusing case: the
// server is healthy and the browser shows nothing.
func TestUnbuiltBundleExplainsItself(t *testing.T) {
	h := New(fstest.MapFS{})

	rec := get(t, h, "/")

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Frontend not built") {
		t.Errorf("body = %q, want it to name the problem", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "build-frontend.sh") {
		t.Error("the not-built page should name the script that fixes it")
	}
}
