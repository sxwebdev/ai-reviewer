// Package cli is the thin urfave/cli v3 launcher. It parses flags, loads
// config, builds the application, and dispatches. Business logic lives in
// internal/app, internal/jobs and the service layer, not here.
//
// The shape of the command set follows §15: with the single exception of
// `review --local`, the CLI *enqueues* work rather than doing it, and a running
// `start` executes it. That keeps one implementation of every operation and
// makes a manual run subject to the same uniqueness, retries and metrics as a
// scheduled one.
package cli

import (
	"context"
	"fmt"

	"github.com/sxwebdev/ai-reviewer/internal/app"
	"github.com/sxwebdev/ai-reviewer/internal/jobs"
	"github.com/sxwebdev/ai-reviewer/internal/version"
	"github.com/tkcrm/mx/logger"
	"github.com/urfave/cli/v3"
)

// NewApp builds the root command tree. boot is the bootstrap logger built in
// main before any command runs; commands pass it to app.New, which uses it for
// events that happen while the real logger's configuration is still being
// resolved.
func NewApp(boot logger.ExtendedLogger) *cli.Command {
	return &cli.Command{
		Name:    app.AppName,
		Usage:   "Team AI code review for GitLab merge requests",
		Version: version.String(),
		Suggest: true,
		Flags: []cli.Flag{
			&cli.StringSliceFlag{
				Name:    "config",
				Aliases: []string{"c"},
				Usage:   "Config file(s), applied in order (default config.yaml)",
				Sources: cli.EnvVars("AI_REVIEWER_CONFIG"),
			},
			&cli.BoolFlag{
				Name:  "debug",
				Usage: "Force debug-level logging regardless of log.level",
			},
		},
		Commands: []*cli.Command{
			startCommand(boot),
			scanCommand(boot),
			reviewCommand(boot),
			digestCommand(boot),
			doctorCommand(boot),
			migrationsCommand(boot),
		},
	}
}

// options maps the global flags to app.Options.
func options(cmd *cli.Command) app.Options {
	return app.Options{
		ConfigPaths: cmd.StringSlice("config"),
		Debug:       cmd.Bool("debug"),
	}
}

// open loads the config and builds the App. Every command except `doctor` and
// `migrations --dsn` starts here: a config that does not validate is a hard
// failure, because the alternative is a service that runs against a half-read
// configuration and reviews the wrong repositories.
func open(ctx context.Context, boot logger.ExtendedLogger, cmd *cli.Command) (*app.App, error) {
	a, err := app.New(ctx, boot, options(cmd))
	if err != nil {
		return nil, err
	}
	return a, nil
}

// teamFlag is shared by `scan` and `digest`.
func teamFlag() *cli.StringFlag {
	return &cli.StringFlag{
		Name:  "team",
		Usage: "Limit the operation to one configured team",
	}
}

func startCommand(boot logger.ExtendedLogger) *cli.Command {
	return &cli.Command{
		Name:  "start",
		Usage: "Run the service: River workers, periodic jobs and the ops server",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			a, err := open(ctx, boot, cmd)
			if err != nil {
				return err
			}
			defer func() { _ = a.Close() }()
			return a.Start(ctx)
		},
	}
}

func scanCommand(boot logger.ExtendedLogger) *cli.Command {
	return &cli.Command{
		Name:  "scan",
		Usage: "Enqueue a scan pass (a running `start` executes it)",
		Flags: []cli.Flag{teamFlag()},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			a, err := open(ctx, boot, cmd)
			if err != nil {
				return err
			}
			defer func() { _ = a.Close() }()

			team := cmd.String("team")
			if team != "" {
				if _, ok := app.TeamByName(a.Config, team); !ok {
					return fmt.Errorf("no configured team named %q", team)
				}
			}

			pg, q, err := a.Queue(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = pg.Stop(ctx) }()

			res, err := q.EnqueueScan(ctx, jobs.ScanArgs{Team: team})
			if err != nil {
				return err
			}
			if res.Deduplicated {
				// §6.2 keys scan uniqueness on the kind alone, so a team-scoped
				// request collapses into a full pass that is already running.
				// Saying so is the difference between "your narrower scan was
				// dropped" and a silent success.
				fmt.Printf("a scan is already in flight (job %d); this request was folded into it\n", res.ID())
				return nil
			}
			fmt.Printf("queued scan job %d\n", res.ID())
			return nil
		},
	}
}
