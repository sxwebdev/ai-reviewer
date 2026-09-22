// Package storetest is a real-Postgres harness for store integration tests.
// Tests skip unless AI_REVIEWER_TEST_PG_DSN points at a throwaway database
// (`make infra-up` starts one; `make test-db` runs the suite with it), so
// `make test` without a database stays green — plan section 17.
//
// # Isolation
//
// `go test ./...` runs each package as its own process, concurrently. A harness
// that truncated tables in one shared database would therefore delete rows a
// different package is mid-assertion on — which is exactly what happened: stray
// mr_reviews_success_uniq violations and reviews vanishing between two steps of
// one test, roughly one run in three.
//
// So the DSN the operator supplies is treated as the *admin* handle, and each
// test binary gets a private database of its own derived from its name
// (ai_reviewer_test_store, _service, _jobs, …). Migrations are applied there and
// the truncate happens there, where it is nobody else's business. The database
// is created on first use and reused afterwards: repeated runs of one package
// are fast, and no database this harness did not create is ever dropped.
package storetest

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tkcrm/mx/logger"

	"github.com/sxwebdev/ai-reviewer/internal/migrator"
	"github.com/sxwebdev/ai-reviewer/internal/store"
	"github.com/sxwebdev/ai-reviewer/sql"
)

// DSNEnv is the environment variable that both enables and configures the
// Postgres-backed tests. It points at an admin handle; see the package comment.
const DSNEnv = "AI_REVIEWER_TEST_PG_DSN"

// dbPrefix namespaces the per-binary databases so they are recognisable on a
// shared server and cannot be mistaken for a real one.
const dbPrefix = "ai_reviewer_test_"

// appTables are truncated between tests. schema_migrations and river_* are left
// alone — recreating them per test would cost more than every query under test
// combined. CASCADE reaches mr_findings and digest_messages through their FKs.
var appTables = []string{"mr_reviews", "digest_runs"}

type options struct {
	setup    []func(ctx context.Context, pool *pgxpool.Pool) error
	truncate []string
}

// Option customises the harness for a package whose needs go beyond the app
// schema.
type Option func(*options)

// WithSetup runs fn against the private database on every Pool call, after the
// application migrations and before the truncate. fn must be idempotent.
//
// This is the hook for schemas this package does not own: internal/jobs passes
// its River migrator as storetest.Pool(t, storetest.WithSetup(jobs.MigrateUp)),
// which keeps River out of storetest's imports.
func WithSetup(fn func(ctx context.Context, pool *pgxpool.Pool) error) Option {
	return func(o *options) { o.setup = append(o.setup, fn) }
}

// WithTruncate adds tables to the per-test truncate list — for example
// WithTruncate("river_job") once a WithSetup hook has created them. Names that
// do not exist yet are skipped rather than failing, so ordering never matters.
func WithTruncate(tables ...string) Option {
	return func(o *options) { o.truncate = append(o.truncate, tables...) }
}

var (
	prepareOnce sync.Once
	privateDSN  string
	prepareErr  error
)

// DSN returns the connection string of this test binary's private database,
// creating it and applying the application migrations on first use. It skips the
// test when DSNEnv is unset.
//
// Use it when you need to build the pool yourself, or to hand a DSN to a CLI
// under test; most callers want Pool or Store instead.
func DSN(t *testing.T) string {
	t.Helper()
	admin := os.Getenv(DSNEnv)
	if admin == "" {
		t.Skipf("set %s (a throwaway database) to run DB integration tests", DSNEnv)
	}
	prepareOnce.Do(func() {
		// Detached from t.Context: the result is memoised for the whole process,
		// so it must not be cancelled when the first test that triggered it ends.
		privateDSN, prepareErr = prepare(context.Background(), admin, databaseName(os.Args[0]))
	})
	if prepareErr != nil {
		t.Fatalf("prepare private test database: %v", prepareErr)
	}
	return privateDSN
}

// Pool connects to this binary's private database, applies migrations, runs any
// WithSetup hooks and truncates the app tables. It skips when DSNEnv is unset.
func Pool(t *testing.T, opts ...Option) *pgxpool.Pool {
	t.Helper()
	dsn := DSN(t)

	var o options
	for _, opt := range opts {
		opt(&o)
	}

	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connect private test database: %v", err)
	}
	t.Cleanup(pool.Close)

	for _, fn := range o.setup {
		if err := fn(t.Context(), pool); err != nil {
			t.Fatalf("setup hook: %v", err)
		}
	}
	if err := truncate(t.Context(), pool, append(appTables, o.truncate...)); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return pool
}

