package migrator_test

import (
	"context"
	"embed"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tkcrm/mx/logger"

	"github.com/sxwebdev/ai-reviewer/internal/migrator"
)

//go:embed testdata/good/*.sql
var goodFS embed.FS

//go:embed testdata/missing_up/*.sql
var missingUpFS embed.FS

//go:embed testdata/no_down/*.sql
var noDownFS embed.FS

const dsnEnv = "AI_REVIEWER_TEST_PG_DSN"

// scratchPool gives each test its own database on the server behind
// AI_REVIEWER_TEST_PG_DSN: these tests drop schemas and roll migrations back, so
// they must not share the database the store tests (or another developer) use.
func scratchPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv(dsnEnv)
	if dsn == "" {
		t.Skipf("set %s (a throwaway database) to run migrator integration tests", dsnEnv)
	}
	ctx := t.Context()

	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect %s: %v", dsnEnv, err)
	}
	defer admin.Close()

	name := "aireviewer_mig_" + strings.ToLower(strings.NewReplacer("/", "", " ", "").Replace(t.Name()))
	if len(name) > 60 {
		name = name[:60]
	}
	if _, err := admin.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %q WITH (FORCE)`, name)); err != nil {
		t.Fatalf("drop scratch database: %v", err)
	}
	if _, err := admin.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %q`, name)); err != nil {
		t.Fatalf("create scratch database: %v", err)
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect scratch database: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		// Detached from t.Context, which is already cancelled by now.
		cleanup, err := pgxpool.New(context.WithoutCancel(context.Background()), dsn)
		if err != nil {
			return
		}
		defer cleanup.Close()
		_, _ = cleanup.Exec(context.Background(), fmt.Sprintf(`DROP DATABASE IF EXISTS %q WITH (FORCE)`, name))
	})
	return pool
}

func newMigrator(fsys embed.FS, dir string, data migrator.DataMigrations) *migrator.Migrator {
	return migrator.New(logger.New(), fsys, dir, data)
}

func TestMigrateUpDownUp(t *testing.T) {
	pool := scratchPool(t)
	m := newMigrator(goodFS, "testdata/good", migrator.DataMigrations{})

	if err := m.MigrateUpAll(t.Context(), pool); err != nil {
		t.Fatalf("up: %v", err)
	}
	assertApplied(t, pool, 2)
	assertColumn(t, pool, "extra", true)

	// Re-running must be a no-op, not a second apply: the .up.sql are not
	// idempotent (ALTER TABLE ADD COLUMN would fail).
	if err := m.MigrateUpAll(t.Context(), pool); err != nil {
		t.Fatalf("second up: %v", err)
	}
	assertApplied(t, pool, 2)

	if err := m.MigrateDownLast(t.Context(), pool); err != nil {
		t.Fatalf("down: %v", err)
	}
	assertApplied(t, pool, 1)
	assertColumn(t, pool, "extra", false)

	if err := m.MigrateDownLast(t.Context(), pool); err != nil {
		t.Fatalf("second down: %v", err)
	}
	assertApplied(t, pool, 0)
	assertTable(t, pool, "mig_probe", false)

	// Rolling back with nothing left is not an error — the CLI must be safe to
	// run twice.
	if err := m.MigrateDownLast(t.Context(), pool); err != nil {
		t.Fatalf("down on an empty ledger: %v", err)
	}

	if err := m.MigrateUpAll(t.Context(), pool); err != nil {
		t.Fatalf("up again: %v", err)
	}
	assertApplied(t, pool, 2)
	assertColumn(t, pool, "extra", true)
}

// A data migration runs inside the same transaction as its SQL, so a failure in
// either leaves the ledger untouched.
func TestMigrateUpRunsDataMigrationsInTheSameTransaction(t *testing.T) {
	pool := scratchPool(t)

	calls := 0
	m := newMigrator(goodFS, "testdata/good", migrator.DataMigrations{
		"0001_probe": func(ctx context.Context, tx pgx.Tx) error {
			calls++
			_, err := tx.Exec(ctx, `INSERT INTO mig_probe (id, note) VALUES (1, 'seeded')`)
			return err
		},
	})
	if err := m.MigrateUpAll(t.Context(), pool); err != nil {
		t.Fatalf("up: %v", err)
	}
	if calls != 1 {
		t.Errorf("data migration ran %d times, want 1", calls)
	}

	var note string
	if err := pool.QueryRow(t.Context(), `SELECT note FROM mig_probe WHERE id = 1`).Scan(&note); err != nil {
		t.Fatalf("read seeded row: %v", err)
	}
	if note != "seeded" {
		t.Errorf("got note %q, want seeded", note)
	}

	// A second up must not re-run it.
	if err := m.MigrateUpAll(t.Context(), pool); err != nil {
		t.Fatalf("second up: %v", err)
	}
	if calls != 1 {
		t.Errorf("data migration ran %d times across two ups, want 1", calls)
	}
}

