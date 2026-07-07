// Package app is the composition root: it wires configuration, logging, state,
// clients, and the service layer together, and exposes lifecycle entrypoints
// used by the CLI commands.
package app

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/sxwebdev/ai-reviewer/internal/config"
	"github.com/sxwebdev/ai-reviewer/internal/security"
	"github.com/sxwebdev/ai-reviewer/internal/server"
	"github.com/sxwebdev/ai-reviewer/internal/service"
	"github.com/sxwebdev/ai-reviewer/internal/state"
)

// App holds the wired dependencies shared across commands. Config and the
// service bundle are held behind atomic pointers so the web UI can hot-apply
// config changes (setup screen, header switches) without a restart.
type App struct {
	Log *slog.Logger
	DB  *state.DB

	// ConfigPath is the config file the app was loaded from (and that runtime
	// changes are persisted to).
	ConfigPath string

	cfg     atomic.Pointer[config.Config]
	bundle  atomic.Pointer[service.Bundle]
	uiSnap  atomic.Pointer[server.UIConfig] // derived from cfg on every store; read per request
	applyMu sync.Mutex                      // serializes patch-file → reload → rebuild

	// overrides re-applies CLI flag mutations (e.g. serve --host/--port) after
	// every config reload, so hot applies don't silently drop them.
	overrides func(*config.Config)

	// claudeProbe caches a successful claude-CLI lookup (bin → resolved path).
	// Positive-only: a miss is re-probed every time so the setup Re-check flow
	// sees a fresh install without a restart.
	claudeProbe atomic.Pointer[claudeLookup]
}

type claudeLookup struct{ bin, path string }

// New constructs an App from an already-loaded config and logger. configPath
// is where runtime config changes are persisted (empty = default path). It
// registers resolved secrets with the redactor so they never appear in logs.
func New(cfg *config.Config, configPath string, log *slog.Logger) (*App, error) {
	if tok := cfg.GitLabToken(); tok != "" {
		security.RegisterSecret(tok)
	}
	if configPath == "" {
		configPath = config.DefaultConfigPath()
	}
	a := &App{Log: log, ConfigPath: configPath}
	a.storeConfig(cfg)
	return a, nil
}

// Config returns the current runtime config. Until a hot apply happens this is
// the pointer passed to New, so pre-start CLI flag mutations are visible.
func (a *App) Config() *config.Config { return a.cfg.Load() }

// storeConfig swaps the runtime config and refreshes the derived UI snapshot.
func (a *App) storeConfig(cfg *config.Config) {
	a.cfg.Store(cfg)
	ui := uiConfigFrom(cfg)
	a.uiSnap.Store(&ui)
}

// SetOverrides registers CLI flag overrides: fn is applied to the current
// config immediately and re-applied after every hot config reload.
func (a *App) SetOverrides(fn func(*config.Config)) {
	a.overrides = fn
	if fn != nil {
		cfg := a.Config()
		fn(cfg)
		a.storeConfig(cfg)
	}
}

// Bundle returns the current service bundle. Nil until Services() has run.
func (a *App) Bundle() *service.Bundle { return a.bundle.Load() }

// OpenState creates the data, cache, and database directories, then opens and
// migrates the SQLite database, caching the handle on the App. It is
// idempotent. This is what makes every entrypoint self-initializing — there is
// no separate init command.
func (a *App) OpenState() (*state.DB, error) {
	if a.DB != nil {
		return a.DB, nil
	}
	cfg := a.Config()
	for _, dir := range []string{cfg.App.DataDir, cfg.Storage.CacheDir, filepath.Dir(cfg.Storage.DBPath)} {
		if dir == "" {
			continue
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create dir %s: %w", dir, err)
		}
	}
	db, err := state.Open(cfg.Storage.DBPath)
	if err != nil {
		return nil, err
	}
	if err := db.Migrate(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	a.DB = db
	return db, nil
}

// Close releases resources (DB handle, etc.). Safe to call multiple times.
func (a *App) Close() error {
	if a.DB != nil {
		err := a.DB.Close()
		a.DB = nil
		return err
	}
	return nil
}
