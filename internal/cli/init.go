package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sxwebdev/ai-reviewer/internal/app"
	"github.com/sxwebdev/ai-reviewer/internal/config"
	"github.com/urfave/cli/v3"
)

func initCommand() *cli.Command {
	return &cli.Command{
		Name:  "init",
		Usage: "Create config, data directory, and SQLite database",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "force", Usage: "Overwrite an existing config file"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			path := cmd.String("config")
			if path == "" {
				path = config.DefaultConfigPath()
			}

			if cmd.Bool("force") {
				_ = os.Remove(path)
			}
			if err := config.WriteDefaultFile(path); err != nil {
				return fmt.Errorf("write config: %w", err)
			}
			fmt.Printf("Wrote config: %s\n", path)

			// Load it back to resolve data/cache directories and create them.
			cfg, err := config.Load(path)
			if err != nil {
				return fmt.Errorf("load new config: %w", err)
			}
			for _, dir := range []string{cfg.App.DataDir, cfg.Storage.CacheDir, filepath.Dir(cfg.Storage.DBPath)} {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					return fmt.Errorf("create %s: %w", dir, err)
				}
			}
			fmt.Printf("Data dir:  %s\n", cfg.App.DataDir)
			fmt.Printf("Cache dir: %s\n", cfg.Storage.CacheDir)

			// Create and migrate the database.
			log := app.NewLogger(cmd.Bool("debug"))
			a, err := app.New(cfg, log)
			if err != nil {
				return err
			}
			defer a.Close()
			if _, err := a.OpenState(); err != nil {
				return fmt.Errorf("initialize database: %w", err)
			}
			fmt.Printf("DB path:   %s (migrated)\n", cfg.Storage.DBPath)

			fmt.Println("\nNext steps:")
			fmt.Printf("  1. Edit %s: set gitlab.host, gitlab.username, and gitlab.token (your PAT, scope: api)\n", path)
			fmt.Println("     The config file is created with 0600 perms; the token stays on this machine.")
			fmt.Println("  2. Ensure the `claude` CLI is installed and logged in")
			fmt.Println("  3. Run: ai-reviewer doctor")
			fmt.Println("  4. Run: ai-reviewer serve")
			return nil
		},
	}
}
