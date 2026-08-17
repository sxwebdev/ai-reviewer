package jobs

import (
	"context"
	"errors"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/tkcrm/mx/logger"

	"github.com/sxwebdev/ai-reviewer/internal/domain"
	"github.com/sxwebdev/ai-reviewer/internal/metrics"
	"github.com/sxwebdev/ai-reviewer/internal/scheduler"
)

// captureReviewer records what runReview handed it. Only the two facts the
// decision produces are interesting: the effective Publish flag and whether a
// publication hand-off was supplied at all.
type captureReviewer struct {
	req        ReviewRequest
	hadPersist bool
	calls      int
}

func (c *captureReviewer) RunReview(_ context.Context, req ReviewRequest, onPersist OnPersist) (*ReviewOutcome, error) {
	c.calls++
	c.req = req
	c.hadPersist = onPersist != nil
	return &ReviewOutcome{Status: "reviewed"}, nil
}

func (c *captureReviewer) PublishReview(context.Context, uuid.UUID) (int, error) { return 0, nil }

func quietLogger() logger.Logger {
	return logger.New(logger.WithConfig(logger.Config{Level: logger.LogLevelFatal, Format: logger.LoggerFormatJSON}))
}

// TestRunReviewPublishDecision pins §15's rule: --publish travels in the job
// args and wins over service.ai_review_publish_enabled, and a false decision
// means no publication hand-off exists at all (the dry-run contract — with a
// nil OnPersist the service cannot enqueue publication even by mistake).
func TestRunReviewPublishDecision(t *testing.T) {
	t.Parallel()
	yes, no := true, false

	cases := []struct {
		name        string
		argsPublish *bool
		configured  bool
		want        bool
	}{
		{"unset args follow the configured default (off)", nil, false, false},
		{"unset args follow the configured default (on)", nil, true, true},
		{"--publish overrides a disabled default", &yes, false, true},
		{"an explicit false overrides an enabled default", &no, true, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc := &Service{log: quietLogger(), cfg: Config{PublishEnabled: tc.configured}}
			rev := &captureReviewer{}

			args := ReviewArgs{
				Team: "payments", ProjectPath: "backend/payments",
				ProjectID: 42, MRIID: 7, HeadSHA: "deadbeefcafe",
				Publish: tc.argsPublish,
			}
			if _, err := svc.runReview(t.Context(), args, rev); err != nil {
				t.Fatalf("runReview: %v", err)
			}

			if rev.calls != 1 {
				t.Fatalf("RunReview called %d times, want exactly 1", rev.calls)
			}
			if rev.req.Publish != tc.want {
				t.Errorf("effective Publish = %v, want %v", rev.req.Publish, tc.want)
			}
			if rev.hadPersist != tc.want {
				t.Errorf("OnPersist supplied = %v, want %v (a nil hand-off is the dry-run contract)",
					rev.hadPersist, tc.want)
			}
			// The rest of the request must survive verbatim; a dropped field
			// would send the review at the wrong merge request.
			if rev.req.Team != args.Team || rev.req.ProjectPath != args.ProjectPath ||
				rev.req.ProjectID != args.ProjectID || rev.req.MRIID != args.MRIID ||
				rev.req.HeadSHA != args.HeadSHA {
				t.Errorf("request lost fields: %+v", rev.req)
			}
		})
	}
}

