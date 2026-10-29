// Command export writes the whole workspace as a zip: one JSON file per table and one
// Markdown file per document. It produces the same zip as POST /api/v1/export.
//
//	export [-o file.zip]     default laminarflow-export-<UTC timestamp>.zip in the current directory
//	export -o -              write the zip to stdout
//
// Reads DATABASE_URL like the other commands. It is for taking your data elsewhere, not
// a backup: attachment bytes are not in it. scripts/backup.sh is the backup.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/aspect"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/config"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/db"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/document"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/export"
)

func main() {
	log.SetFlags(0)

	out := flag.String("o", "", `output file; "-" for stdout (default laminarflow-export-<timestamp>.zip)`)
	flag.Parse()

	ctx := context.Background()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("database: %v", err)
	}
	defer pool.Close()

	name := *out
	if name == "" {
		name = fmt.Sprintf("laminarflow-export-%s.zip", time.Now().UTC().Format("20060102T150405Z"))
	}

	w := os.Stdout
	if name != "-" {
		// Written beside the destination and renamed, so a failed export never leaves
		// a half-written zip under the final name.
		tmp, err := os.CreateTemp(filepath.Dir(name), ".export-*.zip")
		if err != nil {
			log.Fatalf("output: %v", err)
		}
		defer os.Remove(tmp.Name())
		w = tmp
	}

	m, err := export.NewService(pool, document.NewService(pool), aspect.NewService(pool)).Write(ctx, w)
	if err != nil {
		log.Fatalf("export: %v", err)
	}

	if name != "-" {
		if err := w.Close(); err != nil {
			log.Fatalf("output: %v", err)
		}
		if err := os.Chmod(w.Name(), 0o600); err != nil {
			log.Fatalf("output: %v", err)
		}
		if err := os.Rename(w.Name(), name); err != nil {
			log.Fatalf("output: %v", err)
		}
	}

	rows := 0
	for _, n := range m.Files {
		rows += n
	}
	log.Printf("exported %d rows in %d files and %d documents to %s", rows, len(m.Files), m.Documents, name)
}
