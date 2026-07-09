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
	"sync/atomic"
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

	paused atomic.Bool

	mu      sync.Mutex
	running map[string]context.CancelFunc // jobID -> cancel of the job's context
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
		running:  map[string]context.CancelFunc{},
	}
}

// Register wires a handler for a job type. Not safe to call after Run.
func (w *Worker) Register(jobType string, h Handler) { w.handlers[jobType] = h }

// Run starts the pool and blocks until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) error {
	if n, err := w.db.RecoverStuckJobs(ctx, 15*time.Minute); err == nil && n > 0 {
		w.log.Info("recovered stuck jobs", "count", n)
	}
	// Backstop for queued jobs left flagged for cancellation by an older build;
	// the runtime no longer produces them (every requeue path clears the flag).
	if n, err := w.db.FinalizeOrphanCancels(ctx); err == nil && n > 0 {
		w.log.Info("finalized orphaned cancellations", "count", n)
	}
	if paused, err := w.db.JobsPaused(ctx); err == nil {
		w.paused.Store(paused)
		if paused {
			w.log.Warn("job queue starts paused")
		}
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); w.controlLoop(ctx) }()
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

// controlLoop watches for out-of-band control signals — the global pause flag
// and per-job cancellation requests — both mediated through the DB so they work
// even when the worker and the web UI run in separate processes. The terminal
// outcome of a job is decided by run() from that same DB state, so this loop
// only ever cancels contexts; it never records outcomes itself.
func (w *Worker) controlLoop(ctx context.Context) {
	t := time.NewTicker(w.poll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if paused, err := w.db.JobsPaused(ctx); err == nil {
			w.paused.Store(paused)
		}
		// While paused, keep interrupting in-flight jobs every tick — not just on
		// the pause edge — so a job claimed in the race between loop()'s check and
		// this store is still stopped and returned to the queue.
		if w.paused.Load() {
			w.cancelAll()
		}
		w.applyCancels(ctx)
	}
}

// cancelAll cancels the context of every in-flight job.
func (w *Worker) cancelAll() {
	w.mu.Lock()
	for _, cancel := range w.running {
		cancel()
	}
	w.mu.Unlock()
}

// applyCancels stops any running job this worker owns that has been flagged for
// cancellation in the DB.
func (w *Worker) applyCancels(ctx context.Context) {
	w.mu.Lock()
	ids := make([]string, 0, len(w.running))
	for id := range w.running {
		ids = append(ids, id)
	}
	w.mu.Unlock()
	if len(ids) == 0 {
		return
	}
	flagged, err := w.db.CancelRequestedIDs(ctx, ids)
	if err != nil {
		w.log.Warn("poll cancel requests failed", "err", err)
		return
	}
	w.mu.Lock()
	for _, id := range flagged {
		if cancel := w.running[id]; cancel != nil {
			cancel()
		}
	}
	w.mu.Unlock()
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
		if w.paused.Load() {
			timer.Reset(w.poll) // queue paused; don't claim
			continue
		}
		job, err := w.db.ClaimJob(ctx, workerID)
		switch {
		case err == state.ErrNotFound:
			timer.Reset(w.poll) // nothing to do; back off
		case err != nil:
			w.log.Error("claim job failed", "err", err)
			timer.Reset(w.poll)
		default:
			// Pause may have flipped between the check above and the claim; hand the
			// job straight back rather than running it while paused.
			if w.paused.Load() {
				_ = w.db.RequeueRunningJob(ctx, job.ID)
				timer.Reset(w.poll)
				continue
			}
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

	// Per-job cancellation: the control loop can cancel jobCtx to stop just this
	// job. The context propagates down to the claude subprocess (exec.CommandContext),
	// so cancelling actually aborts the review.
	jobCtx, cancel := context.WithCancel(ctx)
	w.mu.Lock()
	w.running[job.ID] = cancel
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		delete(w.running, job.ID)
		w.mu.Unlock()
		cancel()
	}()

	w.log.Info("job started", "id", job.ID, "type", job.Type, "attempt", job.Attempts)

	// A panicking handler must not kill the worker goroutine (and thus a pool
	// slot forever) — recover it into an error so the job retries/fails cleanly.
	err := safeRun(jobCtx, h, job)

	// Decide the terminal state from the DB, not from err: a review that was
	// stopped after one of its passes already succeeded returns a nil error (the
	// engine only errors when every pass fails), so err alone can't tell a genuine
	// completion from an interrupted one. cancel_requested and the pause flag are
	// the authoritative intent.
	flagged, ferr := w.db.CancelRequestedIDs(finalCtx, []string{job.ID})
	switch {
	case ferr == nil && len(flagged) > 0:
		// Explicit stop wins even over a just-completed handler.
		_ = w.db.MarkJobCancelled(finalCtx, job.ID)
		w.log.Info("job cancelled", "id", job.ID, "type", job.Type)
	case w.paused.Load() && jobCtx.Err() != nil:
		// Interrupted by a global pause -> return to the queue to run on resume.
		if rerr := w.db.RequeueRunningJob(finalCtx, job.ID); rerr != nil {
			w.log.Warn("requeue on pause failed", "id", job.ID, "err", rerr)
		} else {
			w.log.Info("job requeued (paused)", "id", job.ID, "type", job.Type)
		}
	case err == nil:
		_ = w.db.CompleteJob(finalCtx, job.ID, state.JobSuccess, "")
		w.log.Info("job done", "id", job.ID, "type", job.Type)
	default:
		msg := security.Mask(err.Error())
		_ = w.db.AppendJobLog(finalCtx, job.ID, "error", msg)
		backoff := time.Duration(job.Attempts) * 30 * time.Second
		retried, _ := w.db.RetryJob(finalCtx, job, msg, backoff)
		w.log.Warn("job failed", "id", job.ID, "type", job.Type, "retried", retried, "err", msg)
	}
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
