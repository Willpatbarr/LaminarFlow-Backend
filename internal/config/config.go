// Package config loads runtime configuration from the environment so the same
// binary can run against a local Postgres, the Pi, or the home server without
// code changes.
package config

import (
	"errors"
	"os"
)

// Config holds the settings the backend needs at startup.
type Config struct {
	Port        string
	DatabaseURL string

	// MigrateOnStartup lets the server apply pending migrations itself. It is
	// off by default: an unattended container restart must not silently reshape
	// the database. Turn it on for the self-hosted target, where a two-step
	// upgrade over Tailscale is friction nobody needs, and leave it off
	// anywhere a human is already running deploys.
	MigrateOnStartup bool

	// FrontendDir overrides the embedded bundle with a directory on disk. Empty
	// means use the embedded one, which is what a deployed binary does. It
	// exists so a frontend rebuild can be seen without rebuilding Go.
	FrontendDir string
}

// ErrMissingDatabaseURL is returned when DATABASE_URL is unset. There is no
// default on purpose: silently connecting to the wrong database is worse than
// refusing to start.
var ErrMissingDatabaseURL = errors.New("DATABASE_URL is not set")

// Load reads configuration from the environment.
func Load() (Config, error) {
	cfg := Config{
		Port:             getenv("PORT", "8080"),
		DatabaseURL:      os.Getenv("DATABASE_URL"),
		MigrateOnStartup: os.Getenv("MIGRATE_ON_STARTUP") == "true",
		FrontendDir:      os.Getenv("FRONTEND_DIR"),
	}

	if cfg.DatabaseURL == "" {
		return Config{}, ErrMissingDatabaseURL
	}

	return cfg, nil
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
