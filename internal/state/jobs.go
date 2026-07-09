package state

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// EnqueueJob inserts a queued job. It fills id, timestamps, and run_after.
func (db *DB) EnqueueJob(ctx context.Context, j *Job) error {
	if j.ID == "" {
		j.ID = uuid.NewString()
	}
	now := nowMillis()
	j.CreatedAt = now
	j.UpdatedAt = now
	if j.Status == "" {
		j.Status = JobQueued
	}
	if j.RunAfter == 0 {
		j.RunAfter = now
	}
	if j.MaxAttempts == 0 {
		j.MaxAttempts = 3
	}
	_, err := db.ExecContext(ctx,
		`INSERT INTO jobs (id, type, status, payload_json, project_id, mr_iid, review_id,
			priority, attempts, max_attempts, run_after, progress_current, progress_total,
			created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, 0, 0, ?, ?)`,
		j.ID, j.Type, j.Status, j.PayloadJSON, j.ProjectID, j.MRIID, j.ReviewID,
		j.Priority, j.MaxAttempts, j.RunAfter, j.CreatedAt, j.UpdatedAt)
	return err
}

const jobColumns = `id, type, status, payload_json, project_id, mr_iid, review_id, priority,
	attempts, max_attempts, run_after, locked_at, locked_by, error, cancel_requested,
	progress_current, progress_total, created_at, started_at, finished_at, updated_at`