// Store returns a *store.Store plus the underlying pool.
func Store(t *testing.T, opts ...Option) (*store.Store, *pgxpool.Pool) {
	t.Helper()
	pool := Pool(t, opts...)
	st, err := store.New(pool)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	return st, pool
}

var nonIdent = regexp.MustCompile(`[^a-z0-9]+`)

// databaseName derives a stable per-package name from the test binary, which
// `go test` names <package>.test. Two packages sharing a final path element
// would collide; the ones that use this harness (store, service, jobs, cli) do
// not.
//
// arg0 is a parameter rather than a read of os.Args[0] because this package's
// own tests exercise the derivation by assigning that global — in the same
// binary whose private database is derived from it and then TRUNCATEd. Today
// only file ordering keeps `storetest.test` from creating, migrating and
// truncating ai_reviewer_test_service; under -shuffle=on, or as soon as a
// DB-backed test is added earlier in the internal test file, it would.
func databaseName(arg0 string) string {
	base := filepath.Base(arg0)
	base = strings.TrimSuffix(base, ".exe")
	base = strings.TrimSuffix(base, ".test")
	base = strings.Trim(nonIdent.ReplaceAllString(strings.ToLower(base), "_"), "_")
	if base == "" {
		base = "unknown"
	}
	if len(base) > 40 {
		base = base[:40]
	}
	return dbPrefix + base
}

// prepare creates the private database if it is missing and migrates it.
func prepare(ctx context.Context, adminDSN, name string) (string, error) {
	dsn, err := swapDatabase(adminDSN, name)
	if err != nil {
		return "", err
	}

	admin, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		return "", fmt.Errorf("connect %s: %w", DSNEnv, err)
	}
	defer admin.Close()

	var exists bool
	if err := admin.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, name,
	).Scan(&exists); err != nil {
		return "", fmt.Errorf("look up database %s: %w", name, err)
	}
	if !exists {
		_, err := admin.Exec(ctx, `CREATE DATABASE `+pgx.Identifier{name}.Sanitize())
		var pgErr *pgconn.PgError
		switch {
		case err == nil:
		// 42P04 duplicate_database: another test binary created it between the
		// lookup and here. That is the expected outcome of a parallel start, not
		// a failure.
		case errors.As(err, &pgErr) && pgErr.Code == "42P04":
		case errors.As(err, &pgErr) && pgErr.Code == "42501":
			return "", fmt.Errorf("creating %s needs the CREATEDB privilege: %w", name, err)
		default:
			return "", fmt.Errorf("create database %s: %w", name, err)
		}
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return "", fmt.Errorf("connect %s: %w", name, err)
	}
	defer pool.Close()

	// MigrateUpAll takes an advisory lock, so two binaries racing on a database
	// they both just found missing still apply the migrations exactly once.
	m := migrator.New(logger.New(), sql.MigrationsFS, sql.MigrationsPath, migrator.DataMigrations{})
	if err := m.MigrateUpAll(ctx, pool); err != nil {
		return "", fmt.Errorf("migrate %s: %w", name, err)
	}
	return dsn, nil
}

// swapDatabase points dsn at another database, in either of the two forms libpq
// accepts. In the keyword/value form a later setting wins, so appending is
// enough.
func swapDatabase(dsn, name string) (string, error) {
	out := strings.TrimSpace(dsn) + " dbname=" + name
	if u, err := url.Parse(dsn); err == nil && (u.Scheme == "postgres" || u.Scheme == "postgresql") {
		u.Path = "/" + name
		out = u.String()
	}
	if _, err := pgxpool.ParseConfig(out); err != nil {
		return "", fmt.Errorf("derive a DSN for database %s: %w", name, err)
	}
	return out, nil
}

// truncate empties the tables that exist and ignores the rest — a caller may ask
// for river_job before any WithSetup hook has created it.
func truncate(ctx context.Context, pool *pgxpool.Pool, tables []string) error {
	rows, err := pool.Query(ctx,
		`SELECT tablename FROM pg_tables WHERE schemaname = 'public' AND tablename = ANY($1)`,
		tables,
	)
	if err != nil {
		return err
	}
	present := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		present = append(present, pgx.Identifier{name}.Sanitize())
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(present) == 0 {
		return nil
	}
	_, err = pool.Exec(ctx, "TRUNCATE "+strings.Join(present, ", ")+" RESTART IDENTITY CASCADE")
	return err
}
