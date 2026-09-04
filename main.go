package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/config"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/db"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/migrate"
	"github.com/Willpatbarr/LaminarFlow-Backend/migrations"
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

	if err := ensureSchema(context.Background(), pool, cfg.MigrateOnStartup); err != nil {
		log.Fatalf("schema: %v", err)
	}

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

// ensureSchema brings the database up to date, or refuses to start.
//
// Serving against a schema this binary does not match is the failure worth
// preventing: it does not announce itself at startup, it announces itself as a
// missing column on whichever request happens to touch the new table first.
func ensureSchema(ctx context.Context, pool *pgxpool.Pool, autoApply bool) error {
	loaded, err := migrate.Load(migrations.FS)
	if err != nil {
		return err
	}

	if autoApply {
		applied, err := migrate.Up(ctx, pool, loaded)
		if err != nil {
			return err
		}
		for _, m := range applied {
			log.Printf("applied migration %04d_%s", m.Version, m.Name)
		}
		return nil
	}

	pending, err := migrate.Pending(ctx, pool, loaded)
	if err != nil {
		return err
	}
	if len(pending) > 0 {
		return fmt.Errorf(
			"%d migration(s) pending, oldest %04d_%s - run `migrate up`, or set MIGRATE_ON_STARTUP=true",
			len(pending), pending[0].Version, pending[0].Name)
	}

	log.Print("schema is up to date")
	return nil
}
