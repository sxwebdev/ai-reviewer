package cli

import (
	"context"
	"fmt"

	"github.com/sxwebdev/ai-reviewer/internal/app"
	"github.com/urfave/cli/v3"
)

func doctorCommand() *cli.Command {
	return &cli.Command{
		Name:  "doctor",
		Usage: "Check GitLab auth, Claude, git, and local setup",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			a, err := bootstrap(cmd)
			if err != nil {
				return err
			}
			defer a.Close()

			checks := a.Doctor(ctx)
			var failed int
			for _, c := range checks {
				fmt.Printf("%s %-16s %s\n", symbol(c.Status), c.Name, c.Detail)
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
