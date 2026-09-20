// Command settings-audit lists stored settings whose key is not in the registry.
//
// LAM-52 decision 3. A row written before the registry existed, or by hand, is skipped
// on read rather than raised - one bad row must not break the settings screen for a
// whole team. Skipping is only acceptable if something can still see it, and this is
// that something.
//
// Instance-wide and unscoped, like cmd/reindex: it answers an operator's question, and
// an answer filtered to one workspace would not say whether the instance is clean.
package main

import (
	"context"
	"log"
	"os"

	"github.com/Willpatbarr/LaminarFlow-Backend/internal/config"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/db"
	"github.com/Willpatbarr/LaminarFlow-Backend/internal/setting"
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

	unknown, err := setting.NewService(pool).Unregistered(ctx)
	if err != nil {
		log.Fatalf("audit: %v", err)
	}

	if len(unknown) == 0 {
		log.Printf("every stored setting is registered (%d keys in the registry)", len(setting.Registry))
		return
	}

	for _, u := range unknown {
		log.Printf("unregistered: %q on %s %s", u.Key, u.Scope, u.TargetID)
	}

	// A non-zero exit so this is usable as a check rather than only as a report.
	// Nothing is repaired: the value may be the only record of what someone
	// intended, and deleting it is not this command's call to make.
	log.Printf("%d stored setting(s) no registry key matches - they are ignored on read", len(unknown))
	os.Exit(1)
}
