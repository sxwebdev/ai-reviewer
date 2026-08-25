package jobs_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/sxwebdev/ai-reviewer/internal/jobs"
)

// These tests need a throwaway PostgreSQL; they skip without
// AI_REVIEWER_TEST_PG_DSN (see storetest), so `make test` stays green.

// TestReviewUniquenessDedupesInFlight is §6.1's core promise: two replicas
// cannot review the same SHA at once, and a re-enqueue while one is in flight
// is a no-op rather than a second LLM run.
func TestReviewUniquenessDedupesInFlight(t *testing.T) {
	f := newFixture(t)
	q := f.newQueue(t)
	args := f.reviewArgs()

	first, err := q.EnqueueReview(t.Context(), args)
	if err != nil {
		t.Fatal(err)
	}
	if first.Deduplicated {
		t.Fatal("the first insert must not be a duplicate")
	}

	second, err := q.EnqueueReview(t.Context(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Deduplicated {
		t.Error("the second insert must be reported as deduplicated")
	}
	if second.ID() != first.ID() {
		t.Errorf("dedupe returned job %d, want the in-flight %d — the CLI inspects this row",
			second.ID(), first.ID())
	}
	if got := f.count(t, jobs.KindReview, "head_sha", args.HeadSHA); got != 1 {
		t.Errorf("review jobs for this SHA = %d, want 1", got)
	}

	// A different head SHA is different work and must not be swallowed.
	other := args
	other.HeadSHA = f.sha("b")
	third, err := q.EnqueueReview(t.Context(), other)
	if err != nil {
		t.Fatal(err)
	}
	if third.Deduplicated {
		t.Error("a new head SHA was deduped into the previous one")
	}
	if got := f.count(t, jobs.KindReview, "head_sha", other.HeadSHA); got != 1 {
		t.Errorf("review jobs for the new SHA = %d, want 1", got)
	}
}

// TestReviewReinsertAfterCompletionIsAllowed is the mutation check for the
// explicit ByState list. River's default includes Completed, and with it this
// second insert would silently collapse into the finished job: re-driving
// publish_review after its attempts ran out would be impossible, and a
// re-review of a SHA whose previous review completed would vanish.
func TestReviewReinsertAfterCompletionIsAllowed(t *testing.T) {
	f := newFixture(t)
	q := f.newQueue(t)
	args := f.reviewArgs()

	first, err := q.EnqueueReview(t.Context(), args)
	if err != nil {
		t.Fatal(err)
	}
	finalize(t, f.pool, first.ID(), "completed")

	second, err := q.EnqueueReview(t.Context(), args)
	if err != nil {
		t.Fatal(err)
	}
	if second.Deduplicated {
		t.Fatal("a completed job blocked a re-insert: ByState must list in-flight states only")
	}
	if second.ID() == first.ID() {
		t.Fatal("the re-insert returned the finished job")
	}
	if got := f.count(t, jobs.KindReview, "head_sha", args.HeadSHA); got != 2 {
		t.Errorf("review jobs = %d, want 2", got)
	}
}

// TestPublishReviewReinsertAfterDiscardIsAllowed is the same property where it
// matters most operationally: a publication that exhausted its ten attempts
// must be re-drivable by the scan_repo sweep.
func TestPublishReviewReinsertAfterDiscardIsAllowed(t *testing.T) {
	f := newFixture(t)
	q := f.newQueue(t)
	id := uuid.New()

	first, err := q.EnqueuePublishReview(t.Context(), jobs.PublishReviewArgs{ReviewID: id})
	if err != nil {
		t.Fatal(err)
	}
	finalize(t, f.pool, first.ID(), "discarded")

	second, err := q.EnqueuePublishReview(t.Context(), jobs.PublishReviewArgs{ReviewID: id})
	if err != nil {
		t.Fatal(err)
	}
	if second.Deduplicated {
		t.Fatal("a discarded publication blocked its own re-drive")
	}
}

// TestPublishFlagIsNotPartOfTheUniqueKey is §15's explicit requirement: the
// flag reaches the worker through the args, and it does not widen the key.
// If it did, a manual --publish run and the scanner's dry run would coexist for
// one SHA and both publishers would post every finding.
func TestPublishFlagIsNotPartOfTheUniqueKey(t *testing.T) {
	f := newFixture(t)
	q := f.newQueue(t)
	args := f.reviewArgs()

	// The scanner's insert: Publish unset, decided from config at run time.
	scanner, err := q.EnqueueReview(t.Context(), args)
	if err != nil {
		t.Fatal(err)
	}

	// The CLI's insert: same SHA, explicitly publishing.
	publish := true
	manualArgs := args
	manualArgs.Publish = &publish
	manual, err := q.EnqueueReview(t.Context(), manualArgs)
	if err != nil {
		t.Fatal(err)
	}

	if !manual.Deduplicated {
		t.Fatal("--publish widened the uniqueness key: two reviews of one SHA would run")
	}
	if manual.ID() != scanner.ID() {
		t.Errorf("dedupe returned %d, want the in-flight %d", manual.ID(), scanner.ID())
	}
	if got := f.count(t, jobs.KindReview, "head_sha", args.HeadSHA); got != 1 {
		t.Errorf("review jobs = %d, want 1", got)
	}

	// The CLI's warning depends on being able to read the surviving job's args
	// and see that it is *not* going to publish.
	existing, err := jobs.DecodeArgs[jobs.ReviewArgs](manual.Job)
	if err != nil {
		t.Fatal(err)
	}
	if existing.Publish != nil {
		t.Errorf("the surviving job's args = %+v, want the scanner's unset Publish", existing.Publish)
	}
}

// TestDigestUniquenessPerSlot covers §6.6: two replicas cannot double-send a
// scheduled slot, while --force (the next attempt) is a genuinely new run.
func TestDigestUniquenessPerSlot(t *testing.T) {
	f := newFixture(t)
	q := f.newQueue(t)
	team := f.team(0).Name
	base := jobs.DigestArgs{Team: team, Slot: "09:00", RunDate: "2026-08-13", Attempt: 0}

	if res, err := q.EnqueueDigest(t.Context(), base); err != nil || res.Deduplicated {
		t.Fatalf("first insert: %v deduped=%v", err, res.Deduplicated)
	}
	second, err := q.EnqueueDigest(t.Context(), base)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Deduplicated {
		t.Fatal("two replicas queued the same slot twice")
	}

	variants := []struct {
		name string
		args jobs.DigestArgs
	}{
		{"--force takes the next attempt", jobs.DigestArgs{Team: team, Slot: "09:00", RunDate: "2026-08-13", Attempt: 1}},
		{"the other slot of the same day", jobs.DigestArgs{Team: team, Slot: "17:30", RunDate: "2026-08-13", Attempt: 0}},
		{"the next day", jobs.DigestArgs{Team: team, Slot: "09:00", RunDate: "2026-08-14", Attempt: 0}},
		{"another team", jobs.DigestArgs{Team: f.team(1).Name, Slot: "09:00", RunDate: "2026-08-13", Attempt: 0}},
	}
	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			res, err := q.EnqueueDigest(t.Context(), v.args)
			if err != nil {
				t.Fatal(err)
			}
			if res.Deduplicated {
				t.Errorf("%+v was deduped into the 09:00 run", v.args)
			}
		})
	}
	// Three of the four variants belong to this team; the fourth is the other.
	if got := f.count(t, jobs.KindDigest, "team", team); got != 4 {
		t.Errorf("digest jobs for %s = %d, want 4", team, got)
	}
	if got := f.count(t, jobs.KindDigest, "team", f.team(1).Name); got != 1 {
		t.Errorf("digest jobs for the second team = %d, want 1", got)
	}
}

