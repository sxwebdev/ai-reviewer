package state

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestJobEnqueueClaimComplete(t *testing.T) {
	db := openTestDB(t)
	ctx := t.Context()

	j := &Job{Type: JobSync}
	if err := db.EnqueueJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	if j.ID == "" {
		t.Fatal("enqueue should assign an id")
	}

	claimed, err := db.ClaimJob(ctx, "w1")
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ID != j.ID || claimed.Status != JobRunning || claimed.Attempts != 1 {
		t.Errorf("claimed wrong: %+v", claimed)
	}

	// No more runnable jobs.
	if _, err := db.ClaimJob(ctx, "w1"); err != ErrNotFound {
		t.Errorf("expected ErrNotFound, got %v", err)
	}

	if err := db.CompleteJob(ctx, j.ID, JobSuccess, ""); err != nil {
		t.Fatal(err)
	}
	jobs, _ := db.ListJobs(ctx, 10)
	if len(jobs) != 1 || jobs[0].Status != JobSuccess {
		t.Errorf("expected success, got %+v", jobs)
	}
}

func TestJobRetryAndExhaustion(t *testing.T) {
	db := openTestDB(t)
	ctx := t.Context()

	// max_attempts=1: after one claim, a retry must fail (exhausted).
	j := &Job{Type: JobReview, MaxAttempts: 1}
	if err := db.EnqueueJob(ctx, j); err != nil {
		t.Fatal(err)
	}
	claimed, err := db.ClaimJob(ctx, "w1")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	retried, err := db.RetryJob(ctx, claimed, "boom", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if retried {
		t.Error("should not retry when attempts exhausted")
	}
	jobs, _ := db.ListJobs(ctx, 10)
	if jobs[0].Status != JobFailed {
		t.Errorf("expected failed, got %q", jobs[0].Status)
	}

	// A job with attempts remaining requeues with a future run_after.
	j2 := &Job{Type: JobReview, MaxAttempts: 3}
	_ = db.EnqueueJob(ctx, j2)
	c2, _ := db.ClaimJob(ctx, "w1")
	retried, _ = db.RetryJob(ctx, c2, "transient", time.Minute)
	if !retried {
		t.Error("should retry when attempts remain")
	}
	// It is queued but not yet runnable (run_after in the future).
	if _, err := db.ClaimJob(ctx, "w1"); err != ErrNotFound {
		t.Errorf("retried job should not be immediately claimable, got %v", err)
	}
}

func TestJobRecoverStuck(t *testing.T) {
	db := openTestDB(t)
	ctx := t.Context()
	j := &Job{Type: JobSync}
	_ = db.EnqueueJob(ctx, j)
	claimed, _ := db.ClaimJob(ctx, "w1") // now running, locked_at ~ now

	// A freshly-locked job is not stuck within an hour.
	if n, _ := db.RecoverStuckJobs(ctx, time.Hour); n != 0 {
		t.Errorf("recovered %d, want 0", n)
	}
	// Backdate the lock two hours; now it is stuck.
	if _, err := db.ExecContext(ctx, `UPDATE jobs SET locked_at = ? WHERE id = ?`,
		nowMillis()-2*int64(time.Hour/time.Millisecond), claimed.ID); err != nil {
		t.Fatal(err)
	}
	if n, _ := db.RecoverStuckJobs(ctx, time.Hour); n != 1 {
		t.Errorf("recovered %d, want 1", n)
	}
	// It is claimable again.
	again, err := db.ClaimJob(ctx, "w2")
	if err != nil || again.ID != claimed.ID {
		t.Errorf("recovered job should be claimable: %+v err=%v", again, err)
	}
}

func TestHasActiveJob(t *testing.T) {
	db := openTestDB(t)
	ctx := t.Context()
	pid, iid := int64(10), int64(5)
	j := &Job{Type: JobReview, ProjectID: &pid, MRIID: &iid}
	_ = db.EnqueueJob(ctx, j)

	if ok, _ := db.HasActiveJob(ctx, JobReview, 10, 5); !ok {
		t.Error("queued job should be active")
	}
	if ok, _ := db.HasActiveJob(ctx, JobReview, 10, 99); ok {
		t.Error("different iid should not match")
	}
	_ = db.CompleteJob(ctx, j.ID, JobSuccess, "")
	if ok, _ := db.HasActiveJob(ctx, JobReview, 10, 5); ok {
		t.Error("completed job should not be active")
	}
}

func TestJobNoDoubleClaimUnderConcurrency(t *testing.T) {
	db := openTestDB(t)
	ctx := t.Context()
	const total = 30
	for i := 0; i < total; i++ {
		if err := db.EnqueueJob(ctx, &Job{Type: JobSync}); err != nil {
			t.Fatal(err)
		}
	}

	var mu sync.Mutex
	seen := map[string]int{}
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				j, err := db.ClaimJob(ctx, "w")
				if err == ErrNotFound {
					return
				}
				if err != nil {
					return
				}
				mu.Lock()
				seen[j.ID]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(seen) != total {
		t.Errorf("claimed %d distinct jobs, want %d", len(seen), total)
	}
	for id, c := range seen {
		if c != 1 {
			t.Errorf("job %s claimed %d times (double-claim)", id, c)
		}
	}
}

// TestLatestReviewJobs proves the targeted latest-review-job queries pick the
// newest job per MR (regardless of volume), exclude non-review jobs, and report
// ErrNotFound for an MR with no review job.
func TestLatestReviewJobs(t *testing.T) {
	db := openTestDB(t)
	ctx := t.Context()
	pid, iid := int64(3), int64(9)

	enqueue := func(id, typ, status string, p, i, createdAt int64) {
		t.Helper()
		if err := db.EnqueueJob(ctx, &Job{ID: id, Type: typ, Status: status, ProjectID: &p, MRIID: &i}); err != nil {
			t.Fatal(err)
		}
		// EnqueueJob stamps now(); override created_at to make ordering deterministic.
		if _, err := db.ExecContext(ctx, `UPDATE jobs SET created_at = ? WHERE id = ?`, createdAt, id); err != nil {
			t.Fatal(err)
		}
	}
	enqueue("old", JobReview, JobFailed, pid, iid, 100)
	enqueue("new", JobReview, JobRunning, pid, iid, 200) // newest for (3,9)
	enqueue("other", JobReview, JobSuccess, 4, iid, 150) // different MR
	enqueue("sync", JobSync, JobRunning, pid, iid, 300)  // not a review job

	latest, err := db.GetLatestReviewJob(ctx, pid, iid)
	if err != nil {
		t.Fatal(err)
	}
	if latest.ID != "new" || latest.Status != JobRunning {
		t.Errorf("GetLatestReviewJob = %s/%s, want new/running", latest.ID, latest.Status)
	}
	if _, err := db.GetLatestReviewJob(ctx, 999, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetLatestReviewJob(unknown) = %v, want ErrNotFound", err)
	}

	perMR, err := db.LatestReviewJobsPerMR(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, j := range perMR {
		got[fmt.Sprintf("%d/%d", *j.ProjectID, *j.MRIID)] = j.ID
	}
	if len(got) != 2 || got["3/9"] != "new" || got["4/9"] != "other" {
		t.Errorf("LatestReviewJobsPerMR = %v, want {3/9:new, 4/9:other}", got)
	}
}