// TestRunReviewPropagatesFailure keeps a failed review a failed job: River must
// see the error to record the attempt, even though it will not retry it.
func TestRunReviewPropagatesFailure(t *testing.T) {
	t.Parallel()
	want := errors.New("clone failed")
	svc := &Service{log: quietLogger(), cfg: Config{}}
	_, err := svc.runReview(t.Context(), ReviewArgs{}, failingReviewer{err: want})
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

type failingReviewer struct{ err error }

func (f failingReviewer) RunReview(context.Context, ReviewRequest, OnPersist) (*ReviewOutcome, error) {
	return nil, f.err
}
func (f failingReviewer) PublishReview(context.Context, uuid.UUID) (int, error) { return 0, nil }

// TestWorkerTimeouts pins §6.2's timeout column. River's default is one minute,
// which every kind here except the dispatchers would blow through.
func TestWorkerTimeouts(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"scan", (&ScanWorker{}).Timeout(nil), 2 * time.Minute},
		{"scan_repo", (&ScanRepoWorker{}).Timeout(nil), 10 * time.Minute},
		{"review", (&ReviewWorker{}).Timeout(nil), 30 * time.Minute},
		{"publish_review", (&PublishReviewWorker{}).Timeout(nil), 5 * time.Minute},
		{"digest", (&DigestWorker{}).Timeout(nil), 10 * time.Minute},
		{"slack_send", (&SlackSendWorker{}).Timeout(nil), 2 * time.Minute},
		{"cleanup", (&CleanupWorker{}).Timeout(nil), 5 * time.Minute},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s timeout = %s, want %s", tc.name, tc.got, tc.want)
		}
	}
}

func TestConfigNormalizedFillsZeroes(t *testing.T) {
	t.Parallel()
	got := Config{}.normalized()
	if got.ScanInterval <= 0 || got.CleanupInterval <= 0 || got.DrainTimeout <= 0 {
		t.Errorf("zero durations survived normalization: %+v", got)
	}
	if got.ReviewWorkers < 1 || got.DefaultWorkers < 1 || got.PublishWorkers < 1 || got.SlackWorkers < 1 {
		t.Errorf("a queue was left with no workers: %+v", got)
	}

	// Explicit values must survive: normalization fills gaps, it does not
	// impose opinions.
	set := Config{ScanInterval: time.Minute, CleanupInterval: 2 * time.Hour, DrainTimeout: 90 * time.Second,
		ReviewWorkers: 8, DefaultWorkers: 3, PublishWorkers: 2, SlackWorkers: 4}.normalized()
	if set.ReviewWorkers != 8 || set.ScanInterval != time.Minute || set.SlackWorkers != 4 {
		t.Errorf("normalization overwrote explicit values: %+v", set)
	}
}

func TestDepsValidate(t *testing.T) {
	t.Parallel()
	if err := (Deps{}).validate(); err == nil {
		t.Fatal("empty deps must be rejected: a nil collaborator panics inside a worker instead")
	}
	err := (Deps{Reviewer: &captureReviewer{}}).validate()
	if err == nil {
		t.Fatal("partially filled deps must be rejected")
	}
	// The message must name what is missing — that is the whole point of a
	// startup check over a nil dereference at 03:00.
	for _, want := range []string{"Scanner", "Digester"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name the missing %s", err, want)
		}
	}
	full := Deps{Reviewer: &captureReviewer{}, Scanner: nopScanner{}, Digester: nopDigester{}}
	if err := full.validate(); err != nil {
		t.Errorf("complete deps rejected: %v", err)
	}
}

type nopScanner struct{}

func (nopScanner) ScanRepository(context.Context, domain.Team, string) (*ScanResult, error) {
	return &ScanResult{}, nil
}

type nopDigester struct{}

func (nopDigester) BuildDigest(context.Context, domain.Team, string, time.Time, int) (*DigestOutcome, error) {
	return &DigestOutcome{}, nil
}
func (nopDigester) SendMessage(context.Context, uuid.UUID) error { return nil }

func TestFindTeam(t *testing.T) {
	t.Parallel()
	teams := []domain.Team{{Name: "Payments"}, {Name: "platform"}}

	// Case-insensitive, because config validation enforces uniqueness under the
	// same comparison — a job arg spelled "payments" must find "Payments".
	if got, ok := findTeam(teams, "PAYMENTS"); !ok || got.Name != "Payments" {
		t.Errorf("findTeam(PAYMENTS) = %+v, %v", got, ok)
	}
	if _, ok := findTeam(teams, "gone"); ok {
		t.Error("a removed team must not resolve")
	}
	if _, ok := findTeam(nil, "payments"); ok {
		t.Error("no teams configured must not resolve")
	}
}

