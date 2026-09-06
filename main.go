package main

import (
	"context"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/api"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/config"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/db"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/migrate"
	"github.com/Willpatbarr/LaminarFlow-Backend/migrations"
	"github.com/Willpatbarr/LaminarFlow-Backend/web"
)

const (
	// Short on purpose: a container healthcheck that hangs is indistinguishable
	// from one that failed, but takes the whole interval to say so.
	healthcheckTimeout = 2 * time.Second
)

func main() {
	// A HEALTHCHECK in a distroless image has no shell and no curl, so the
	// binary checks itself. Handled before anything else so it never opens a
	// database connection or reads config it does not need.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}

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

	bundle, err := frontendFS(cfg.FrontendDir)
	if err != nil {
		log.Fatalf("frontend: %v", err)
	}

	mux := api.NewMux(pool, bundle)

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

// frontendFS picks the bundle the server will serve. An empty dir means the
// bundle embedded at build time, which is what a deployed binary uses.
func frontendFS(dir string) (fs.FS, error) {
	if dir != "" {
		log.Printf("serving frontend from %s", dir)
		return os.DirFS(dir), nil
	}

	// The embedded FS is rooted at the module, so strip the directory name to
	// make index.html sit at the root of what the handler sees.
	sub, err := fs.Sub(web.Dist, "dist")
	if err != nil {
		return nil, fmt.Errorf("open embedded bundle: %w", err)
	}

	return sub, nil
}

// healthcheck asks the running server whether it is alive and returns a process
// exit code. It deliberately hits /healthz rather than /healthz/db: a database
// blip should not make a supervisor restart a healthy process.
func healthcheck() int {
	port := os.Getenv("PORT")
	if port == "" {
		port = config.DefaultPort
	}

	client := &http.Client{Timeout: healthcheckTimeout}

	resp, err := client.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		return 1
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck: status %d\n", resp.StatusCode)
		return 1
	}

	return 0
}
