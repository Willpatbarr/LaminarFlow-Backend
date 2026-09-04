package config

import (
	"errors"
	"testing"
)

// Load reads process environment, so each case sets exactly the variables it
// cares about and t.Setenv restores them afterwards. t.Setenv also fails the
// test if it is ever run in parallel, which is the correct guard here.
func setEnv(t *testing.T, vars map[string]string) {
	t.Helper()

	for _, key := range []string{"PORT", "DATABASE_URL", "MIGRATE_ON_STARTUP", "FRONTEND_DIR"} {
		t.Setenv(key, vars[key])
	}
}

// There is deliberately no default database URL: silently connecting to the
// wrong database is worse than refusing to start.
func TestLoadRequiresDatabaseURL(t *testing.T) {
	setEnv(t, map[string]string{})

	if _, err := Load(); !errors.Is(err, ErrMissingDatabaseURL) {
		t.Fatalf("Load() error = %v, want ErrMissingDatabaseURL", err)
	}
}

func TestLoadDefaults(t *testing.T) {
	setEnv(t, map[string]string{"DATABASE_URL": "postgres://localhost/db"})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.Port != DefaultPort {
		t.Errorf("Port = %q, want %q", cfg.Port, DefaultPort)
	}
	// Off unless explicitly enabled: an unattended restart must not reshape the
	// database on its own.
	if cfg.MigrateOnStartup {
		t.Error("MigrateOnStartup defaulted to true")
	}
	// Empty means the embedded bundle, which is what a deployed binary uses.
	if cfg.FrontendDir != "" {
		t.Errorf("FrontendDir = %q, want empty", cfg.FrontendDir)
	}
}

func TestLoadReadsEveryValue(t *testing.T) {
	setEnv(t, map[string]string{
		"PORT":               "9999",
		"DATABASE_URL":       "postgres://example/db",
		"MIGRATE_ON_STARTUP": "true",
		"FRONTEND_DIR":       "/srv/bundle",
	})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.Port != "9999" {
		t.Errorf("Port = %q, want %q", cfg.Port, "9999")
	}
	if cfg.DatabaseURL != "postgres://example/db" {
		t.Errorf("DatabaseURL = %q", cfg.DatabaseURL)
	}
	if !cfg.MigrateOnStartup {
		t.Error("MigrateOnStartup = false, want true")
	}
	if cfg.FrontendDir != "/srv/bundle" {
		t.Errorf("FrontendDir = %q, want %q", cfg.FrontendDir, "/srv/bundle")
	}
}

// Anything other than exactly "true" leaves migrations off. The safe direction
// is the one a typo lands in.
func TestMigrateOnStartupOnlyAcceptsTrue(t *testing.T) {
	for _, value := range []string{"", "false", "1", "yes", "TRUE", "True", "on"} {
		t.Run("value="+value, func(t *testing.T) {
			setEnv(t, map[string]string{
				"DATABASE_URL":       "postgres://localhost/db",
				"MIGRATE_ON_STARTUP": value,
			})

			cfg, err := Load()
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if cfg.MigrateOnStartup {
				t.Errorf("MIGRATE_ON_STARTUP=%q enabled migrations", value)
			}
		})
	}
}

// An empty PORT must fall back rather than producing a bind address of ":".
func TestEmptyPortFallsBackToDefault(t *testing.T) {
	setEnv(t, map[string]string{
		"DATABASE_URL": "postgres://localhost/db",
		"PORT":         "",
	})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Port != DefaultPort {
		t.Errorf("Port = %q, want %q", cfg.Port, DefaultPort)
	}
}

// A failed Load must not hand back a half-populated Config that a caller could
// mistake for a usable one.
func TestFailedLoadReturnsZeroConfig(t *testing.T) {
	setEnv(t, map[string]string{"PORT": "9999"})

	cfg, err := Load()
	if err == nil {
		t.Fatal("Load succeeded without DATABASE_URL")
	}
	if cfg != (Config{}) {
		t.Errorf("Load returned %+v on error, want the zero Config", cfg)
	}
}