func TestShortSHA(t *testing.T) {
	t.Parallel()
	if got := shortSHA("0123456789abcdef"); got != "01234567" {
		t.Errorf("shortSHA = %q, want 8 characters", got)
	}
	if got := shortSHA("abc"); got != "abc" {
		t.Errorf("a short input must pass through: %q", got)
	}
	if got := shortSHA(""); got != "" {
		t.Errorf("empty = %q", got)
	}
}

// TestCappedBackoff asserts both directions: the delay must grow with the
// attempt and must never exceed the cap.
func TestCappedBackoff(t *testing.T) {
	t.Parallel()
	limit := 10 * time.Second

	if got := cappedBackoff(1, limit); got != time.Second {
		t.Errorf("attempt 1 = %s, want 1s", got)
	}
	if got := cappedBackoff(2, limit); got != 4*time.Second {
		t.Errorf("attempt 2 = %s, want 4s", got)
	}
	if got := cappedBackoff(3, limit); got != 9*time.Second {
		t.Errorf("attempt 3 = %s, want 9s", got)
	}
	if got := cappedBackoff(4, limit); got != limit {
		t.Errorf("attempt 4 = %s, want the cap %s", got, limit)
	}
	if got := cappedBackoff(1000, limit); got != limit {
		t.Errorf("a large attempt = %s, want the cap %s", got, limit)
	}
	// attempt² overflows int64 nanoseconds long before this; the cap must still
	// hold rather than wrapping into a negative delay.
	if got := cappedBackoff(1<<31, limit); got != limit {
		t.Errorf("an overflowing attempt = %s, want the cap %s", got, limit)
	}
}

// TestNextRetryUsesTheCappedSchedule checks the wiring, not the arithmetic:
// both delivery workers must be on the capped backoff rather than River's
// default schedule.
func TestNextRetryUsesTheCappedSchedule(t *testing.T) {
	t.Parallel()
	job := &river.Job[PublishReviewArgs]{JobRow: &rivertype.JobRow{Attempt: 2}}
	delay := time.Until((&PublishReviewWorker{}).NextRetry(job))
	if delay < 3*time.Second || delay > 5*time.Second {
		t.Errorf("publish_review retry delay = %s, want ~4s", delay)
	}

	slackJob := &river.Job[SlackSendArgs]{JobRow: &rivertype.JobRow{Attempt: 100}}
	slackDelay := time.Until((&SlackSendWorker{}).NextRetry(slackJob))
	if slackDelay > 5*time.Minute+time.Second || slackDelay < 4*time.Minute {
		t.Errorf("slack_send retry delay = %s, want the 5m cap", slackDelay)
	}
}

