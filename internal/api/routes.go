package api

import (
	"io/fs"
	"net/http"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/frontend"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NewMux wires every route the server answers.
//
// Split out of main so it can be tested: main itself needs a real database URL
// and a listening socket, which is why the routing went unverified until LAM-39.
func NewMux(pool *pgxpool.Pool, bundle fs.FS) *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", live())
	mux.HandleFunc("GET /healthz/db", ready(pool))
	NewHumaAPI(mux)
	mux.HandleFunc("/api/", notFound())

	// Everything else is the frontend: real files where they exist, the app
	// shell everywhere else so client-side routing survives a refresh.
	mux.Handle("/", frontend.New(bundle))

	return mux
}
