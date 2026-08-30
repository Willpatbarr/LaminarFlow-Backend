// Command reindex regenerates search_index from the document body blobs.
package main

import (
	"context"
	"log"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/config"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/db"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/document"
)

func main() {
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

	count, err := document.NewService(pool).RebuildIndex(ctx)
	if err != nil {
		log.Fatalf("rebuild: %v", err)
	}

	log.Printf("reindexed %d documents", count)
}