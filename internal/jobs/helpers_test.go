package jobs_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tkcrm/mx/logger"

	"github.com/sxwebdev/ai-reviewer/internal/domain"
	"github.com/sxwebdev/ai-reviewer/internal/jobs"
	"github.com/sxwebdev/ai-reviewer/internal/store"
	"github.com/sxwebdev/ai-reviewer/internal/store/storetest"
)

// fixture is one test's slice of this binary's private test database.
//
// storetest treats AI_REVIEWER_TEST_PG_DSN as an admin handle and gives each
// test binary a database of its own (ai_reviewer_test_jobs here), creating and
// migrating it; the operator's database is never written to. River's schema is
// not storetest's to know about, so it arrives through WithSetup — whose
// signature jobs.MigrateUp already matches — and river_job joins the per-test
// truncate list through WithTruncate.
//
// On top of that, every fixture carries a token that makes its team names,
// repositories and head SHAs unique, and assertions count only rows carrying
// it. With a private database that is no longer load-bearing, but it keeps each
// test's intent explicit: these counts are about *this* fixture's jobs.
type fixture struct {
	pool  *pgxpool.Pool
	token string
	teams []domain.Team
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := storetest.Pool(t,
		storetest.WithSetup(jobs.MigrateUp),
		storetest.WithTruncate("river_job"),
	)

	token := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	return &fixture{
		pool:  pool,
		token: token,
		teams: []domain.Team{
			{
				Name: "payments-" + token, SlackChannel: "C1", AIReview: true,
				Repositories:   []string{token + "/payments", token + "/billing"},
				DigestSlots:    []string{"09:00", "14:00", "17:30"},
				DigestTimezone: "Europe/Moscow",
			},
			{
				// A second zone on purpose: every fixture that exercises two teams
				// then exercises two schedules, which is the shape the per-team
				// change introduced.
				Name: "platform-" + token, SlackChannel: "C2", AIReview: true,
				Repositories:   []string{token + "/auth"},
				DigestSlots:    []string{"10:00", "18:00"},
				DigestTimezone: "Europe/Lisbon",
			},
		},
	}
}

// team returns one of the fixture's teams (0 = payments, 1 = platform).
func (f *fixture) team(i int) domain.Team { return f.teams[i] }

// repo returns one of the payments team's repositories.
func (f *fixture) repo(i int) string { return f.teams[0].Repositories[i] }

// sha returns a head SHA unique to this fixture and suffix.
func (f *fixture) sha(suffix string) string { return f.token + "deadbeef" + suffix }

// reviewArgs is the fixture's canonical review request.
func (f *fixture) reviewArgs() jobs.ReviewArgs {
	return jobs.ReviewArgs{
		Team:        f.team(0).Name,
		ProjectPath: f.repo(0),
		ProjectID:   42,
		MRIID:       7,
		HeadSHA:     f.sha("a"),
	}
}

// digestArgs is the fixture's canonical digest request.
func (f *fixture) digestArgs() jobs.DigestArgs {
	return jobs.DigestArgs{Team: f.team(0).Name, Slot: "09:00", RunDate: "2026-08-13", Attempt: 0}
}

