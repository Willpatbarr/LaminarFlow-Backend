package main

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/config"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/db"
)

const dbCheckTimeout = 3 * time.Second

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	pool, err := db.Connect(context.Background(), cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("database: %v", err)
	}
	defer pool.Close()

	log.Print("connected to postgres")

	mux := http.NewServeMux()

	// Liveness. Deliberately does not touch Postgres: if this failed whenever
	// the database blipped, a supervisor would restart a healthy process.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})

	// Readiness. Proves the backend can still reach Postgres.
	mux.HandleFunc("GET /healthz/db", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), dbCheckTimeout)
		defer cancel()

		w.Header().Set("Content-Type", "application/json")

		if err := db.Check(ctx, pool); err != nil {
			log.Printf("db check failed: %v", err)
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"status":"unavailable"}`))
			return
		}

		w.Write([]byte(`{"status":"ok"}`))
	})

	addr := ":" + cfg.Port
	log.Printf("laminarflow backend listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}
