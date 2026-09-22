// Package app is the composition root: it turns CLI input into a running
// service. Configuration and logging are built here (app.go), the runtime graph
// — Postgres pool, store, GitLab/Slack clients, review engine, git cache, the
// service layer and the River client — in runtime.go, and the mx launcher that
// owns their lifecycle in start.go. doctor.go holds the checks that need no
// infrastructure; doctor_service.go those that do.
package app

import (
	"context"

	"github.com/sxwebdev/ai-reviewer/internal/config"
	"github.com/tkcrm/mx/logger"
)

// Options are the process-wide switches the CLI's global flags map to.
type Options struct {
	// ConfigPaths are the YAML files to read, in order. Empty means
	// config.DefaultConfigPath. Missing files are not an error: a deployment
	// configured purely through env and Vault has none.
	ConfigPaths []string
	// Debug forces the log level to debug regardless of what the file says.
	Debug bool
}

// App holds the wired dependencies shared across commands.
type App struct {
	Config *config.Config
	Log    logger.ExtendedLogger

	cleanup func()
}

// New loads the configuration and builds the application logger.
//
// boot is the bootstrap logger built in main before the CLI tree exists; it is
// used only for events that happen during the load itself (Vault). Everything
// afterwards uses App.Log, which is configured from the file/env/Vault result.
func New(ctx context.Context, boot logger.Logger, opts Options) (*App, error) {
	paths := opts.ConfigPaths
	if len(paths) == 0 {
		paths = []string{config.DefaultConfigPath}
	}

	cfg, err := config.Default()
	if err != nil {
		return nil, err
	}
	res, err := config.Load(ctx, boot, cfg, paths)
	if err != nil {
		return nil, err
	}

	// The flag wins over the file: --debug exists precisely for the case where
	// the configured level is hiding what you need to see.
	if opts.Debug {
		cfg.Log.Level = logger.LogLevelDebug
	}

	return &App{
		Config:  cfg,
		Log:     NewLogger(cfg.Log),
		cleanup: res.Cleanup,
	}, nil
}

// Minimal builds an App with default configuration and a real logger, without
// reading or validating anything.
//
// It exists for `migrations up --dsn …`, which runs from an init container that
// has a database URL and nothing else: no GitLab token, no Slack token, no
// teams. Requiring the full config there would make applying the schema depend
// on credentials the schema never touches.
func Minimal(opts Options) (*App, error) {
	cfg, err := config.Default()
	if err != nil {
		return nil, err
	}
	if opts.Debug {
		cfg.Log.Level = logger.LogLevelDebug
	}
	return &App{Config: cfg, Log: NewLogger(cfg.Log)}, nil
}

// Close releases resources acquired during the load (the Vault client and its
// token renewer). Safe to call more than once.
func (a *App) Close() error {
	if a.cleanup != nil {
		a.cleanup()
		a.cleanup = nil
	}
	if a.Log != nil {
		// Flushing is best-effort: on a plain terminal Sync returns EINVAL for
		// stdout, which is noise, not a failure.
		_ = a.Log.Sync()
	}
	return nil
}