func scanJob(s interface{ Scan(...any) error }) (*Job, error) {
	j := &Job{}
	err := s.Scan(&j.ID, &j.Type, &j.Status, &j.PayloadJSON, &j.ProjectID, &j.MRIID, &j.ReviewID,
		&j.Priority, &j.Attempts, &j.MaxAttempts, &j.RunAfter, &j.LockedAt, &j.LockedBy, &j.Error,
		&j.CancelRequested, &j.ProgressCurrent, &j.ProgressTotal, &j.CreatedAt, &j.StartedAt,
		&j.FinishedAt, &j.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return j, nil
}

// ClaimJob atomically claims the next runnable queued job. Because the pool is
// capped at one connection, the SELECT+UPDATE is effectively serialized; the
// IMMEDIATE transaction guards against races if that ever changes. Returns
// ErrNotFound when nothing is runnable.
func (db *DB) ClaimJob(ctx context.Context, workerID string) (*Job, error) {
	now := nowMillis()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck

	j, err := scanJob(tx.QueryRowContext(ctx,
		`SELECT `+jobColumns+` FROM jobs
		 WHERE status = ? AND run_after <= ? AND cancel_requested = 0
		 ORDER BY priority DESC, run_after ASC, id ASC LIMIT 1`, JobQueued, now))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE jobs SET status = ?, locked_at = ?, locked_by = ?, started_at = COALESCE(started_at, ?),
			attempts = attempts + 1, updated_at = ? WHERE id = ?`,
		JobRunning, now, workerID, now, now, j.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	j.Status = JobRunning
	j.Attempts++
	return j, nil
}

// CompleteJob marks a job finished with the given terminal status.
func (db *DB) CompleteJob(ctx context.Context, id, status, errMsg string) error {
	now := nowMillis()
	_, err := db.ExecContext(ctx,
		`UPDATE jobs SET status = ?, error = ?, finished_at = ?, locked_by = '', updated_at = ? WHERE id = ?`,
		status, errMsg, now, now, id)
	return err
}

// RetryJob requeues a job with a delay, or marks it failed if attempts are
// exhausted. Returns true if the job will be retried.
func (db *DB) RetryJob(ctx context.Context, j *Job, errMsg string, backoff time.Duration) (bool, error) {
	if j.Attempts >= j.MaxAttempts {
		return false, db.CompleteJob(ctx, j.ID, JobFailed, errMsg)
	}
	now := nowMillis()
	// Clear cancel_requested: a job flagged for stop that still reaches the retry
	// path (a genuine error raced the stop signal) must not requeue while flagged,
	// or ClaimJob's `cancel_requested = 0` guard would skip it forever.
	_, err := db.ExecContext(ctx,
		`UPDATE jobs SET status = ?, error = ?, run_after = ?, locked_by = '', cancel_requested = 0, updated_at = ? WHERE id = ?`,
		JobQueued, errMsg, now+backoff.Milliseconds(), now, j.ID)
	return err == nil, err
}

// RequeueJob resets a finished (failed/cancelled) job back to queued so the
// worker runs it again with a fresh attempt budget. Returns true if a job was
// requeued (i.e. it existed and was in a terminal-but-retryable state).
func (db *DB) RequeueJob(ctx context.Context, id string) (bool, error) {
	now := nowMillis()
	res, err := db.ExecContext(ctx,
		`UPDATE jobs SET status = ?, attempts = 0, error = '', locked_by = '', locked_at = NULL,
			cancel_requested = 0, run_after = ?, finished_at = NULL, updated_at = ?
		 WHERE id = ? AND status IN (?, ?)`,
		JobQueued, now, now, id, JobFailed, JobCancelled)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// settingJobsPaused is the settings key holding the global queue-pause flag.
const settingJobsPaused = "jobs.paused"

// JobsPaused reports whether the queue is globally paused.
func (db *DB) JobsPaused(ctx context.Context) (bool, error) {
	v, ok, err := db.GetSetting(ctx, settingJobsPaused)
	if err != nil {
		return false, err
	}
	return ok && v == "true", nil
}

// SetJobsPaused persists the global queue-pause flag. The worker observes it in
// its control loop: it stops claiming new jobs and requeues any it is running.
func (db *DB) SetJobsPaused(ctx context.Context, paused bool) error {
	v := "false"
	if paused {
		v = "true"
	}
	return db.SetSetting(ctx, settingJobsPaused, v)
}

// Cancel outcomes returned by RequestCancelJob.
const (
	CancelDone    = "cancelled"  // job was queued and is now terminally cancelled
	CancelPending = "cancelling" // job is running; the worker will stop it shortly
	CancelNoop    = ""           // job was not in an active (queued/running) state
)

// RequestCancelJob stops a job. A queued job is cancelled synchronously (the
// worker never claims it). A running job is flagged cancel_requested=1; the
// worker owning it observes the flag, cancels its context, and writes the
// terminal 'cancelled' status. Returns the outcome for user feedback.
func (db *DB) RequestCancelJob(ctx context.Context, id string) (string, error) {
	now := nowMillis()
	res, err := db.ExecContext(ctx,
		`UPDATE jobs SET status = ?, finished_at = ?, locked_by = '', updated_at = ?
		 WHERE id = ? AND status = ?`,
		JobCancelled, now, now, id, JobQueued)
	if err != nil {
		return CancelNoop, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return CancelDone, nil
	}
	res, err = db.ExecContext(ctx,
		`UPDATE jobs SET cancel_requested = 1, updated_at = ? WHERE id = ? AND status = ?`,
		now, id, JobRunning)
	if err != nil {
		return CancelNoop, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return CancelPending, nil
	}
	return CancelNoop, nil
}

// RequeueRunningJob returns a running job to the queue (used when the queue is
// paused). It undoes the attempt increment ClaimJob made so a pause/resume cycle
// never exhausts the retry budget, and clears any cancel flag.
func (db *DB) RequeueRunningJob(ctx context.Context, id string) error {
	now := nowMillis()
	_, err := db.ExecContext(ctx,
		`UPDATE jobs SET status = ?, locked_by = '', locked_at = NULL, cancel_requested = 0,
			attempts = MAX(0, attempts - 1), run_after = ?, updated_at = ?
		 WHERE id = ? AND status = ?`,
		JobQueued, now, now, id, JobRunning)
	return err
}

// MarkJobCancelled writes the terminal cancelled status and clears the cancel
// flag. Used by the worker after it has stopped a running job. Guarded on the
// running status so it can never clobber a job that was concurrently requeued.
func (db *DB) MarkJobCancelled(ctx context.Context, id string) error {
	now := nowMillis()
	_, err := db.ExecContext(ctx,
		`UPDATE jobs SET status = ?, cancel_requested = 0, finished_at = ?, locked_by = ?, updated_at = ?
		 WHERE id = ? AND status = ?`,
		JobCancelled, now, "", now, id, JobRunning)
	return err
}

// CancelRequestedIDs returns the subset of the given job ids that are flagged
// for cancellation. The worker calls this for the jobs it currently runs.
func (db *DB) CancelRequestedIDs(ctx context.Context, ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	q := `SELECT id FROM jobs WHERE cancel_requested = 1 AND id IN (` +
		strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + `)`
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// FinalizeOrphanCancels cancels queued jobs still flagged cancel_requested=1 —
// orphans left when a worker died mid-stop and RecoverStuckJobs requeued them.
// Returns the number finalized.
func (db *DB) FinalizeOrphanCancels(ctx context.Context) (int64, error) {
	now := nowMillis()
	res, err := db.ExecContext(ctx,
		`UPDATE jobs SET status = ?, cancel_requested = 0, finished_at = ?, locked_by = '', updated_at = ?
		 WHERE status = ? AND cancel_requested = 1`,
		JobCancelled, now, now, JobQueued)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RecoverStuckJobs reconciles jobs stuck in 'running' longer than maxAge (e.g.
// their owning worker crashed). A stuck job that was flagged for cancellation is
// finalized as cancelled — honoring the user's stop and preventing a zombie that
// blocks its MR — while the rest are requeued. Returns the number reconciled.
func (db *DB) RecoverStuckJobs(ctx context.Context, maxAge time.Duration) (int64, error) {
	cutoff := nowMillis() - maxAge.Milliseconds()
	now := nowMillis()
	cancelled, err := db.ExecContext(ctx,
		`UPDATE jobs SET status = ?, cancel_requested = 0, finished_at = ?, locked_by = '', updated_at = ?
		 WHERE status = ? AND locked_at IS NOT NULL AND locked_at < ? AND cancel_requested = 1`,
		JobCancelled, now, now, JobRunning, cutoff)
	if err != nil {
		return 0, err
	}
	requeued, err := db.ExecContext(ctx,
		`UPDATE jobs SET status = ?, locked_by = '', updated_at = ?
		 WHERE status = ? AND locked_at IS NOT NULL AND locked_at < ? AND cancel_requested = 0`,
		JobQueued, now, JobRunning, cutoff)
	if err != nil {
		return 0, err
	}
	nc, _ := cancelled.RowsAffected()
	nq, _ := requeued.RowsAffected()
	return nc + nq, nil
}

// ListJobs returns recent jobs, newest first.
func (db *DB) ListJobs(ctx context.Context, limit int) ([]*Job, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := db.QueryContext(ctx,
		`SELECT `+jobColumns+` FROM jobs ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// GetLatestReviewJob returns the most recent review job for an MR (ErrNotFound
// if none), correct regardless of how many other jobs exist.
func (db *DB) GetLatestReviewJob(ctx context.Context, projectID, iid int64) (*Job, error) {
	j, err := scanJob(db.QueryRowContext(ctx,
		`SELECT `+jobColumns+` FROM jobs
		 WHERE type = ? AND project_id = ? AND mr_iid = ?
		 ORDER BY created_at DESC, id DESC LIMIT 1`, JobReview, projectID, iid))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return j, err
}

// LatestReviewJobsPerMR returns the newest review job for each MR (one row per
// project_id + mr_iid), so dashboard status is correct no matter the job volume.
func (db *DB) LatestReviewJobsPerMR(ctx context.Context) ([]*Job, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT `+jobColumns+` FROM (
		   SELECT *, ROW_NUMBER() OVER (PARTITION BY project_id, mr_iid ORDER BY created_at DESC, id DESC) AS rn
		   FROM jobs WHERE type = ? AND project_id IS NOT NULL AND mr_iid IS NOT NULL
		 ) WHERE rn = 1`, JobReview)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// HasActiveJob reports whether a queued/running job of the given type exists
// for a project+MR, used to avoid enqueuing duplicate reviews.
func (db *DB) HasActiveJob(ctx context.Context, jobType string, projectID, mrIID int64) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM jobs
		 WHERE type = ? AND status IN (?, ?) AND project_id = ? AND mr_iid = ?`,
		jobType, JobQueued, JobRunning, projectID, mrIID).Scan(&n)
	return n > 0, err
}

// AppendJobLog records a redacted log line for a job.
func (db *DB) AppendJobLog(ctx context.Context, jobID, level, message string) error {
	_, err := db.ExecContext(ctx,
		`INSERT INTO job_logs (job_id, level, message, created_at) VALUES (?, ?, ?, ?)`,
		jobID, level, message, nowMillis())
	return err
}