func TestMigrateUpRollsBackAFailingDataMigration(t *testing.T) {
	pool := scratchPool(t)

	boom := fmt.Errorf("boom")
	m := newMigrator(goodFS, "testdata/good", migrator.DataMigrations{
		"0001_probe": func(context.Context, pgx.Tx) error { return boom },
	})
	err := m.MigrateUpAll(t.Context(), pool)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("got err %v, want it to wrap boom", err)
	}
	// Neither the table nor the ledger entry may survive.
	assertTable(t, pool, "mig_probe", false)
	assertApplied(t, pool, 0)
}

func TestMigrateDropAll(t *testing.T) {
	pool := scratchPool(t)
	m := newMigrator(goodFS, "testdata/good", migrator.DataMigrations{})

	if err := m.MigrateUpAll(t.Context(), pool); err != nil {
		t.Fatalf("up: %v", err)
	}
	if err := m.MigrateDropAll(t.Context(), pool); err != nil {
		t.Fatalf("drop: %v", err)
	}
	assertTable(t, pool, "mig_probe", false)
	assertTable(t, pool, "schema_migrations", false)

	// Drop must leave a database that can be rebuilt from scratch.
	if err := m.MigrateUpAll(t.Context(), pool); err != nil {
		t.Fatalf("up after drop: %v", err)
	}
	assertApplied(t, pool, 2)
}

func TestMigrateUpRejectsAMigrationWithoutAnUpFile(t *testing.T) {
	pool := scratchPool(t)
	m := newMigrator(missingUpFS, "testdata/missing_up", migrator.DataMigrations{})

	err := m.MigrateUpAll(t.Context(), pool)
	if err == nil || !strings.Contains(err.Error(), "no .up.sql") {
		t.Fatalf("got err %v, want a complaint about the missing .up.sql", err)
	}
}

func TestMigrateUpRejectsAMissingDir(t *testing.T) {
	pool := scratchPool(t)
	m := newMigrator(goodFS, "testdata/nonexistent", migrator.DataMigrations{})

	if err := m.MigrateUpAll(t.Context(), pool); err == nil {
		t.Fatal("up from a missing directory succeeded, want an error")
	}
}

func TestMigrateDownLastRejectsAMissingDownFile(t *testing.T) {
	pool := scratchPool(t)
	m := newMigrator(noDownFS, "testdata/no_down", nil)

	if err := m.MigrateUpAll(t.Context(), pool); err != nil {
		t.Fatalf("up: %v", err)
	}
	err := m.MigrateDownLast(t.Context(), pool)
	if err == nil || !strings.Contains(err.Error(), "no .down.sql") {
		t.Fatalf("got err %v, want a complaint about the missing .down.sql", err)
	}
}

// A ledger entry the migrations directory knows nothing about means the binary
// is older than the database — refusing beats silently skipping.
func TestMigrateDownLastRejectsAnUnknownVersion(t *testing.T) {
	pool := scratchPool(t)
	m := newMigrator(goodFS, "testdata/good", nil)

	if err := m.MigrateUpAll(t.Context(), pool); err != nil {
		t.Fatalf("up: %v", err)
	}
	if _, err := pool.Exec(t.Context(),
		`INSERT INTO schema_migrations (version, name) VALUES (99, '0099_from_the_future')`,
	); err != nil {
		t.Fatalf("insert phantom ledger row: %v", err)
	}

	err := m.MigrateDownLast(t.Context(), pool)
	if err == nil || !strings.Contains(err.Error(), "not found in migrations dir") {
		t.Fatalf("got err %v, want a complaint about the unknown version", err)
	}
}

func assertApplied(t *testing.T, pool *pgxpool.Pool, want int) {
	t.Helper()
	var got int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM schema_migrations`).Scan(&got); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if got != want {
		t.Errorf("got %d applied migrations, want %d", got, want)
	}
}

func assertTable(t *testing.T, pool *pgxpool.Pool, name string, want bool) {
	t.Helper()
	var got bool
	if err := pool.QueryRow(t.Context(),
		`SELECT EXISTS (SELECT 1 FROM pg_tables WHERE schemaname='public' AND tablename=$1)`, name,
	).Scan(&got); err != nil {
		t.Fatalf("look up table %s: %v", name, err)
	}
	if got != want {
		t.Errorf("table %s exists=%v, want %v", name, got, want)
	}
}

func assertColumn(t *testing.T, pool *pgxpool.Pool, name string, want bool) {
	t.Helper()
	var got bool
	if err := pool.QueryRow(t.Context(),
		`SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			 WHERE table_schema='public' AND table_name='mig_probe' AND column_name=$1)`, name,
	).Scan(&got); err != nil {
		t.Fatalf("look up column %s: %v", name, err)
	}
	if got != want {
		t.Errorf("column mig_probe.%s exists=%v, want %v", name, got, want)
	}
}