// assertNoAdvisoryLocks fails if this database has any advisory lock left.
//
// Scoped to the current database, which storetest makes private to this test
// binary, so another package's locks are invisible here. A lock nobody releases
// is not a failing assertion anywhere — it is a wait, so the next acquirer
// hangs with nothing red in the run.
func (f *fixture) assertNoAdvisoryLocks(t *testing.T) {
	t.Helper()
	var n int
	err := f.pool.QueryRow(context.WithoutCancel(t.Context()), `
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

// count returns how many jobs of a kind carry the given args value. Scoping a
// count rather than taking a table total says which jobs the assertion is about,
// and keeps it honest if a test ever queues more than one kind's worth of work.
func (f *fixture) count(t *testing.T, kind, argsKey, argsValue string) int {
	t.Helper()
	var n int
	err := f.pool.QueryRow(t.Context(),
		"SELECT count(*) FROM river_job WHERE kind = $1 AND args->>$2 = $3",
		kind, argsKey, argsValue).Scan(&n)
	if err != nil {
		t.Fatalf("count %s jobs: %v", kind, err)
	}
	return n
}

// countByPrefix counts jobs whose args value starts with a prefix — used where
// the discriminating value differs per row (one scan_repo job per repository).
func (f *fixture) countByPrefix(t *testing.T, kind, argsKey, prefix string) int {
	t.Helper()
	var n int
	err := f.pool.QueryRow(t.Context(),
		"SELECT count(*) FROM river_job WHERE kind = $1 AND args->>$2 LIKE $3",
		kind, argsKey, prefix+"%").Scan(&n)
	if err != nil {
		t.Fatalf("count %s jobs: %v", kind, err)
	}
	return n
}

// rawArgs returns the encoded args of the newest job of a kind carrying a value.
func (f *fixture) rawArgs(t *testing.T, kind, argsKey, argsValue string) []byte {
	t.Helper()
	var raw []byte
	err := f.pool.QueryRow(t.Context(),
		"SELECT args FROM river_job WHERE kind = $1 AND args->>$2 = $3 ORDER BY id DESC LIMIT 1",
		kind, argsKey, argsValue).Scan(&raw)
	if err != nil {
		t.Fatalf("load %s args: %v", kind, err)
	}
	return raw
}

// finalize forces a job into a terminal state without running it, which is how
// the "a completed job does not block a re-insert" test gets a completed job
// without a worker.
func finalize(t *testing.T, pool *pgxpool.Pool, id int64, state string) {
	t.Helper()
	_, err := pool.Exec(t.Context(),
		`UPDATE river_job SET state = $2::river_job_state, finalized_at = now() WHERE id = $1`, id, state)
	if err != nil {
		t.Fatalf("finalize job %d as %s: %v", id, state, err)
	}
}

func testLogger() logger.Logger {
	// Fatal keeps the suite's output readable; nothing under test asserts on logs.
	return logger.New(logger.WithConfig(logger.Config{
		Level:  logger.LogLevelFatal,
		Format: logger.LoggerFormatJSON,
	}))
}

// newService builds a Service over the fixture's pool with fake collaborators.
func (f *fixture) newService(t *testing.T, cfg jobs.Config, deps jobs.Deps) *jobs.Service {
	t.Helper()
	if cfg.Teams == nil {
		cfg.Teams = f.teams
	}
	// A schedule is required per team — NewService refuses a team it cannot
	// schedule, and every real caller gets one from app.Teams. Filled in here so
	// the tests that hand-build a team need not say so.
	for i := range cfg.Teams {
		if len(cfg.Teams[i].DigestSlots) == 0 {
			cfg.Teams[i].DigestSlots = []string{"09:00", "14:00", "17:30"}
		}
		if cfg.Teams[i].DigestTimezone == "" {
			cfg.Teams[i].DigestTimezone = "Europe/Moscow"
		}
	}
	svc, err := jobs.NewService(testLogger(), cfg, f.pool, deps)
	if err != nil {
		t.Fatalf("jobs.NewService: %v", err)
	}
	return svc
}

func (f *fixture) newQueue(t *testing.T) *jobs.Queue {
	t.Helper()
	q, err := jobs.NewQueue(f.pool)
	if err != nil {
		t.Fatalf("jobs.NewQueue: %v", err)
	}
	return q
}

func (f *fixture) store(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.New(f.pool)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	return st
}

// --- fakes -----------------------------------------------------------------

// fakeReviewer records the requests it was handed. When a store is set it
// persists inside a real transaction, calling onPersist from within — which is
// what the §10.4 atomicity test observes.
type fakeReviewer struct {
	mu sync.Mutex

	store *store.Store
	// rollback forces the persistence transaction to fail after onPersist ran.
	rollback bool
	runErr   error
	outcome  *jobs.ReviewOutcome
	// nilOutcome makes RunReview return (nil, nil) — the service saying
	// "nothing to do" without an error.
	nilOutcome bool
	// started is closed-over signalling for the advisory-lock tests: RunReview
	// announces itself on started and then waits for block to be closed, which
	// is how a test can observe a worker *while it holds the lock*.
	started chan<- struct{}
	block   <-chan struct{}

	requests    []jobs.ReviewRequest
	hadPersist  []bool
	publishedID []uuid.UUID
	publishErr  error
	persistID   uuid.UUID
}

var errForcedRollback = errors.New("forced rollback")

func (f *fakeReviewer) RunReview(ctx context.Context, req jobs.ReviewRequest, onPersist jobs.OnPersist) (*jobs.ReviewOutcome, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.hadPersist = append(f.hadPersist, onPersist != nil)
	rollback, runErr, st, out, noOutcome := f.rollback, f.runErr, f.store, f.outcome, f.nilOutcome
	started, block, first := f.started, f.block, len(f.requests) == 1
	f.mu.Unlock()

	// Only the first call blocks: the same fake then serves the follow-up runs
	// the test makes once the lock is free.
	if first && started != nil {
		close(started)
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	if runErr != nil {
		return nil, runErr
	}
	if onPersist != nil && st != nil {
		err := st.RunInTx(ctx, func(tx pgx.Tx) error {
			if err := onPersist(ctx, tx, f.reviewID()); err != nil {
				return err
			}
			if rollback {
				return errForcedRollback
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	if noOutcome {
		return nil, nil
	}
	if out == nil {
		out = &jobs.ReviewOutcome{Status: "reviewed"}
	}
	return out, nil
}

// reviewID is the id the fake persists under. It is generated once and reused so
// the transactional-enqueue test can find the job it expects.
func (f *fakeReviewer) reviewID() uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.persistID == uuid.Nil {
		f.persistID = uuid.New()
	}
	return f.persistID
}

func (f *fakeReviewer) PublishReview(_ context.Context, reviewID uuid.UUID) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.publishedID = append(f.publishedID, reviewID)
	return len(f.publishedID), f.publishErr
}

func (f *fakeReviewer) snapshot() ([]jobs.ReviewRequest, []bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]jobs.ReviewRequest(nil), f.requests...), append([]bool(nil), f.hadPersist...)
}

// fakeScanner returns a canned result per repository.
type fakeScanner struct {
	mu       sync.Mutex
	results  map[string]*jobs.ScanResult
	err      error
	inspects []string
}

func (f *fakeScanner) ScanRepository(_ context.Context, team domain.Team, repository string) (*jobs.ScanResult, error) {
	f.mu.Lock()
	f.inspects = append(f.inspects, team.Name+"/"+repository)
	res, ok := f.results[repository]
	err := f.err
	f.mu.Unlock()

	if err != nil {
		return nil, err
	}
	if !ok {
		return &jobs.ScanResult{}, nil
	}
	return res, nil
}

// fakeDigester records what was built and delivered.
type fakeDigester struct {
	mu       sync.Mutex
	outcome  *jobs.DigestOutcome
	buildErr error
	sendErr  error

	builds []string // "team|slot|runDate|attempt"
	sent   []uuid.UUID
}

func (f *fakeDigester) BuildDigest(_ context.Context, team domain.Team, slot string, runDate time.Time, attempt int) (*jobs.DigestOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.builds = append(f.builds, team.Name+"|"+slot+"|"+runDate.Format(time.RFC3339)+"|"+strconv.Itoa(attempt))
	if f.buildErr != nil {
		return nil, f.buildErr
	}
	return f.outcome, nil
}

func (f *fakeDigester) SendMessage(_ context.Context, messageID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, messageID)
	return f.sendErr
}

func newDeps(rev *fakeReviewer, sc *fakeScanner, dg *fakeDigester) jobs.Deps {
	if rev == nil {
		rev = &fakeReviewer{}
	}
	if sc == nil {
		sc = &fakeScanner{}
	}
	if dg == nil {
		dg = &fakeDigester{}
	}
	return jobs.Deps{Reviewer: rev, Scanner: sc, Digester: dg}
}
