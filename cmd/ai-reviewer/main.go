// Command ai-reviewer is the team AI code-review service for GitLab merge
// requests.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/sxwebdev/ai-reviewer/internal/app"
	"github.com/sxwebdev/ai-reviewer/internal/cli"
	"github.com/sxwebdev/ai-reviewer/internal/config"
	"github.com/sxwebdev/ai-reviewer/internal/security"
	"github.com/sxwebdev/xconfig"
	"github.com/sxwebdev/xconfig/decoders/xconfigdotenv"
	"github.com/sxwebdev/xconfig/decoders/xconfigyaml"
	"github.com/sxwebdev/xconfig/plugins/loader"
	"github.com/tkcrm/mx/launcher"
	"github.com/tkcrm/mx/logger"
)

// bootstrapLogger builds the logger used before (and during) the main config
// load: Vault events and config errors have to go somewhere, and the real
// logger's own settings are part of what that load resolves.
//
// It reads only the log section, from the default file locations, and skips
// flags — otherwise xconfig's flag plugin would fight urfave/cli over os.Args.
func bootstrapLogger() (logger.ExtendedLogger, error) {
	ld, err := loader.NewLoader(map[string]loader.Unmarshal{
		"yaml": xconfigyaml.New().Unmarshal,
		"env":  xconfigdotenv.New().Unmarshal,
	})
	if err != nil {
		return nil, fmt.Errorf("create config loader: %w", err)
	}
	if err := ld.AddFiles([]string{".env", config.DefaultConfigPath}, true); err != nil {
		return nil, fmt.Errorf("add config files: %w", err)
	}

	var cfg struct {
		Log logger.Config
	}
	if _, err := xconfig.Load(&cfg,
		xconfig.WithSkipFlags(),
		xconfig.WithEnvPrefix(config.EnvPrefix),
		xconfig.WithLoader(ld),
	); err != nil {
		return nil, fmt.Errorf("load logger config: %w", err)
	}

	return app.NewLogger(cfg.Log), nil
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), launcher.ShutdownSiganl()...)
	defer cancel()

	l, err := bootstrapLogger()
	if err != nil {
		logger.Default().Fatalf("failed to build logger: %s", err)
	}

	if err := cli.NewApp(l).Run(ctx, os.Args); err != nil {
		l.Fatal(security.Mask(err.Error()))
	}
}
