// Command migrate applies, rolls back, and reports on schema migrations.
//
//	migrate up          apply every pending migration
//	migrate down [n]    roll back the n most recent (default 1)
//	migrate status      list every migration and whether it has run
//	migrate baseline    record all migrations as applied, running none
//
// The migrations are embedded in this binary, so it needs a database URL and
// nothing else - there is no migrations directory to be standing in.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/config"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/db"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/migrate"
	"github.com/Willpatbarr/LaminarFlow-Backend/migrations"
)

func main() {
	log.SetFlags(0)

	if len(os.Args) < 2 {
		usage()
	}

	ctx := context.Background()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	loaded, err := migrate.Load(migrations.FS)
	if err != nil {
		log.Fatalf("migrations: %v", err)
	}

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("database: %v", err)
	}
	defer pool.Close()

	switch os.Args[1] {
	case "up":
		applied, err := migrate.Up(ctx, pool, loaded)
		if err != nil {
			log.Fatalf("up: %v", err)
		}
		if len(applied) == 0 {
			log.Print("already up to date")
			return
		}
		for _, m := range applied {
			log.Printf("applied %04d_%s", m.Version, m.Name)
		}

	case "down":
		n := 1
		if len(os.Args) > 2 {
			n, err = strconv.Atoi(os.Args[2])
			if err != nil {
				log.Fatalf("down: %q is not a number", os.Args[2])
			}
		}
		reverted, err := migrate.Down(ctx, pool, loaded, n)
		if err != nil {
			log.Fatalf("down: %v", err)
		}
		if len(reverted) == 0 {
			log.Print("nothing to roll back")
			return
		}
		for _, m := range reverted {
			log.Printf("reverted %04d_%s", m.Version, m.Name)
		}

	case "status":
		records, err := migrate.Status(ctx, pool, loaded)
		if err != nil {
			log.Fatalf("status: %v", err)
		}
		for _, r := range records {
			mark := "pending"
			if r.Applied {
				mark = "applied"
			}
			fmt.Printf("%-8s %04d_%s\n", mark, r.Version, r.Name)
		}

	case "baseline":
		recorded, err := migrate.Baseline(ctx, pool, loaded)
		if err != nil {
			log.Fatalf("baseline: %v", err)
		}
		log.Printf("recorded %d migrations as applied without running them", len(recorded))

	default:
		usage()
	}
}

func usage() {
	log.Fatal("usage: migrate up | down [n] | status | baseline")
}
