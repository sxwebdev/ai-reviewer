// Package migrator applies the embedded SQL migrations, serialized across
// replicas by a Postgres advisory lock.
package migrator

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tkcrm/mx/logger"
)

// DataMigrations maps a migration name (e.g. "0001_init") to a Go data
// migration executed in the same transaction right after the SQL migration.
type DataMigrations map[string]func(ctx context.Context, tx pgx.Tx) error

type migration struct {
	version int
	name    string
	upSQL   string
	downSQL string
}

type Migrator struct {
	logger logger.Logger
	fsys   embed.FS
	dir    string
	data   DataMigrations
}

func New(l logger.Logger, fsys embed.FS, dir string, data DataMigrations) *Migrator {
	return &Migrator{logger: l, fsys: fsys, dir: dir, data: data}
}

var migrationFileRe = regexp.MustCompile(`^(\d+)_(.+)\.(up|down)\.sql$`)

func (m *Migrator) load() ([]migration, error) {
	entries, err := fs.ReadDir(m.fsys, m.dir)
	if err != nil {
		return nil, fmt.Errorf("failed to read migrations dir: %w", err)
	}

	byVersion := map[int]*migration{}
	for _, e := range entries {
		match := migrationFileRe.FindStringSubmatch(e.Name())
		if match == nil {
			continue
		}

		version, err := strconv.Atoi(match[1])
		if err != nil {
			return nil, fmt.Errorf("invalid migration version in %q: %w", e.Name(), err)
		}

		content, err := fs.ReadFile(m.fsys, path.Join(m.dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("failed to read migration %q: %w", e.Name(), err)
		}

		mig, ok := byVersion[version]
		if !ok {
			mig = &migration{version: version, name: match[1] + "_" + match[2]}
			byVersion[version] = mig
		}

		if match[3] == "up" {
			mig.upSQL = string(content)
		} else {
			mig.downSQL = string(content)
		}
	}

	migrations := make([]migration, 0, len(byVersion))
	for _, mig := range byVersion {
		if mig.upSQL == "" {
			return nil, fmt.Errorf("migration %q has no .up.sql file", mig.name)
		}
		migrations = append(migrations, *mig)
	}

	sort.Slice(migrations, func(i, j int) bool {
		return migrations[i].version < migrations[j].version
	})

	return migrations, nil
}

func (m *Migrator) ensureTable(ctx context.Context, db *pgxpool.Pool) error {
	_, err := db.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version integer PRIMARY KEY,
		name text NOT NULL,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`)
	return err
}

func (m *Migrator) appliedVersions(ctx context.Context, db *pgxpool.Pool) (map[int]struct{}, error) {
	rows, err := db.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	applied := map[int]struct{}{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		applied[v] = struct{}{}
	}
	return applied, rows.Err()
}

// migrationsLock identifies the session advisory lock that serializes
// MigrateUpAll across replicas. The two-int form occupies a lock space of its
// own, distinct from any single-bigint lock the application may take (the CLI's
// `review --local` guard, for one), so the two can never collide.
const (
	migrationsLockClass int32 = 0x4149 // "AI"
	migrationsLockObj   int32 = 0x5256 // "RV"
)

// MigrateUpAll applies all pending migrations.
func (m *Migrator) MigrateUpAll(ctx context.Context, db *pgxpool.Pool) error {
	// Serialize across replicas: without this lock two pods starting together
	// both see the same pending set and both run the non-idempotent .up.sql, and
	// the loser aborts startup. Held on a dedicated connection for the whole
	// apply loop; if this process dies, Postgres releases the session lock and
	// the next waiter proceeds.
	conn, err := db.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration lock connection: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1, $2)`, migrationsLockClass, migrationsLockObj); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		// Detach from ctx so the unlock runs even if startup was cancelled.
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1, $2)`, migrationsLockClass, migrationsLockObj)
	}()

	if err := m.ensureTable(ctx, db); err != nil {
		return fmt.Errorf("failed to ensure schema_migrations table: %w", err)
	}

	migrations, err := m.load()
	if err != nil {
		return err
	}

	applied, err := m.appliedVersions(ctx, db)
	if err != nil {
		return fmt.Errorf("failed to read applied migrations: %w", err)
	}

	for _, mig := range migrations {
		if _, ok := applied[mig.version]; ok {
			continue
		}

		if err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, mig.upSQL); err != nil {
				return err
			}
			if dataFn, ok := m.data[mig.name]; ok {
				if err := dataFn(ctx, tx); err != nil {
					return fmt.Errorf("data migration failed: %w", err)
				}
			}
			_, err := tx.Exec(
				ctx,
				`INSERT INTO schema_migrations (version, name) VALUES ($1, $2)`,
				mig.version, mig.name,
			)
			return err
		}); err != nil {
			return fmt.Errorf("migration %q failed: %w", mig.name, err)
		}

		m.logger.Infof("applied migration %s", mig.name)
	}

	return nil
}

// MigrateDownLast rolls back the most recently applied migration.
func (m *Migrator) MigrateDownLast(ctx context.Context, db *pgxpool.Pool) error {
	if err := m.ensureTable(ctx, db); err != nil {
		return fmt.Errorf("failed to ensure schema_migrations table: %w", err)
	}

	var version int
	err := db.QueryRow(
		ctx,
		`SELECT version FROM schema_migrations ORDER BY version DESC LIMIT 1`,
	).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		m.logger.Infof("no migrations to roll back")
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to read last applied migration: %w", err)
	}

	migrations, err := m.load()
	if err != nil {
		return err
	}

	for _, mig := range migrations {
		if mig.version != version {
			continue
		}
		if mig.downSQL == "" {
			return fmt.Errorf("migration %q has no .down.sql file", mig.name)
		}

		if err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, mig.downSQL); err != nil {
				return err
			}
			_, err := tx.Exec(
				ctx,
				`DELETE FROM schema_migrations WHERE version = $1`, mig.version,
			)
			return err
		}); err != nil {
			return fmt.Errorf("rollback of %q failed: %w", mig.name, err)
		}

		m.logger.Infof("rolled back migration %s", mig.name)
		return nil
	}

	return fmt.Errorf("migration with version %d not found in migrations dir", version)
}

// MigrateDropAll drops every database object by resetting the public schema
// (including schema_migrations and River's own tables). Unlike MigrateDownLast
// it does not depend on the down files — a clean slate even when migrations were
// hand-edited. This is destructive (discards all data); follow with MigrateUpAll
// plus River's own migrator to rebuild.
func (m *Migrator) MigrateDropAll(ctx context.Context, db *pgxpool.Pool) error {
	if _, err := db.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;`); err != nil {
		return fmt.Errorf("drop schema: %w", err)
	}
	m.logger.Infof("dropped all database objects (public schema reset)")
	return nil
}
