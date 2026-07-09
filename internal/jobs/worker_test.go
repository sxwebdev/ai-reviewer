package jobs

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/sxwebdev/ai-reviewer/internal/state"
)

func testDB(t *testing.T) *state.DB {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func testWorker(db *state.DB) *Worker {
	w := NewWorker(db, 2, slog.New(slog.NewTextHandler(io.Discard, nil)))
	w.poll = 10 * time.Millisecond // fast polling so control signals apply quickly
	return w
}

// waitStatus polls until the job reaches want or the deadline elapses.
func waitStatus(t *testing.T, db *state.DB, id, want string) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		jobs, _ := db.ListJobs(ctx, 50)
		for _, j := range jobs {
			if j.ID == id && j.Status == want {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	jobs, _ := db.ListJobs(ctx, 50)
	for _, j := range jobs {
		if j.ID == id {
			t.Fatalf("job %s status = %q, want %q", id, j.Status, want)
		}
	}
	t.Fatalf("job %s not found, want status %q", id, want)
}

// TestWorkerStopRunningJob proves a stop request cancels the running handler's
// context and lands the job in the terminal 'cancelled' state.
func TestWorkerStopRunningJob(t *testing.T) {
	db := testDB(t)
	w := testWorker(db)

	started := make(chan string, 1)
	w.Register(state.JobReview, func(ctx context.Context, job *state.Job) error {
		started <- job.ID
		<-ctx.Done() // block until cancelled
		return ctx.Err()
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Run(ctx) }()

	j := &state.Job{Type: state.JobReview}
	if err := db.EnqueueJob(ctx, j); err != nil {
		t.Fatal(err)
	}

	var runningID string
	select {
	case runningID = <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never started")
	}

	if outcome, err := db.RequestCancelJob(ctx, runningID); err != nil || outcome != state.CancelPending {
		t.Fatalf("RequestCancelJob = %q,%v want cancelling,nil", outcome, err)
	}
	waitStatus(t, db, runningID, state.JobCancelled)
}

// TestWorkerStopNilReturn is the regression for the partial-success bug: the
// review engine returns a nil error when a stop lands after one pass already
// succeeded. The terminal state must still be 'cancelled', decided from the DB
// flag rather than from the handler's error.
func TestWorkerStopNilReturn(t *testing.T) {
	db := testDB(t)
	w := testWorker(db)

	started := make(chan string, 1)
	w.Register(state.JobReview, func(ctx context.Context, job *state.Job) error {
		started <- job.ID
		<-ctx.Done()
		return nil // simulate the engine's partial-success (nil) after cancellation
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Run(ctx) }()

	j := &state.Job{Type: state.JobReview}
	_ = db.EnqueueJob(ctx, j)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never started")
	}
	if _, err := db.RequestCancelJob(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, db, j.ID, state.JobCancelled)
}

// TestWorkerPauseNilReturn is the partial-success regression for pause: a nil
// error from an interrupted handler must still requeue the job, not complete it.
func TestWorkerPauseNilReturn(t *testing.T) {
	db := testDB(t)
	w := testWorker(db)

	started := make(chan string, 1)
	w.Register(state.JobReview, func(ctx context.Context, job *state.Job) error {
		started <- job.ID
		<-ctx.Done()
		return nil // partial success on interruption
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Run(ctx) }()

	j := &state.Job{Type: state.JobReview}
	_ = db.EnqueueJob(ctx, j)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never started")
	}
	if err := db.SetJobsPaused(ctx, true); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, db, j.ID, state.JobQueued) // requeued, not marked success
}

// TestWorkerPauseRequeuesRunning proves that pausing the queue interrupts a
// running job and returns it to 'queued', and that no job is claimed while
// paused; resuming lets it run again.
func TestWorkerPauseRequeuesRunning(t *testing.T) {
	db := testDB(t)
	w := testWorker(db)

	starts := make(chan string, 8)
	release := make(chan struct{})
	w.Register(state.JobReview, func(ctx context.Context, job *state.Job) error {
		starts <- job.ID
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-release:
			return nil
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Run(ctx) }()

	j := &state.Job{Type: state.JobReview}
	_ = db.EnqueueJob(ctx, j)

	select {
	case <-starts:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never started")
	}

	// Pause: the running job must return to the queue.
	if err := db.SetJobsPaused(ctx, true); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, db, j.ID, state.JobQueued)

	// While paused it must stay queued and not restart.
	select {
	case id := <-starts:
		t.Fatalf("job %s started while paused", id)
	case <-time.After(200 * time.Millisecond):
	}

	// Resume: it should be claimed and run again.
	if err := db.SetJobsPaused(ctx, false); err != nil {
		t.Fatal(err)
	}
	select {
	case <-starts:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not restart after resume")
	}
	close(release)
	waitStatus(t, db, j.ID, state.JobSuccess)
}

// TestWorkerStartsPausedFromDB proves the pause flag persisted in the DB is
// honored at worker startup (survives a restart).
func TestWorkerStartsPausedFromDB(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	if err := db.SetJobsPaused(ctx, true); err != nil {
		t.Fatal(err)
	}

	w := testWorker(db)
	starts := make(chan string, 1)
	w.Register(state.JobReview, func(ctx context.Context, job *state.Job) error {
		starts <- job.ID
		return nil
	})
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = w.Run(runCtx) }()

	j := &state.Job{Type: state.JobReview}
	_ = db.EnqueueJob(ctx, j)

	select {
	case id := <-starts:
		t.Fatalf("job %s ran despite the worker starting paused", id)
	case <-time.After(300 * time.Millisecond):
	}
}
