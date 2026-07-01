// Package app is the composition root: it wires configuration, logging, state,
// clients, and the service layer together, and exposes lifecycle entrypoints
// used by the CLI commands.
package app

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/sxwebdev/ai-reviewer/internal/config"
	"github.com/sxwebdev/ai-reviewer/internal/security"
	"github.com/sxwebdev/ai-reviewer/internal/state"
)

// App holds the wired dependencies shared across commands. Fields are added as
// subsystems come online (state, gitlab, git, index, review, jobs, services).
type App struct {
	Cfg *config.Config
	Log *slog.Logger
	DB  *state.DB
}

// New constructs an App from an already-loaded config and logger. It registers
// resolved secrets with the redactor so they never appear in logs.
func New(cfg *config.Config, log *slog.Logger) (*App, error) {
	if tok := cfg.GitLabToken(); tok != "" {
		security.RegisterSecret(tok)
	}
	return &App{Cfg: cfg, Log: log}, nil
}

// OpenState opens (creating the parent directory) and migrates the SQLite
// database, caching the handle on the App. It is idempotent.
func (a *App) OpenState() (*state.DB, error) {
	if a.DB != nil {
		return a.DB, nil
	}
	if err := os.MkdirAll(filepath.Dir(a.Cfg.Storage.DBPath), 0o755); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}
	db, err := state.Open(a.Cfg.Storage.DBPath)
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
