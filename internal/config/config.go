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
}

// ErrMissingDatabaseURL is returned when DATABASE_URL is unset. There is no
// default on purpose: silently connecting to the wrong database is worse than
// refusing to start.
var ErrMissingDatabaseURL = errors.New("DATABASE_URL is not set")

// Load reads configuration from the environment.
func Load() (Config, error) {
	cfg := Config{
		Port:        getenv("PORT", "8080"),
		DatabaseURL: os.Getenv("DATABASE_URL"),
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
