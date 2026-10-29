// Package config loads runtime configuration from the environment so the same
// binary can run against a local Postgres, the Pi, or the home server without
// code changes.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
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

	// AttachmentDir is the folder uploaded files are kept in (LAM-90). It is
	// state, like the database: it belongs on a volume and in the backup. The
	// default is relative so a bare `go run` works; the deployed stack sets it
	// to a mounted volume.
	AttachmentDir string

	// MaxAttachmentBytes is the largest single upload the API accepts.
	MaxAttachmentBytes int64
}

// DefaultAttachmentDir and DefaultMaxAttachmentBytes are the values used when the
// environment says nothing.
const (
	DefaultAttachmentDir      = "data/files"
	DefaultMaxAttachmentBytes = 25 << 20
)

// DefaultPort is the port used when PORT is unset. Exported so the container
// healthcheck can reach the server without loading the whole config, which
// would need a database URL it has no business requiring.
const DefaultPort = "8080"

// ErrMissingDatabaseURL is returned when DATABASE_URL is unset. There is no
// default on purpose: silently connecting to the wrong database is worse than
// refusing to start.
var ErrMissingDatabaseURL = errors.New("DATABASE_URL is not set")

// Load reads configuration from the environment.
func Load() (Config, error) {
	cfg := Config{
		Port:             getenv("PORT", DefaultPort),
		DatabaseURL:      os.Getenv("DATABASE_URL"),
		MigrateOnStartup: os.Getenv("MIGRATE_ON_STARTUP") == "true",
		FrontendDir:      os.Getenv("FRONTEND_DIR"),
		AttachmentDir:    getenv("ATTACHMENT_DIR", DefaultAttachmentDir),
	}

	cfg.MaxAttachmentBytes = DefaultMaxAttachmentBytes
	if v := os.Getenv("MAX_ATTACHMENT_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 {
			return Config{}, fmt.Errorf("MAX_ATTACHMENT_BYTES must be a positive integer, got %q", v)
		}
		cfg.MaxAttachmentBytes = n
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