// TestTrackedRecordsTerminalState pins how a worker's outcome becomes a metric.
// The discarded case is the one worth guarding: an error on the last permitted
// attempt is not a retry, and labelling it "failed" would hide exhaustion. The
// snooze case matters for the same reason in reverse: a review deferred because
// another run holds its advisory lock is neither failed nor discarded, and
// counting it as either would make a healthy queue look broken.
//
// Deltas, not absolutes: these are process-global promauto collectors with no
// reset, so an absolute assertion makes `go test -count=2` — the standard way
// to smoke out flakes — fail on this package.
func TestTrackedRecordsTerminalState(t *testing.T) {
	cases := []struct {
		name        string
		attempt     int
		maxAttempts int
		err         error
		wantState   string
	}{
		{"success", 1, 3, nil, stateCompleted},
		{"failure with attempts left", 1, 3, errors.New("boom"), stateFailed},
		{"failure on the last attempt", 3, 3, errors.New("boom"), stateDiscarded},
		{"single-attempt kind fails straight to discarded", 1, 1, errors.New("boom"), stateDiscarded},
		{"a cancelled job is not a discarded one", 1, 3, river.JobCancel(errors.New("401")), stateCancelled},
		{"a snoozed job is neither failed nor discarded", 1, 1, river.JobSnooze(time.Minute), stateScheduled},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A kind unique to the subtest keeps these global counters from
			// colliding with each other or with another package's test.
			kind := "test_" + tc.name
			job := &river.Job[CleanupArgs]{JobRow: &rivertype.JobRow{
				Kind: kind, Attempt: tc.attempt, MaxAttempts: tc.maxAttempts,
			}}

			beforeState := testutil.ToFloat64(metrics.RiverJobsTotal.WithLabelValues(kind, tc.wantState))
			beforeRetries := testutil.ToFloat64(metrics.RiverJobRetriesTotal.WithLabelValues(kind))

			err := tracked(job, func() error { return tc.err })
			if !errors.Is(err, tc.err) {
				t.Fatalf("tracked swallowed the error: %v", err)
			}
			if got := testutil.ToFloat64(metrics.RiverJobsTotal.WithLabelValues(kind, tc.wantState)); got != beforeState+1 {
				t.Errorf("river_jobs_total{kind=%q,state=%q} = %v, want %v", kind, tc.wantState, got, beforeState+1)
			}
			if got := testutil.CollectAndCount(metrics.RiverJobDurationSeconds); got == 0 {
				t.Error("no duration was observed")
			}
			wantRetries := beforeRetries
			if tc.attempt > 1 {
				wantRetries++
			}
			if got := testutil.ToFloat64(metrics.RiverJobRetriesTotal.WithLabelValues(kind)); got != wantRetries {
				t.Errorf("river_job_retries_total{kind=%q} = %v, want %v", kind, got, wantRetries)
			}
		})
	}
}

// TestDigestSlotForNow is the reconciliation rule behind RunOnStart (§6.6).
//
// River's periodic enqueuer is leader-only and keeps its schedule in memory, so
// a leadership turnover across a slot instant loses that slot outright: no job,
// no digest_runs row, and nothing that looks for missing ones. RunOnStart is
// the documented hedge; this function is what keeps it from also firing
// yesterday's digest at 03:00.
func TestDigestSlotForNow(t *testing.T) {
	t.Parallel()
	sched, err := scheduler.NewDigest()
	if err != nil {
		t.Fatal(err)
	}
	loc := sched.Location()
	at := func(h, m int) time.Time { return time.Date(2026, 8, 13, h, m, 0, 0, loc) }

	cases := []struct {
		name     string
		at       time.Time
		wantSlot string
		wantDate string
		wantOK   bool
	}{
		// The scheduled firings: the enqueuer calls the constructor just after
		// the slot elapsed, so these must always insert.
		{"exactly on the morning slot", at(9, 0), "09:00", "2026-08-13", true},
		{"a moment after the morning slot", at(9, 0).Add(31 * time.Second), "09:00", "2026-08-13", true},
		{"exactly on the afternoon slot", at(16, 30), "16:30", "2026-08-13", true},
		// The RunOnStart firings: a leader elected later in the day still owes
		// today's slot, and a repeat is absorbed by digest_runs' unique index.
		{"a leader elected at 11:00 still owes the 09:00 slot", at(11, 0), "09:00", "2026-08-13", true},
		{"late evening owes the 16:30 slot", at(23, 59), "16:30", "2026-08-13", true},
		// Before the first slot of the day the instant belongs to yesterday's
		// last one. Yesterday's digest is not worth sending.
		{"03:00 belongs to yesterday and is refused", at(3, 0), "", "", false},
		{"one second before the morning slot", at(9, 0).Add(-time.Second), "", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			args, ok := digestSlotForNow("payments", sched, tc.at)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (args %+v)", ok, tc.wantOK, args)
			}
			if !ok {
				return
			}
			if args.Slot != tc.wantSlot || args.RunDate != tc.wantDate {
				t.Errorf("args = %+v, want slot %s on %s", args, tc.wantSlot, tc.wantDate)
			}
			if args.Attempt != 0 {
				t.Errorf("attempt = %d, want 0: a reconciled slot is still the scheduled run, and attempt 0 is what makes two replicas collapse into one job",
					args.Attempt)
			}
		})
	}

	// A schedule that can name no slot must insert nothing rather than invent a
	// name digest_runs' unique index cannot recognise on the next pass.
	if _, ok := digestSlotForNow("payments", scheduler.Daily{Loc: loc}, at(11, 0)); ok {
		t.Error("a schedule with no slots produced a digest run")
	}
}

