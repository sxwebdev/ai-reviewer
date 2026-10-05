package jobs

import (
	"errors"
	"time"

	"github.com/riverqueue/river"

	"github.com/sxwebdev/ai-reviewer/internal/metrics"
)

// River job states as metric label values. They match rivertype's own spelling
// so a dashboard can join river_jobs_total against the river_job table.
const (
	stateCompleted = "completed"
	stateFailed    = "failed"
	stateDiscarded = "discarded"
	stateCancelled = "cancelled"
	// stateScheduled is where a snoozed job lands: River sets it `scheduled`
	// for the snooze duration and rolls the attempt back, so it is neither a
	// failure nor a completion. Labelling it "failed" would make a deferred
	// review look like a broken one on every dashboard.
	stateScheduled = "scheduled"
)

// tracked wraps a worker body with the river_* metrics.
//
// It reads the outcome from the worker rather than from the database: River is
// the authority on a job's final state, but it settles that state after Work
// returns, and re-reading the row to label a counter would cost a query per job.
// The mapping is exact for the states that matter — a nil error is a
// completion, JobCancel and JobSnooze name their own state, and an error on the
// last permitted attempt is a discard because River will not schedule another.
func tracked[T river.JobArgs](job *river.Job[T], fn func() error) error {
	metrics.JobRetry(job.Kind, job.Attempt)

	start := time.Now()
	err := fn()

	metrics.ObserveJob(job.Kind, terminalState(job, err), time.Since(start))
	return err
}

// terminalState maps a worker's return value onto the state River is about to
// write. The two sentinel errors are checked before the attempt count because
// both override it: a cancelled job stops whatever remains, and a snoozed one
// gives its attempt back.
func terminalState[T river.JobArgs](job *river.Job[T], err error) string {
	var (
		snooze *river.JobSnoozeError
		cancel *river.JobCancelError
	)
	switch {
	case err == nil:
		return stateCompleted
	case errors.As(err, &snooze):
		return stateScheduled
	case errors.As(err, &cancel):
		return stateCancelled
	case job.Attempt >= job.MaxAttempts:
		return stateDiscarded
	default:
		return stateFailed
	}
}
