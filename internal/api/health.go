package api

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/db"
	"github.com/jackc/pgx/v5/pgxpool"
)

const dbCheckTimeout = 3 * time.Second

// live answers liveness. Deliberately does not touch Postgres: if this failed
// whenever the database blipped, a supervisor would restart a healthy process.
func live() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, `{"status":"ok"}`)
	}
}

// ready answers readiness. Proves the backend can still reach Postgres.
func ready(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), dbCheckTimeout)
		defer cancel()

		if err := db.Check(ctx, pool); err != nil {
			log.Printf("db check failed: %v", err)
			writeJSON(w, http.StatusServiceUnavailable, `{"status":"unavailable"}`)
			return
		}

		writeJSON(w, http.StatusOK, `{"status":"ok"}`)
	}
}
