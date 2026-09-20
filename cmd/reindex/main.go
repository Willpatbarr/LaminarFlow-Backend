// Command reindex regenerates search_index from documents, tickets and comments.
package main

import (
	"context"
	"log"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/config"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/db"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/search"
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

	counts, err := search.NewService(pool).Rebuild(ctx)
	if err != nil {
		log.Fatalf("rebuild: %v", err)
	}

	// Per source, because "reindexed 12" hides a rebuild that silently indexed no
	// tickets at all - which is exactly the regression LAM-45 is closing.
	log.Printf("reindexed %d documents, %d tickets, %d comments",
		counts.Documents, counts.Tickets, counts.Comments)
}