// TestDigestPeriodicJobRunsOnStart pins the flag itself. digestSlotForNow is
// only reachable from the periodic constructor, and with RunOnStart off the
// constructor is never called outside a scheduled firing — which is exactly the
// slot a leadership turnover loses.
//
// River keeps PeriodicJobOpts in an unexported field, so this reads it through
// reflection. That is worth it here: the alternative is an end-to-end test
// whose outcome depends on the wall-clock hour it runs at.
func TestDigestPeriodicJobRunsOnStart(t *testing.T) {
	t.Parallel()
	cfg := Config{Teams: []domain.Team{{Name: "payments"}}}.normalized()
	sched, err := scheduler.NewDigest()
	if err != nil {
		t.Fatal(err)
	}

	jobs := periodicJobs(cfg, sched)
	digest := jobs[len(jobs)-1] // scan, cleanup, then one per team

	opts := reflect.ValueOf(digest).Elem().FieldByName("opts")
	if !opts.IsValid() || opts.IsNil() {
		t.Fatal("river.PeriodicJob has no opts field any more; re-pin this test against the new shape")
	}
	if id := opts.Elem().FieldByName("ID").String(); id != "digest:payments" {
		t.Fatalf("inspected the wrong periodic job: ID = %q", id)
	}
	if !opts.Elem().FieldByName("RunOnStart").Bool() {
		t.Error("the digest schedule does not run on start: a slot that falls into a leadership turnover is lost outright")
	}
}

// TestPeriodicJobsCoverEveryTeam guards the fan-out shape: one digest schedule
// per team plus the two interval jobs. A single shared digest job would send
// only the first team's digest, silently.
func TestPeriodicJobsCoverEveryTeam(t *testing.T) {
	t.Parallel()
	cfg := Config{Teams: []domain.Team{{Name: "payments"}, {Name: "platform"}}}.normalized()
	sched, err := scheduler.NewDigest()
	if err != nil {
		t.Fatal(err)
	}

	got := periodicJobs(cfg, sched)
	if len(got) != 2+len(cfg.Teams) {
		t.Fatalf("periodic jobs = %d, want scan + cleanup + one digest per team", len(got))
	}

	// No teams still leaves the two interval jobs: cleanup must run even in a
	// deployment that has not been given a team yet.
	if got := periodicJobs(Config{}.normalized(), sched); len(got) != 2 {
		t.Errorf("periodic jobs without teams = %d, want scan + cleanup", len(got))
	}
}

func TestOnPersistIsNilWhenPublishingIsOff(t *testing.T) {
	t.Parallel()
	svc := &Service{log: quietLogger(), cfg: Config{PublishEnabled: false}}
	rev := &captureReviewer{}
	if _, err := svc.runReview(t.Context(), ReviewArgs{}, rev); err != nil {
		t.Fatal(err)
	}
	if rev.hadPersist {
		t.Fatal("a dry run must not be able to enqueue publication at all")
	}
}

