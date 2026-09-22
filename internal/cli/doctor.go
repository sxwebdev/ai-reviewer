package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/sxwebdev/ai-reviewer/internal/app"
	"github.com/sxwebdev/ai-reviewer/internal/config"
	"github.com/tkcrm/mx/logger"
	"github.com/urfave/cli/v3"
)

func doctorCommand(boot logger.ExtendedLogger) *cli.Command {
	return &cli.Command{
		Name:  "doctor",
		Usage: "Check the configuration, the local environment and every external dependency",
		Flags: []cli.Flag{
			&cli.BoolFlag{
				Name:  "local",
				Usage: "Skip the network probes (Postgres, GitLab, Slack)",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			// Doctor is the one command that must survive a broken config: a
			// hard failure here would hide the environment checks that explain
			// what is wrong. So the load error becomes a check, not an exit.
			in := app.DoctorInput{}
			a, err := app.New(ctx, boot, options(cmd))
			if err != nil {
				in.ConfigErr = err
				// Default's own error is joined rather than dropped: a malformed
				// `default:` tag would otherwise look like the operator's mistake.
				cfg, derr := config.Default()
				in.Config = cfg
				if derr != nil {
					in.ConfigErr = errors.Join(err, derr)
				}
			} else {
				defer func() { _ = a.Close() }()
				in.Config = a.Config
			}

			checks := app.Doctor(ctx, in)

			// The service probes need a config that actually loaded. Running
			// them against the defaults would report a failed connection to
			// localhost, which tells an operator nothing the config check above
			// has not already said.
			if err == nil && !cmd.Bool("local") {
				checks = append(checks, a.ServiceChecks(ctx)...)
			}

			var failed int
			for _, c := range checks {
				fmt.Printf("%s %-20s %s\n", symbol(c.Status), c.Name, c.Detail)
				if c.Status == app.StatusFail {
					failed++
				}
			}
			if failed > 0 {
				return cli.Exit(fmt.Sprintf("%d check(s) failed", failed), 1)
			}
			return nil
		},
	}
}

func symbol(s app.CheckStatus) string {
	switch s {
	case app.StatusOK:
		return "[ OK ]"
	case app.StatusWarn:
		return "[WARN]"
	default:
		return "[FAIL]"
	}
}
