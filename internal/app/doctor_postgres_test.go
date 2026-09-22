package app

import (
	"context"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sxwebdev/ai-reviewer/internal/config"
	"github.com/sxwebdev/ai-reviewer/internal/jobs"
	"github.com/sxwebdev/ai-reviewer/internal/store/storetest"
)

// dbApp builds an App pointed at this binary's private test database
// (ai_reviewer_test_app), plus a pool for direct queries. It skips when
// AI_REVIEWER_TEST_PG_DSN is unset.
//
// The DSN the operator supplies is an admin handle: storetest derives a private
// database per test binary and owns its creation, migration and truncation, so
// the operator's own database never gains app or river_* tables from a test run.
// The doctor probes build their own connection from config, so the private DSN
// is parsed back into a PostgresConfig rather than reused as a pool.
func dbApp(t *testing.T) (*App, *pgxpool.Pool) {
	t.Helper()
	pool := storetest.Pool(t,
		storetest.WithSetup(jobs.MigrateUp),
		storetest.WithTruncate("river_job"),
	)

	u, err := url.Parse(storetest.DSN(t))
	if err != nil {
		t.Fatalf("parse the private test DSN: %v", err)
	}
	pass, _ := u.User.Password()

	cfg := testConfig(t)
	cfg.Postgres = config.PostgresConfig{
		Host:     u.Hostname(),
		Port:     u.Port(),
		Database: strings.TrimPrefix(u.Path, "/"),
		Username: config.Secret(u.User.Username()),
		Password: config.Secret(pass),
		SSLMode:  u.Query().Get("sslmode"),
	}

	return &App{Config: cfg, Log: quietLogger()}, pool
}

// TestCheckPostgresOnAMigratedDatabase covers §15's Postgres probes: the
// connection, the application's migration state and River's own tables. The
// third is worth its own line — a pod that connects but finds no river_job
// table starts, elects a leader, and then fails every insert.
func TestCheckPostgresOnAMigratedDatabase(t *testing.T) {
	// The fixture arrives fully migrated: storetest applies the app schema and
	// the WithSetup hook applies River's.
	a, _ := dbApp(t)

	col := &checkCollector{}
	a.checkPostgres(t.Context(), col)

	byName := map[string]DoctorCheck{}
	for _, c := range col.checks {
		byName[c.Name] = c
	}
	for _, name := range []string{"postgres", "migrations", "river schema"} {
		c, ok := byName[name]
		if !ok {
			t.Fatalf("no %q check: %+v", name, col.checks)
		}
		if c.Status != StatusOK {
			t.Errorf("%s = %v (%s)", name, c.Status, c.Detail)
		}
	}
}

func TestPendingMigrationsOnAMigratedDatabase(t *testing.T) {
	_, pool := dbApp(t)

	got, err := pendingMigrations(t.Context(), pool)
	if err != nil {
		t.Fatalf("pendingMigrations: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("pending = %v, want none on a fully migrated database", got)
	}
}

func TestRiverTablesPresent(t *testing.T) {
	_, pool := dbApp(t)
	if err := riverTablesPresent(t.Context(), pool); err != nil {
		t.Errorf("riverTablesPresent after migrating: %v", err)
	}
}

// TestCheckPostgresCannotConnect: an unreachable database must be one clear
// failure, not a panic or a hang.
func TestCheckPostgresCannotConnect(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	cfg.Postgres.Host = "127.0.0.1"
	cfg.Postgres.Port = "1" // nothing listens here
	cfg.Postgres.Username = config.Secret("nobody")
	cfg.Postgres.SSLMode = "disable"

	a := &App{Config: cfg, Log: quietLogger()}
	col := &checkCollector{}
	a.checkPostgres(t.Context(), col)

	if len(col.checks) != 1 || col.checks[0].Status != StatusFail {
		t.Fatalf("checks = %+v, want a single failure", col.checks)
	}
}

// TestMigrateHoldsOneLockAcrossBothMigrators is F2.3: rivermigrate takes no
// lock of its own, so if the application migrator's lock were the only one, two
// replicas starting together would both read an empty river_migration and both
// run `CREATE TABLE river_job` — the loser aborting startup. The lock has to
// span the whole sequence, application schema and River's alike.
//
// The test holds the lock from a second session and asserts Migrate waits for
// it rather than proceeding. Without the lock the call returns immediately
// (both migrators are no-ops on an already-migrated fixture), which is exactly
// the regression.
func TestMigrateHoldsOneLockAcrossBothMigrators(t *testing.T) {
	a, pool := dbApp(t)

	holder, err := pool.Acquire(t.Context())
	if err != nil {
		t.Fatalf("acquire the holding connection: %v", err)
	}
	// Unlock deferred, not just written at the happy-path point below: a
	// session lock survives the connection going back to the pool, so an
	// assertion failing between here and the release would leave it held — and
	// the next run of anything that migrates would block rather than fail,
	// which presents as a package that hangs with nothing red in it.
	unlock := sync.OnceFunc(func() {
		if _, err := holder.Exec(context.WithoutCancel(t.Context()),
			`SELECT pg_advisory_unlock($1, $2)`, migrateLockClass, migrateLockObj); err != nil {
			t.Errorf("release the migration lock: %v", err)
		}
	})
	defer func() { unlock(); holder.Release() }()

	if _, err := holder.Exec(t.Context(),
		`SELECT pg_advisory_lock($1, $2)`, migrateLockClass, migrateLockObj); err != nil {
		t.Fatalf("hold the migration lock: %v", err)
	}

	blocked, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := a.Migrate(blocked, pool); err == nil {
		t.Fatal("Migrate ran while another replica held the migration lock")
	}

	// Releasing must let it through, or the lock would be a queue nobody leaves.
	// Bounded, because the failure mode of a leaked lock is a wait, not an
	// error: an unbounded call here would hang the package instead of failing it.
	unlock()
	through, cancelThrough := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancelThrough()
	if err := a.Migrate(through, pool); err != nil {
		t.Fatalf("Migrate after the lock was released: %v", err)
	}

	// And it left nothing behind. A Migrate that returns while still holding
	// its lock is invisible until the next process blocks on it.
	assertNoAdvisoryLocks(t, pool)
}

// assertNoAdvisoryLocks fails if this database has any advisory lock left.
//
// Scoped to the current database, which storetest makes private to this test
// binary, so it cannot see another package's locks. Tests in a package run
// sequentially, so "none held" is a fact about the code under test rather than
// a race.
func assertNoAdvisoryLocks(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	var n int
	err := pool.QueryRow(context.WithoutCancel(t.Context()), `
		SELECT count(*) FROM pg_locks
		 WHERE locktype = 'advisory'
		   AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`).Scan(&n)
	if err != nil {
		t.Fatalf("read pg_locks: %v", err)
	}
	if n != 0 {
		t.Errorf("%d advisory lock(s) still held; a leaked session lock blocks every later acquirer until the connection is recycled", n)
	}
}
