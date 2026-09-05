// Package api owns the HTTP surface: one function per endpoint, grouped by
// resource, and nothing beyond parsing a request and formatting a response.
//
// Rules and SQL belong in internal/document and its siblings. A handler that
// grows a query is a handler in the wrong package - that is the convention
// this package exists to hold.
package api

import "net/http"

// writeJSON is the one way this package answers. Routing every handler through
// it means no endpoint can ship without the Content-Type a fetch() needs.
func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write([]byte(body))
}

// notFound answers anything under /api/ that no handler claims.
//
// The prefix is registered even though nothing lives under it yet. Without it,
// Go's mux routes /api/anything to the frontend catch-all and a typo'd endpoint
// returns the HTML app shell with status 200 - which a fetch() reports as a
// JSON parse error, three layers from the cause.
func notFound() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, `{"error":"not found"}`)
	}
}
