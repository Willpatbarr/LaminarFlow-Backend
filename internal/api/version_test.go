package api

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// The guard LAM-54 exists for.
//
// A const only helps a route that uses it. Nothing stops the next handler
// writing Path: "/api/v1/tickets" in full, and nothing stops that literal
// being "/api/vl/tickets" - which is a 404 indistinguishable from every other
// 404, on a route nobody has called yet.
//
// So the rule is enforced over the built document rather than trusted: every
// operation this API registers must sit under V1. Same shape as
// internal/document's boundary test, for the same reason - a convention that
// only lives in a comment is a convention until someone is in a hurry.
func TestEveryRouteCarriesTheVersionPrefix(t *testing.T) {
	api := NewHumaAPI(http.NewServeMux(), nil, nil)

	paths := api.OpenAPI().Paths
	if len(paths) == 0 {
		t.Fatal("no operations registered, so this guard proves nothing")
	}

	for path := range paths {
		if !strings.HasPrefix(path, V1+"/") {
			t.Errorf("operation path %q does not start with %q - compose it "+
				"with the V1 const rather than writing the prefix out", path, V1)
		}
	}
}

// The two version numbers disagreed before LAM-54: the path said v1 and the
// OpenAPI document said 0.1.0, which reads as "no stability promise" next to a
// path that carries one. Pinned so they cannot drift apart again.
func TestSpecVersionMajorMatchesThePathVersion(t *testing.T) {
	major, _, ok := strings.Cut(SpecVersion, ".")
	if !ok {
		t.Fatalf("SpecVersion = %q, want a dotted version", SpecVersion)
	}

	digits := strings.TrimPrefix(V1, "/api/v")
	if digits == V1 {
		t.Fatalf("V1 = %q, want it to end in a version number", V1)
	}

	if major != digits {
		t.Errorf("SpecVersion major = %q but %s says %q - the major must equal "+
			"the path version", major, V1, digits)
	}

	if _, err := strconv.Atoi(digits); err != nil {
		t.Errorf("path version = %q, want digits: %v", digits, err)
	}
}

// The docs, spec and schema routes are part of the versioned contract, not the
// root of the service (LAM-54 decision 4). Asserted against the real mux
// because huma's own prefix mechanism does not move them - setting Servers
// leaves them where they are, so only the explicit paths in NewHumaAPI do this.
func TestHumaOwnRoutesAreVersioned(t *testing.T) {
	mux := http.NewServeMux()
	NewHumaAPI(mux, nil, nil)

	for _, tc := range []struct {
		path string
		want int
	}{
		{V1 + "/docs", http.StatusOK},
		{V1 + "/openapi.json", http.StatusOK},
		{V1 + "/schemas/PingBody.json", http.StatusOK},

		// The root is where DefaultConfig would have put them.
		{"/docs", http.StatusNotFound},
		{"/openapi.json", http.StatusNotFound},
		{"/schemas/PingBody.json", http.StatusNotFound},
	} {
		t.Run(tc.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))

			if rec.Code != tc.want {
				t.Errorf("GET %s = %d, want %d", tc.path, rec.Code, tc.want)
			}
		})
	}
}
