package state

import (
	"testing"
	"time"
)

// TestRequestCancelQueued cancels a queued job synchronously and proves the
// worker can no longer claim it.
func TestRequestCancelQueued(t *testing.T) {
	db := openTestDB(t)
	ctx := t.Context()

	j := &Job{Type: JobReview}
	if err := db.EnqueueJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	outcome, err := db.RequestCancelJob(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != CancelDone {
		t.Errorf("outcome = %q, want %q", outcome, CancelDone)
	}
	if _, err := db.ClaimJob(ctx, "w1"); err != ErrNotFound {
		t.Errorf("cancelled job must not be claimable, got %v", err)
	}
	jobs, _ := db.ListJobs(ctx, 10)
	if jobs[0].Status != JobCancelled {
		t.Errorf("status = %q, want cancelled", jobs[0].Status)
	}
}

// TestRequestCancelRunning flags a running job for cancellation rather than
// terminating it directly (the owning worker does that), and the flag surfaces
// via CancelRequestedIDs.
func TestRequestCancelRunning(t *testing.T) {
	db := openTestDB(t)
	ctx := t.Context()

	j := &Job{Type: JobReview}
	_ = db.EnqueueJob(ctx, j)
	claimed, _ := db.ClaimJob(ctx, "w1") // -> running

	outcome, err := db.RequestCancelJob(ctx, claimed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != CancelPending {
		t.Errorf("outcome = %q, want %q", outcome, CancelPending)
	}
	// Still running until the worker acts, but flagged.
	ids, err := db.CancelRequestedIDs(ctx, []string{claimed.ID, "nope"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != claimed.ID {
		t.Errorf("CancelRequestedIDs = %v, want [%s]", ids, claimed.ID)
	}

	// The worker finalizes it.
	if err := db.MarkJobCancelled(ctx, claimed.ID); err != nil {
		t.Fatal(err)
	}
	jobs, _ := db.ListJobs(ctx, 10)
	if jobs[0].Status != JobCancelled || jobs[0].CancelRequested {
		t.Errorf("after finalize: status=%q cancel_requested=%v", jobs[0].Status, jobs[0].CancelRequested)
	}
}

// TestRequestCancelUnknown is a no-op for a job that is neither queued nor running.
func TestRequestCancelUnknown(t *testing.T) {
	db := openTestDB(t)
	ctx := t.Context()
	j := &Job{Type: JobReview}
	_ = db.EnqueueJob(ctx, j)
	_ = db.CompleteJob(ctx, j.ID, JobSuccess, "")

	if outcome, err := db.RequestCancelJob(ctx, j.ID); err != nil || outcome != CancelNoop {
		t.Errorf("RequestCancelJob(success) = %q,%v want noop,nil", outcome, err)
	}
	if outcome, err := db.RequestCancelJob(ctx, "missing"); err != nil || outcome != CancelNoop {
		t.Errorf("RequestCancelJob(missing) = %q,%v want noop,nil", outcome, err)
	}
}

// TestRequeueRunningJob returns a running job to the queue and undoes the claim's
// attempt increment, so a pause/resume cycle doesn't burn the retry budget.
func TestRequeueRunningJob(t *testing.T) {
	db := openTestDB(t)
	ctx := t.Context()

	j := &Job{Type: JobReview}
	_ = db.EnqueueJob(ctx, j)
	claimed, _ := db.ClaimJob(ctx, "w1")
	if claimed.Attempts != 1 {
		t.Fatalf("attempts after claim = %d, want 1", claimed.Attempts)
	}
	// Flag it, then requeue — requeue must clear the flag too.
	_, _ = db.RequestCancelJob(ctx, claimed.ID)
	if err := db.RequeueRunningJob(ctx, claimed.ID); err != nil {
		t.Fatal(err)
	}
	again, err := db.ClaimJob(ctx, "w1")
	if err != nil {
		t.Fatalf("requeued job should be claimable: %v", err)
	}
	if again.ID != claimed.ID {
		t.Fatalf("claimed %s, want %s", again.ID, claimed.ID)
	}
	if again.Attempts != 1 {
		t.Errorf("attempts after requeue+reclaim = %d, want 1 (net zero across the cycle)", again.Attempts)
	}
}

// TestFinalizeOrphanCancels cleans up a queued job still flagged for cancellation
// (e.g. a worker died mid-stop and RecoverStuckJobs requeued it).
func TestFinalizeOrphanCancels(t *testing.T) {
	db := openTestDB(t)
	ctx := t.Context()

	j := &Job{Type: JobReview}
	_ = db.EnqueueJob(ctx, j)
	// Simulate the orphan: queued with cancel_requested=1.
	if _, err := db.ExecContext(ctx, `UPDATE jobs SET cancel_requested = 1 WHERE id = ?`, j.ID); err != nil {
		t.Fatal(err)
	}
	n, err := db.FinalizeOrphanCancels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("finalized %d, want 1", n)
	}
	jobs, _ := db.ListJobs(ctx, 10)
	if jobs[0].Status != JobCancelled {
		t.Errorf("status = %q, want cancelled", jobs[0].Status)
	}
}

// TestRetryJobClearsCancelFlag proves a stop-flagged running job that falls to
// the retry path (a genuine error raced the stop signal) is requeued WITHOUT the
// cancel flag, so ClaimJob's `cancel_requested = 0` guard doesn't wedge it.
func TestRetryJobClearsCancelFlag(t *testing.T) {
	db := openTestDB(t)
	ctx := t.Context()

	j := &Job{Type: JobReview, MaxAttempts: 3}
	_ = db.EnqueueJob(ctx, j)
	claimed, _ := db.ClaimJob(ctx, "w1")
	if _, err := db.RequestCancelJob(ctx, claimed.ID); err != nil { // sets cancel_requested=1
		t.Fatal(err)
	}
	if _, err := db.RetryJob(ctx, claimed, "transient boom", 0); err != nil {
		t.Fatal(err)
	}
	// Must be claimable again (flag cleared) — not wedged.
	again, err := db.ClaimJob(ctx, "w1")
	if err != nil {
		t.Fatalf("retried job should be claimable, got %v (wedged by cancel_requested?)", err)
	}
	if again.ID != claimed.ID || again.CancelRequested {
		t.Errorf("reclaimed = %s cancel_requested=%v, want %s false", again.ID, again.CancelRequested, claimed.ID)
	}
}

// TestRequeueJobClearsCancelFlag proves the UI Retry action clears a lingering
// cancel flag so the requeued job is claimable.
func TestRequeueJobClearsCancelFlag(t *testing.T) {
	db := openTestDB(t)
	ctx := t.Context()

	j := &Job{Type: JobReview}
	_ = db.EnqueueJob(ctx, j)
	claimed, _ := db.ClaimJob(ctx, "w1")
	// Simulate a failed row that still carries the flag.
	if _, err := db.ExecContext(ctx, `UPDATE jobs SET status = ?, cancel_requested = 1 WHERE id = ?`,
		JobFailed, claimed.ID); err != nil {
		t.Fatal(err)
	}
	ok, err := db.RequeueJob(ctx, claimed.ID)
	if err != nil || !ok {
		t.Fatalf("RequeueJob = %v,%v want true,nil", ok, err)
	}
	if _, err := db.ClaimJob(ctx, "w1"); err != nil {
		t.Fatalf("requeued job should be claimable, got %v", err)
	}
}

// TestRecoverStuckFinalizesFlagged proves crash recovery cancels a stuck job
// that was flagged for stop (honoring the user's intent, no zombie) while
// requeuing unflagged stuck jobs as before.
func TestRecoverStuckFinalizesFlagged(t *testing.T) {
	db := openTestDB(t)
	ctx := t.Context()

	mk := func(flagged bool) string {
		j := &Job{Type: JobReview}
		_ = db.EnqueueJob(ctx, j)
		c, _ := db.ClaimJob(ctx, "w1") // -> running
		// Backdate the lock two hours so it counts as stuck; optionally flag it.
		q := `UPDATE jobs SET locked_at = ?`
		if flagged {
			q += `, cancel_requested = 1`
		}
		q += ` WHERE id = ?`
		if _, err := db.ExecContext(ctx, q, nowMillis()-2*int64(time.Hour/time.Millisecond), c.ID); err != nil {
			t.Fatal(err)
		}
		return c.ID
	}
	flaggedID := mk(true)
	plainID := mk(false)

	if n, err := db.RecoverStuckJobs(ctx, time.Hour); err != nil || n != 2 {
		t.Fatalf("RecoverStuckJobs = %d,%v want 2,nil", n, err)
	}
	byID := map[string]*Job{}
	jobs, _ := db.ListJobs(ctx, 10)
	for _, j := range jobs {
		byID[j.ID] = j
	}
	if got := byID[flaggedID]; got.Status != JobCancelled || got.CancelRequested {
		t.Errorf("flagged stuck job = %s cancel_requested=%v, want cancelled false", got.Status, got.CancelRequested)
	}
	if got := byID[plainID]; got.Status != JobQueued {
		t.Errorf("plain stuck job = %s, want queued", got.Status)
	}
}

// TestMarkJobCancelledGuarded proves MarkJobCancelled only affects a running job,
// so it can't clobber a job that was concurrently requeued to queued.
func TestMarkJobCancelledGuarded(t *testing.T) {
	db := openTestDB(t)
	ctx := t.Context()
	j := &Job{Type: JobReview}
	_ = db.EnqueueJob(ctx, j) // status queued, not running
	if err := db.MarkJobCancelled(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	jobs, _ := db.ListJobs(ctx, 10)
	if jobs[0].Status != JobQueued {
		t.Errorf("queued job was clobbered to %q by MarkJobCancelled", jobs[0].Status)
	}
}

// TestJobsPausedFlag round-trips the global pause flag through the settings table.
func TestJobsPausedFlag(t *testing.T) {
	db := openTestDB(t)
	ctx := t.Context()

	if paused, _ := db.JobsPaused(ctx); paused {
		t.Error("default should be not paused")
	}
	if err := db.SetJobsPaused(ctx, true); err != nil {
		t.Fatal(err)
	}
	if paused, _ := db.JobsPaused(ctx); !paused {
		t.Error("expected paused after SetJobsPaused(true)")
	}
	if err := db.SetJobsPaused(ctx, false); err != nil {
		t.Fatal(err)
	}
	if paused, _ := db.JobsPaused(ctx); paused {
		t.Error("expected not paused after SetJobsPaused(false)")
	}
}