// TestSlackSendUniquenessPerMessage: one digest_messages row is one delivery.
func TestSlackSendUniquenessPerMessage(t *testing.T) {
	f := newFixture(t)
	q := f.newQueue(t)
	id := uuid.New()

	if _, err := q.EnqueueSlackSend(t.Context(), jobs.SlackSendArgs{MessageID: id, Team: "payments"}); err != nil {
		t.Fatal(err)
	}
	// A different team label must not create a second delivery of one message.
	res, err := q.EnqueueSlackSend(t.Context(), jobs.SlackSendArgs{MessageID: id, Team: "platform"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Deduplicated {
		t.Error("the team label widened the key: one message would be posted twice")
	}
	if got := f.count(t, jobs.KindSlackSend, "message_id", id.String()); got != 1 {
		t.Errorf("slack_send jobs = %d, want 1", got)
	}
}

// TestScanRepoDedupesPerRepository: two concurrent passes over one repository
// would double every review they find.
func TestScanRepoDedupesPerRepository(t *testing.T) {
	f := newFixture(t)
	q := f.newQueue(t)

	if _, err := q.EnqueueScanRepo(t.Context(), jobs.ScanRepoArgs{Team: f.team(0).Name, Repository: f.repo(0)}); err != nil {
		t.Fatal(err)
	}
	dup, err := q.EnqueueScanRepo(t.Context(), jobs.ScanRepoArgs{Team: f.team(0).Name, Repository: f.repo(0)})
	if err != nil {
		t.Fatal(err)
	}
	if !dup.Deduplicated {
		t.Error("one repository was queued for two concurrent scans")
	}
	other, err := q.EnqueueScanRepo(t.Context(), jobs.ScanRepoArgs{Team: f.team(0).Name, Repository: f.repo(1)})
	if err != nil {
		t.Fatal(err)
	}
	if other.Deduplicated {
		t.Error("a second repository was deduped into the first")
	}
	if got := f.count(t, jobs.KindScanRepo, "repository", f.repo(0)); got != 1 {
		t.Errorf("scan_repo jobs for one repository = %d, want 1", got)
	}
}

// TestScanDedupesPerKind: §6.2 keys scan uniqueness on the kind alone, so a
// team-scoped request collapses into a full pass that is already running. The
// CLI reports that rather than exiting silently successful.
//
// This one asserts on the insert result rather than on a row count, because the
// whole point is that the args do not participate in the key — there is nothing
// to scope a count by.
func TestScanDedupesPerKind(t *testing.T) {
	f := newFixture(t)
	q := f.newQueue(t)

	first, err := q.EnqueueScan(t.Context(), jobs.ScanArgs{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := q.EnqueueScan(t.Context(), jobs.ScanArgs{Team: f.team(0).Name})
	if err != nil {
		t.Fatal(err)
	}
	if !second.Deduplicated {
		t.Error("a second scan pass started while one was already in flight")
	}
	if second.ID() != first.ID() {
		t.Errorf("dedupe returned job %d, want the in-flight %d", second.ID(), first.ID())
	}
}

// TestPublishEnqueuedInsideTheReviewTransaction is §10.4: the review write and
// the publication enqueue commit together or not at all. A crash between them
// would strand the review forever — the next scan sees the head SHA as reviewed
// and enqueues neither job.
func TestPublishEnqueuedInsideTheReviewTransaction(t *testing.T) {
	t.Run("rollback leaves no job", func(t *testing.T) {
		f := newFixture(t)
		rev := &fakeReviewer{store: f.store(t), rollback: true}
		svc := f.newService(t, jobs.Config{PublishEnabled: true}, newDeps(rev, nil, nil))

		if _, err := svc.RunReviewLocal(t.Context(), f.reviewArgs()); !errors.Is(err, errForcedRollback) {
			t.Fatalf("err = %v, want the forced rollback", err)
		}
		if got := f.count(t, jobs.KindPublishReview, "review_id", rev.reviewID().String()); got != 0 {
			t.Errorf("publish_review jobs after a rollback = %d, want 0", got)
		}
	})

	t.Run("commit guarantees the job", func(t *testing.T) {
		f := newFixture(t)
		rev := &fakeReviewer{store: f.store(t)}
		svc := f.newService(t, jobs.Config{PublishEnabled: true}, newDeps(rev, nil, nil))

		if _, err := svc.RunReviewLocal(t.Context(), f.reviewArgs()); err != nil {
			t.Fatalf("RunReviewLocal: %v", err)
		}
		if got := f.count(t, jobs.KindPublishReview, "review_id", rev.reviewID().String()); got != 1 {
			t.Errorf("publish_review jobs after a commit = %d, want 1", got)
		}
	})
}

// TestRunReviewLocalRefusesWhileAQueuedJobHoldsTheSHA is §17's `--local`
// bullet, established the way the bullet means it: a **River job** is running
// the review, and the local run must decline.
//
// The precondition is a real worker holding the lock, not the test taking it.
// Taking it here would only prove that a second `--local` is excluded — the one
// case the old one-sided lock already covered — while the message the CLI
// prints ("by a queued job or another --local run") promises both.
func TestRunReviewLocalRefusesWhileAQueuedJobHoldsTheSHA(t *testing.T) {
	f := newFixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	rev := &fakeReviewer{started: started, block: release}
	// A short drain keeps the failure path quick: if the assertions below fail
	// before the worker is released, shutdown must not wait out the default.
	svc := f.newService(t, jobs.Config{ReviewWorkers: 1, DrainTimeout: 2 * time.Second}, newDeps(rev, nil, nil))
	args := f.reviewArgs()

	if _, err := svc.EnqueueReview(t.Context(), args); err != nil {
		t.Fatal(err)
	}
	stop := runService(t, svc)

	select {
	case <-started:
	case <-time.After(30 * time.Second):
		t.Fatal("the review worker never started")
	}

	// The queued job is inside RunReview right now. --local must not start a
	// second pipeline on the same SHA.
	if _, err := svc.RunReviewLocal(t.Context(), args); !errors.Is(err, jobs.ErrReviewLocked) {
		t.Fatalf("err = %v, want ErrReviewLocked while a queued job is reviewing this SHA", err)
	}

	// A different SHA is different work and must not be blocked.
	other := args
	other.HeadSHA = f.sha("z")
	if _, err := svc.RunReviewLocal(t.Context(), other); err != nil {
		t.Errorf("an unrelated SHA was blocked: %v", err)
	}

	close(release)
	stop()

	// Once the job is done the lock is gone: a crashed or finished run must not
	// poison the SHA.
	if _, err := svc.RunReviewLocal(t.Context(), args); err != nil {
		t.Errorf("the worker did not release the lock: %v", err)
	}
	if reqs, _ := rev.snapshot(); len(reqs) != 3 {
		// the queued job, the unrelated SHA, and the final local run
		t.Errorf("RunReview ran %d times, want 3", len(reqs))
	}

	// Every one of those runs must have given its lock back. A review lock left
	// behind does not fail anything — it makes the next review of that SHA wait
	// for a connection to be recycled.
	f.assertNoAdvisoryLocks(t)
}

// TestReviewJobSnoozesWhenTheSHAIsLocked is the other half of the same lock:
// the worker must not run a review somebody else is already running, and must
// not burn its single attempt saying so. review has MaxAttempts = 1, so an
// error here would discard the job and leave the SHA to the next scan pass.
func TestReviewJobSnoozesWhenTheSHAIsLocked(t *testing.T) {
	f := newFixture(t)
	rev := &fakeReviewer{}
	svc := f.newService(t, jobs.Config{}, newDeps(rev, nil, nil))
	args := f.reviewArgs()

	// A `--local` run in another process holds the lock.
	release, ok, err := jobs.TryLock(t.Context(), f.pool, jobs.LocalLockKey(args))
	if err != nil || !ok {
		t.Fatalf("TryLock: %v ok=%v", err, ok)
	}
	defer release()

	err = jobs.WorkReview(t.Context(), svc, args)
	var snooze *river.JobSnoozeError
	if !errors.As(err, &snooze) {
		t.Fatalf("err = %v, want a JobSnooze so the single attempt survives", err)
	}
	if snooze.Duration <= 0 {
		t.Errorf("snooze duration = %s, want a real delay", snooze.Duration)
	}
	if reqs, _ := rev.snapshot(); len(reqs) != 0 {
		t.Errorf("the worker reviewed anyway: %d requests", len(reqs))
	}

	// Released, the same job proceeds — the snooze defers work, it does not
	// drop it.
	release()
	if err := jobs.WorkReview(t.Context(), svc, args); err != nil {
		t.Fatalf("after the lock was released: %v", err)
	}
	if reqs, _ := rev.snapshot(); len(reqs) != 1 {
		t.Errorf("RunReview ran %d times, want 1", len(reqs))
	}
}

// runService starts the River client and returns a function that stops it and
// waits for the drain.
func runService(t *testing.T, svc *jobs.Service) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- svc.Start(ctx) }()

	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			<-done
			stopCtx, stopCancel := context.WithTimeout(context.WithoutCancel(t.Context()), 30*time.Second)
			defer stopCancel()
			if err := svc.Stop(stopCtx); err != nil {
				t.Errorf("Stop: %v", err)
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func TestTryLockIsExclusiveAndReleasable(t *testing.T) {
	f := newFixture(t)
	key := "review:" + f.token

	release, ok, err := jobs.TryLock(t.Context(), f.pool, key)
	if err != nil || !ok {
		t.Fatalf("first TryLock: %v ok=%v", err, ok)
	}
	if _, ok, err := jobs.TryLock(t.Context(), f.pool, key); err != nil || ok {
		t.Fatalf("second TryLock: %v ok=%v, want ok=false", err, ok)
	}
	release()

	release2, ok, err := jobs.TryLock(t.Context(), f.pool, key)
	if err != nil || !ok {
		t.Fatalf("TryLock after release: %v ok=%v", err, ok)
	}
	release2()
	// Releasing twice must be harmless: the CLI defers it and may also call it
	// on an error path.
	release2()
}

// TestReviewRunsThroughRiver is the one end-to-end pass: it proves the worker
// registration, the queue names in InsertOpts and the args encoding all agree,
// which unit tests of each piece cannot.
func TestReviewRunsThroughRiver(t *testing.T) {
	f := newFixture(t)
	rev := &fakeReviewer{}
	// PublishEnabled is off in config; the job asks for publication anyway.
	svc := f.newService(t, jobs.Config{PublishEnabled: false, ReviewWorkers: 1}, newDeps(rev, nil, nil))

	publish := true
	args := f.reviewArgs()
	args.Publish = &publish
	res, err := svc.EnqueueReview(t.Context(), args)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- svc.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
		stopCtx, stopCancel := context.WithTimeout(context.WithoutCancel(t.Context()), 10*time.Second)
		defer stopCancel()
		if err := svc.Stop(stopCtx); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})

	row, err := svc.Wait(ctx, res.ID(), 50*time.Millisecond, 30*time.Second)
	if err != nil {
		t.Fatalf("waiting for the review job: %v (state %v)", err, row.State)
	}
	if row.State != rivertype.JobStateCompleted {
		t.Fatalf("job state = %s, errors = %v", row.State, row.Errors)
	}

	// Exactly once, and for exactly this merge request: river_job is truncated
	// per test, so a second call here would mean a job worked twice or a stray
	// one picked up, both of which are bugs worth failing on.
	reqs, hadPersist := rev.snapshot()
	if len(reqs) != 1 {
		t.Fatalf("RunReview ran %d times, want exactly 1: %+v", len(reqs), reqs)
	}
	got := reqs[0]
	if got.HeadSHA != args.HeadSHA {
		t.Fatalf("the worker was handed %s, want %s", got.HeadSHA, args.HeadSHA)
	}
	if !got.Publish {
		t.Error("--publish did not reach the worker through the job args")
	}
	if !hadPersist[0] {
		t.Error("a publishing review was given no publication hand-off")
	}
	if got.ProjectID != args.ProjectID || got.MRIID != args.MRIID {
		t.Errorf("the worker received %+v", got)
	}
}

// TestLockerBlocksUntilReleased covers the blocking half of the advisory-lock
// pair. It is what internal/git's mirror serialization runs on: two reviews of
// one repository both need the mirror, and the second must wait for the first
// rather than fetch concurrently — concurrent `git fetch` on one mirror does
// not duplicate work, it fails with `cannot lock ref` and silently degrades the
// loser to a diff-only review.
//
// The mutation this is really guarding: pg_try_advisory_lock through Exec looks
// identical to pg_advisory_lock — it returns a row either way, so the error is
// nil whether or not the lock was granted.
func TestLockerBlocksUntilReleased(t *testing.T) {
	f := newFixture(t)
	locker := jobs.NewLocker(f.pool)
	// Unique per run: advisory locks are database-wide and outlive a killed
	// test binary until Postgres notices the dead socket.
	key := "git-mirror:test/" + f.token

	release, err := locker.Lock(t.Context(), key)
	if err != nil {
		t.Fatalf("first Lock: %v", err)
	}
	defer release()

	// A second holder must wait, not be handed the lock. The deadline is what
	// turns "waits" into an assertion.
	waiting, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	start := time.Now()
	if granted, err := locker.Lock(waiting, key); err == nil {
		// Release before failing: the lock pins a checked-out connection, and
		// the pool's cleanup waits for it, so leaking it here would hang the
		// run instead of failing it.
		granted()
		t.Fatal("two holders of one mirror lock: the second was granted it while the first held it")
	}
	if waited := time.Since(start); waited < 500*time.Millisecond {
		t.Errorf("the second Lock gave up after %s: it must block, not poll-and-fail", waited)
	}

	// An unrelated key is unrelated work.
	other, err := locker.Lock(t.Context(), key+":other")
	if err != nil {
		t.Fatalf("an unrelated key was blocked: %v", err)
	}
	other()

	release()
	second, err := locker.Lock(t.Context(), key)
	if err != nil {
		t.Fatalf("Lock after release: %v", err)
	}
	// Releasing twice must be harmless — callers defer it and may also release
	// explicitly, and a double Release of a pooled connection panics.
	second()
	second()

	f.assertNoAdvisoryLocks(t)
}
