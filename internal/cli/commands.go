// Package cli is the thin urfave/cli v3 launcher. It parses flags, loads
// config, builds the application, and dispatches to the app's lifecycle
// entrypoints. Business logic lives in internal/app and the service layer, not
// here.
package cli

import (
	"context"
	"fmt"

	"github.com/sxwebdev/ai-reviewer/internal/app"
	"github.com/sxwebdev/ai-reviewer/internal/config"
	"github.com/sxwebdev/ai-reviewer/internal/version"
	"github.com/urfave/cli/v3"
)

// NewApp builds the root command tree.
func NewApp() *cli.Command {
	return &cli.Command{
		Name:    "ai-reviewer",
		Usage:   "Local AI code review for GitLab merge requests",
		Version: version.String(),
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "config",
				Aliases: []string{"c"},
				Usage:   "Path to config file (default ~/.ai-reviewer/config.yaml)",
				Sources: cli.EnvVars("AI_REVIEWER_CONFIG"),
			},
			&cli.BoolFlag{
				Name:  "debug",
				Usage: "Enable debug logging",
			},
		},
		Commands: []*cli.Command{
			serveCommand(),
			daemonCommand(),
			syncCommand(),
			reviewCommand(),
			doctorCommand(),
		},
	}
}

// bootstrap loads config (with CLI overrides) and constructs the App + logger.
// There is no init step: directories and the database are created on first
// use, and the web UI walks the user through the required settings.
func bootstrap(cmd *cli.Command) (*app.App, error) {
	path := cmd.String("config")
	cfg, err := config.Load(path)
	if err != nil {
		shown := path
		if shown == "" {
			shown = config.DefaultConfigPath()
		}
		// With `init --force` gone this is the only recovery hint for a broken
		// config file — without it every command dies here before any UI.
		return nil, fmt.Errorf("%w\nfix or delete %s, then run 'ai-reviewer serve' to reconfigure via the web UI", err, shown)
	}
	log := app.NewLogger(cmd.Bool("debug"))
	return app.New(cfg, path, log)
}

func serveCommand() *cli.Command {
	return &cli.Command{
		Name:    "serve",
		Usage:   "Start the local web UI and background worker",
		Aliases: []string{"start"},
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "host", Usage: "Bind host"},
			&cli.IntFlag{Name: "port", Usage: "Bind port (0 = random)", Value: -1},
			&cli.BoolFlag{Name: "open", Usage: "Open the browser", Value: true},
			&cli.BoolFlag{Name: "daemon", Usage: "Run the background worker", Value: true},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			a, err := bootstrap(cmd)
			if err != nil {
				return err
			}
			defer a.Close()
			// Registered as overrides (not one-off mutations) so hot config
			// reloads triggered from the web UI keep the flag values.
			a.SetOverrides(func(cfg *config.Config) {
				if h := cmd.String("host"); h != "" {
					cfg.App.BindHost = h
				}
				if p := cmd.Int("port"); p >= 0 {
					cfg.App.Port = p
				}
				cfg.App.OpenBrowser = cmd.Bool("open")
			})
			return a.Serve(ctx, app.ServeOptions{RunWorker: cmd.Bool("daemon")})
		},
	}
}

func daemonCommand() *cli.Command {
	return &cli.Command{
		Name:  "daemon",
		Usage: "Run the background watch/review worker without the web UI",
		Flags: []cli.Flag{
			&cli.DurationFlag{Name: "interval", Usage: "Watch interval"},
			&cli.BoolFlag{Name: "foreground", Usage: "Run in foreground", Value: true},
			&cli.BoolFlag{Name: "auto-review", Usage: "Auto-run review (local report only)", Value: true},
			&cli.BoolFlag{Name: "auto-draft", Usage: "Allow auto-create of GitLab draft notes"},
			&cli.BoolFlag{Name: "auto-publish", Usage: "DANGER: allow auto-publish"},
			&cli.IntFlag{Name: "max-parallel", Usage: "Max parallel jobs", Value: -1},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			a, err := bootstrap(cmd)
			if err != nil {
				return err
			}
			defer a.Close()
			a.SetOverrides(func(cfg *config.Config) { applyDaemonFlags(cfg, cmd) })
			return a.RunDaemon(ctx)
		},
	}
}

func applyDaemonFlags(cfg *config.Config, cmd *cli.Command) {
	if d := cmd.Duration("interval"); d > 0 {
		cfg.Watch.Interval = d
	}
	if n := cmd.Int("max-parallel"); n > 0 {
		cfg.Watch.MaxParallel = n
	}
	cfg.Review.AutoReview = cmd.Bool("auto-review")
	cfg.Review.AutoDraft = cmd.Bool("auto-draft")
	cfg.Review.AutoPublish = cmd.Bool("auto-publish")
}

func syncCommand() *cli.Command {
	return &cli.Command{
		Name:  "sync",
		Usage: "One-shot sync of merge requests assigned to you for review",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			a, err := bootstrap(cmd)
			if err != nil {
				return err
			}
			defer a.Close()
			return a.SyncOnce(ctx)
		},
	}
}

func reviewCommand() *cli.Command {
	return &cli.Command{
		Name:      "review",
		Usage:     "One-shot review of a single MR (local report by default)",
		ArgsUsage: "<mr-url | project-path!iid | project-id:iid>",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			ref := cmd.Args().First()
			if ref == "" {
				return cli.Exit("missing MR reference argument", 2)
			}
			a, err := bootstrap(cmd)
			if err != nil {
				return err
			}
			defer a.Close()
			return a.ReviewOnce(ctx, ref)
		},
	}
}
