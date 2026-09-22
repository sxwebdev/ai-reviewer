package cli

import (
	"context"
	"fmt"

	"github.com/tkcrm/mx/logger"
	"github.com/urfave/cli/v3"

	"github.com/sxwebdev/ai-reviewer/internal/app"
	"github.com/sxwebdev/ai-reviewer/internal/migrator"
	"github.com/sxwebdev/ai-reviewer/internal/postgres"
	"github.com/sxwebdev/ai-reviewer/sql"
)

// defaultMigrationsDir is where `migrations create` writes new files. It is a
// source path, not an embedded one: creating a migration is a development
// action, and the file has to land in the tree before it can be embedded.
const defaultMigrationsDir = "./sql/migrations"

func dsnFlag() *cli.StringFlag {
	return &cli.StringFlag{
		Name:    "dsn",
		Usage:   "PostgreSQL DSN (defaults to the one assembled from the config)",
		Sources: cli.EnvVars("AI_REVIEWER_POSTGRES_DSN"),
	}
}

func migrationsCommand(boot logger.ExtendedLogger) *cli.Command {
	return &cli.Command{
		Name:  "migrations",
		Usage: "Manage the database schema",
		Commands: []*cli.Command{
			{
				Name:  "up",
				Usage: "Apply all pending migrations (application schema, then River's)",
				Flags: []cli.Flag{dsnFlag()},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					return withMigrationPool(ctx, boot, cmd, func(a *app.App, pg *postgres.Postgres) error {
						if err := a.Migrate(ctx, pg.Pool); err != nil {
							return err
						}
						fmt.Println("migrations applied (application schema + river)")
						return nil
					})
				},
			},
			{
				Name: "down",
				// Only the application's schema rolls back. River's migrator
				// versions its own tables and rolling them back under a running
				// service would discard queued jobs — an explicit, separate act,
				// not something `down` should do as a side effect.
				Usage: "Roll back the last applied application migration",
				Flags: []cli.Flag{dsnFlag()},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					return withMigrationPool(ctx, boot, cmd, func(a *app.App, pg *postgres.Postgres) error {
						m := migrator.New(a.Log, sql.MigrationsFS, sql.MigrationsPath, migrator.DataMigrations{})
						if err := m.MigrateDownLast(ctx, pg.Pool); err != nil {
							return err
						}
						fmt.Println("rolled back the last application migration")
						return nil
					})
				},
			},
			{
				Name:  "create",
				Usage: "Create an empty up/down migration pair",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "path", Aliases: []string{"p"}, Value: defaultMigrationsDir, Usage: "Migrations directory"},
					&cli.StringFlag{Name: "name", Required: true, Usage: "Migration name"},
				},
				Action: func(_ context.Context, cmd *cli.Command) error {
					base, err := migrator.Create(cmd.String("path"), cmd.String("name"))
					if err != nil {
						return err
					}
					fmt.Printf("created %s.up.sql / %s.down.sql in %s\n", base, base, cmd.String("path"))
					return nil
				},
			},
		},
	}
}

// withMigrationPool opens a pool from --dsn, or from the config when no DSN was
// given.
//
// The --dsn path deliberately skips config validation: migrations run from an
// init container that has a database URL and nothing else — no GitLab token, no
// Slack token, no teams. Requiring a complete config there would make the
// schema depend on credentials it never touches.
func withMigrationPool(ctx context.Context, boot logger.ExtendedLogger, cmd *cli.Command, fn func(*app.App, *postgres.Postgres) error) error {
	dsn := cmd.String("dsn")

	if dsn != "" {
		a, err := app.Minimal(options(cmd))
		if err != nil {
			return err
		}
		pg, err := postgres.New(ctx, dsn)
		if err != nil {
			return fmt.Errorf("connect postgres: %w", err)
		}
		defer func() { _ = pg.Stop(ctx) }()
		return fn(a, pg)
	}

	a, err := open(ctx, boot, cmd)
	if err != nil {
		return err
	}
	defer func() { _ = a.Close() }()

	pg, err := a.OpenPostgres(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = pg.Stop(ctx) }()
	return fn(a, pg)
}
