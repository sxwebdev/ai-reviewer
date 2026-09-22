package storetest_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"

	"github.com/sxwebdev/ai-reviewer/internal/store/storetest"
)

// riverMigrateUp has the same signature and does the same work as
// internal/jobs.MigrateUp, which is the real caller of WithSetup. Exercising the
// hook here proves the contract without storetest's tests depending on the jobs
// package.
func riverMigrateUp(ctx context.Context, pool *pgxpool.Pool) error {
	m, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		return err
	}
	_, err = m.Migrate(ctx, rivermigrate.DirectionUp, nil)
	return err
}

// A package that owns tables outside the app schema must be able to create them
// and have them cleaned between tests — River's, in practice.
func TestPoolWithSetupAndTruncate(t *testing.T) {
	// WithTruncate names river_job before the hook has necessarily created it,
	// which is the ordering the option is required to tolerate.
	pool := storetest.Pool(t,
		storetest.WithSetup(riverMigrateUp),
		storetest.WithTruncate("river_job"),
	)

	var exists bool
	if err := pool.QueryRow(t.Context(),
		`SELECT EXISTS (SELECT 1 FROM pg_tables WHERE schemaname='public' AND tablename='river_job')`,
	).Scan(&exists); err != nil {
		t.Fatalf("look up river_job: %v", err)
	}
	if !exists {
		t.Fatal("the setup hook did not create river_job")
	}

	if _, err := pool.Exec(t.Context(),
		`INSERT INTO river_job (args, kind, max_attempts, metadata, priority, queue, state)
		 VALUES ('{}', 'probe', 1, '{}', 1, 'default', 'available')`,
	); err != nil {
		t.Fatalf("insert a job: %v", err)
	}

	// A second Pool call is the next test's setup; the row must not survive it.
	next := storetest.Pool(t,
		storetest.WithSetup(riverMigrateUp),
		storetest.WithTruncate("river_job"),
	)
	var n int64
	if err := next.QueryRow(t.Context(), `SELECT count(*) FROM river_job`).Scan(&n); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if n != 0 {
		t.Errorf("got %d jobs after the truncate, want 0", n)
	}
}

// Every helper resolves to the private database, never to the admin handle the
// operator supplied — that identity is the isolation guarantee.
func TestHelpersShareOnePrivateDatabase(t *testing.T) {
	dsn := storetest.DSN(t)
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	// The binary here is storetest.test, so the database is …_test_storetest.
	if cfg.ConnConfig.Database != "ai_reviewer_test_storetest" {
		t.Errorf("got database %q, want ai_reviewer_test_storetest", cfg.ConnConfig.Database)
	}

	pool := storetest.Pool(t)
	var current string
	if err := pool.QueryRow(t.Context(), `SELECT current_database()`).Scan(&current); err != nil {
		t.Fatalf("current_database: %v", err)
	}
	if current != cfg.ConnConfig.Database {
		t.Errorf("Pool connected to %q, want %q", current, cfg.ConnConfig.Database)
	}

	st, _ := storetest.Store(t)
	if err := st.Pool().QueryRow(t.Context(), `SELECT current_database()`).Scan(&current); err != nil {
		t.Fatalf("current_database via Store: %v", err)
	}
	if current != cfg.ConnConfig.Database {
		t.Errorf("Store connected to %q, want %q", current, cfg.ConnConfig.Database)
	}
}
