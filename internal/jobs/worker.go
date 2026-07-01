// Package jobs is the durable background worker: the SQLite `jobs` table is the
// source of truth, a bounded pool of goroutines claims and runs jobs, and a
// scheduler enqueues review jobs when an MR's head sha changes.
package jobs

import (
	"context"
	"fmt"
	"runtime/debug"
	"strconv"
	"sync"
	"time"

	"log/slog"

	"github.com/sxwebdev/ai-reviewer/internal/security"
	"github.com/sxwebdev/ai-reviewer/internal/state"
)

// Handler runs a single job. A returned error triggers retry/backoff.
type Handler func(ctx context.Context, job *state.Job) error

// Worker drains the jobs table with a bounded pool.
type Worker struct {
	db       *state.DB
	handlers map[string]Handler
	n        int
	poll     time.Duration
	log      *slog.Logger
}

// NewWorker builds a worker with n concurrent slots (min 1).
func NewWorker(db *state.DB, n int, log *slog.Logger) *Worker {
	if n < 1 {
		n = 1
	}
	return &Worker{
		db:       db,
		handlers: map[string]Handler{},
		n:        n,
		poll:     2 * time.Second,
		log:      log,
	}
}

// Register wires a handler for a job type. Not safe to call after Run.
func (w *Worker) Register(jobType string, h Handler) { w.handlers[jobType] = h }

// Run starts the pool and blocks until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) error {
	if n, err := w.db.RecoverStuckJobs(ctx, 15*time.Minute); err == nil && n > 0 {
		w.log.Info("recovered stuck jobs", "count", n)
	}
	var wg sync.WaitGroup
	for i := 0; i < w.n; i++ {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			w.loop(ctx, id)
		}("worker-" + strconv.Itoa(i))
	}
	wg.Wait()
	return nil
}

func (w *Worker) loop(ctx context.Context, workerID string) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		job, err := w.db.ClaimJob(ctx, workerID)
		switch {
		case err == state.ErrNotFound:
			timer.Reset(w.poll) // nothing to do; back off
		case err != nil:
			w.log.Error("claim job failed", "err", err)
			timer.Reset(w.poll)
		default:
			w.run(ctx, job)
			timer.Reset(0) // immediately look for more
		}
	}
}

func (w *Worker) run(ctx context.Context, job *state.Job) {
	// Terminal DB writes must survive shutdown: if ctx is cancelled mid-job, a
	// write on it would fail and leave the job stuck 'running' until the next
	// RecoverStuckJobs sweep. Use a cancellation-detached context for them.
	finalCtx := context.WithoutCancel(ctx)

	h := w.handlers[job.Type]
	if h == nil {
		_ = w.db.CompleteJob(finalCtx, job.ID, state.JobFailed, "no handler for job type "+job.Type)
		return
	}
	w.log.Info("job started", "id", job.ID, "type", job.Type, "attempt", job.Attempts)

	// A panicking handler must not kill the worker goroutine (and thus a pool
	// slot forever) — recover it into an error so the job retries/fails cleanly.
	err := safeRun(ctx, h, job)
	if err == nil {
		_ = w.db.CompleteJob(finalCtx, job.ID, state.JobSuccess, "")
		w.log.Info("job done", "id", job.ID, "type", job.Type)
		return
	}
	msg := security.Mask(err.Error())
	_ = w.db.AppendJobLog(finalCtx, job.ID, "error", msg)
	backoff := time.Duration(job.Attempts) * 30 * time.Second
	retried, _ := w.db.RetryJob(finalCtx, job, msg, backoff)
	w.log.Warn("job failed", "id", job.ID, "type", job.Type, "retried", retried, "err", msg)
}

// safeRun invokes a handler, converting a panic into an error so the worker
// goroutine survives.
func safeRun(ctx context.Context, h Handler, job *state.Job) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic in job handler: %v\n%s", r, debug.Stack())
		}
	}()
	return h(ctx, job)
}