// Guard against the seam drifting: OnPersist must keep taking the raw pgx.Tx,
// because that is exactly what river's InsertTx accepts and what store.RunInTx
// hands over. A wrapper type here would break the one-transaction guarantee.
var _ OnPersist = func(context.Context, pgx.Tx, uuid.UUID) error { return nil }

// recordingDigester captures the run date the worker parsed.
type recordingDigester struct {
	runDate time.Time
	calls   int
}

func (d *recordingDigester) BuildDigest(_ context.Context, _ domain.Team, _ string, runDate time.Time, _ int) (*DigestOutcome, error) {
	d.calls++
	d.runDate = runDate
	return &DigestOutcome{Status: "built"}, nil
}
func (d *recordingDigester) SendMessage(context.Context, uuid.UUID) error { return nil }

// TestDigestWorkerUsesTheScheduleAccessor: scheduler.Daily.Location() is the
// nil-safe accessor the package provides, and time.ParseInLocation panics on a
// nil *time.Location. Reading the Loc field directly turns a degenerate
// schedule into a worker panic instead of the UTC fallback Next and SlotAt both
// take.
func TestDigestWorkerUsesTheScheduleAccessor(t *testing.T) {
	t.Parallel()
	svc := &Service{log: quietLogger(), cfg: Config{
		Teams:            []domain.Team{{Name: "payments"}},
		SlackSendEnabled: false, // returns before the queue is touched
	}}
	dg := &recordingDigester{}
	// Times set, Loc deliberately nil — the shape a hand-built schedule has.
	w := &DigestWorker{log: quietLogger(), svc: svc, digester: dg,
		schedule: scheduler.Daily{Times: []scheduler.Clock{{Hour: 9}}}}

	err := w.Work(t.Context(), testJob(DigestArgs{Team: "payments", Slot: "09:00", RunDate: "2026-08-13"}))
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if dg.calls != 1 {
		t.Fatalf("BuildDigest called %d times", dg.calls)
	}
	want := time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
	if !dg.runDate.Equal(want) {
		t.Errorf("run date = %s, want the UTC fallback %s", dg.runDate, want)
	}
}

// TestWithShutdownCancelsReviewsAtStop pins the arithmetic that made this
// necessary: a review takes 4-10 minutes and the default drain window is one, so
// a review left in the drain cannot finish — it only holds the shutdown open for
// the full window and keeps paying the model for output that is discarded when
// the window closes. Measured on the first live run: Ctrl-C, then a full minute
// of LLM passes, then "signal: killed".
func TestWithShutdownCancelsReviewsAtStop(t *testing.T) {
	t.Parallel()
	svc := &Service{log: quietLogger(), stopping: make(chan struct{})}

	ctx, cancel := svc.withShutdown(t.Context())
	defer cancel()

	select {
	case <-ctx.Done():
		t.Fatal("the review context must be live before the shutdown starts")
	case <-time.After(20 * time.Millisecond):
	}

	svc.beginStop()
	svc.beginStop() // idempotent: Stop may be reached twice, and a double close is fatal

	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("the review context was not cancelled when the shutdown began")
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Errorf("ctx.Err() = %v, want context.Canceled — service.recordFailure discriminates on it", ctx.Err())
	}
}

// A review that finishes normally must not leave its watcher behind: the cancel
// func is the only thing that ends it, and there is one per in-flight review.
func TestWithShutdownWatcherEndsWithItsJob(t *testing.T) {
	t.Parallel()
	svc := &Service{log: quietLogger(), stopping: make(chan struct{})}

	before := runtime.NumGoroutine()
	for range 50 {
		_, cancel := svc.withShutdown(t.Context())
		cancel()
	}
	// The watchers are woken by cancel(); give the scheduler a moment to run them.
	for range 100 {
		if runtime.NumGoroutine() <= before {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("goroutines = %d, want back to %d: the shutdown watchers are leaking",
		runtime.NumGoroutine(), before)
}
