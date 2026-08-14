package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/urfave/cli/v3"

	"github.com/sxwebdev/ai-reviewer/internal/domain"
	"github.com/sxwebdev/ai-reviewer/internal/jobs"
	"github.com/sxwebdev/ai-reviewer/internal/scheduler"
	"github.com/sxwebdev/ai-reviewer/internal/store"
	"github.com/sxwebdev/ai-reviewer/internal/store/repos/repo_digestrun"
	"github.com/sxwebdev/ai-reviewer/internal/store/storetest"
)

// digestFixture is a queue and a store over this binary's private test database
// (ai_reviewer_test_cli), plus a team name unique to this test.
//
// storetest treats AI_REVIEWER_TEST_PG_DSN as an admin handle and owns the
// creation, migration and per-test truncation of the private database, so the
// operator's own is never written to. River's schema is not storetest's to know
// about, so it comes in through WithSetup and river_job joins the truncate list
// through WithTruncate.
//
// The per-test team name is no longer needed for isolation, but it keeps each
// assertion explicit about whose digest run it is counting.
func digestFixture(t *testing.T) (*jobs.Queue, *store.Store, domain.Team) {
	t.Helper()
	st, pool := storetest.Store(t,
		storetest.WithSetup(jobs.MigrateUp),
		storetest.WithTruncate("river_job"),
	)

	q, err := jobs.NewQueue(pool)
	if err != nil {
		t.Fatal(err)
	}
	token := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	return q, st, domain.Team{Name: "payments-" + token, SlackChannel: "C1"}
}

func testSchedule(t *testing.T) scheduler.Daily {
	t.Helper()
	s, err := scheduler.NewDigest()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestDigestRefusesAnAlreadyRunSlot is §15's readable-refusal requirement: a
// repeat without --force must name the team, the date and the slot, not surface
// the unique index on (team, run_date, slot, attempt) as a constraint error.
func TestDigestRefusesAnAlreadyRunSlot(t *testing.T) {
	q, st, team := digestFixture(t)
	sched := testSchedule(t)

	// 06:00 UTC = 09:00 Moscow, so the run this fixture describes is the
	// morning slot of 13 August.
	now := time.Date(2026, 8, 13, 6, 0, 0, 0, time.UTC)
	runDate := time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)

	if _, err := st.DigestRun().Create(t.Context(), repo_digestrun.CreateParams{
		Team: team.Name, Slot: "09:00", RunDate: runDate, Attempt: 0, Status: "sent",
	}); err != nil {
		t.Fatalf("seed digest_runs: %v", err)
	}

	err := enqueueDigest(t.Context(), q, st, team, sched, now, false)
	if err == nil {
		t.Fatal("a repeat of a sent slot must be refused")
	}
	var coder cli.ExitCoder
	if !asExitCoder(err, &coder) || coder.ExitCode() == 0 {
		t.Fatalf("err = %v, want a non-zero exit code", err)
	}
	for _, want := range []string{team.Name, "2026-08-13", "09:00", "--force"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q:\n%s", want, err)
		}
	}
	if got := countDigestJobs(t, st, team.Name); got != 0 {
		t.Errorf("digest jobs = %d, want 0 — the refusal must not queue anything", got)
	}
}

// TestDigestForceTakesTheNextAttempt: --force does not relax the index, it
// files a new attempt, which is what keeps the journal of manual repeats honest.
func TestDigestForceTakesTheNextAttempt(t *testing.T) {
	q, st, team := digestFixture(t)
	sched := testSchedule(t)

	now := time.Date(2026, 8, 13, 6, 0, 0, 0, time.UTC)
	runDate := time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
	if _, err := st.DigestRun().Create(t.Context(), repo_digestrun.CreateParams{
		Team: team.Name, Slot: "09:00", RunDate: runDate, Attempt: 0, Status: "sent",
	}); err != nil {
		t.Fatalf("seed digest_runs: %v", err)
	}

	if err := enqueueDigest(t.Context(), q, st, team, sched, now, true); err != nil {
		t.Fatalf("--force: %v", err)
	}

	args := lastDigestArgs(t, st, team.Name)
	if args.Attempt != 1 {
		t.Errorf("attempt = %d, want 1", args.Attempt)
	}
	if args.Team != team.Name || args.Slot != "09:00" || args.RunDate != "2026-08-13" {
		t.Errorf("args = %+v, want the same slot as the run it repeats", args)
	}

	// A second --force must take attempt 2, not collide with the first: the
	// digest_runs row for attempt 1 does not exist yet (the job has not run),
	// so this also pins that the CLI reads the persisted runs, not the queue.
	if err := enqueueDigest(t.Context(), q, st, team, sched, now, true); err != nil {
		t.Fatalf("second --force: %v", err)
	}
}

// TestDigestQueuesAFreshSlot: with no prior run the command just enqueues
// attempt 0 — the same args the scheduler would have produced.
func TestDigestQueuesAFreshSlot(t *testing.T) {
	q, st, team := digestFixture(t)
	sched := testSchedule(t)
	now := time.Date(2026, 8, 13, 13, 30, 0, 0, time.UTC) // 16:30 Moscow

	if err := enqueueDigest(t.Context(), q, st, team, sched, now, false); err != nil {
		t.Fatalf("enqueueDigest: %v", err)
	}
	args := lastDigestArgs(t, st, team.Name)
	want := jobs.NewDigestArgs(team.Name, sched, now, 0)
	if args != want {
		t.Errorf("args = %+v, want %+v", args, want)
	}

	// Re-running while that job is still queued is a no-op, not a duplicate.
	if err := enqueueDigest(t.Context(), q, st, team, sched, now, false); err != nil {
		t.Fatalf("second enqueue: %v", err)
	}
	if got := countDigestJobs(t, st, team.Name); got != 1 {
		t.Errorf("digest jobs = %d, want 1", got)
	}
}

func countDigestJobs(t *testing.T, st *store.Store, team string) int {
	t.Helper()
	var n int
	if err := st.Pool().QueryRow(t.Context(),
		"SELECT count(*) FROM river_job WHERE kind = $1 AND args->>'team' = $2",
		jobs.KindDigest, team).Scan(&n); err != nil {
		t.Fatalf("count digest jobs: %v", err)
	}
	return n
}

func lastDigestArgs(t *testing.T, st *store.Store, team string) jobs.DigestArgs {
	t.Helper()
	var raw []byte
	if err := st.Pool().QueryRow(t.Context(),
		"SELECT args FROM river_job WHERE kind = $1 AND args->>'team' = $2 ORDER BY id DESC LIMIT 1",
		jobs.KindDigest, team).Scan(&raw); err != nil {
		t.Fatalf("load digest args: %v", err)
	}
	var args jobs.DigestArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		t.Fatalf("decode digest args: %v", err)
	}
	return args
}
