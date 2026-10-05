package repo_review

import (
	"context"
	"database/sql"
	"time"
)

// FailureStatsParams identifies one (MR, head SHA) pair and describes how to
// recognise an attempt that is still running.
type FailureStatsParams struct {
	ProjectID int64
	MrIid     int64
	HeadSha   string
	// InFlightMarker is the `error` text StartAttempt writes before the expensive
	// work begins. It is passed in rather than hard-coded in the SQL so the string
	// has exactly one definition (service.inFlightMarker).
	InFlightMarker string
	// InFlightSince bounds how long such a row may be believed: a row younger than
	// this is a live attempt, an older one is the crash the row exists to make
	// countable. Callers derive it from the review job's own timeout.
	InFlightSince time.Time
}

// FailureStats is the input to the backoff ladder of plan section 6.5.
type FailureStats struct {
	Failures int64
	// LastFailureAt is the zero time when Failures == 0.
	LastFailureAt time.Time
	// LastError is the `error` column of the most recent failed attempt, empty
	// when Failures == 0. Section 6.5 requires the warning logged once the ladder
	// gives up to carry it: an MR that has stopped costing money still has to say
	// why it is poisoned, or it becomes invisible instead of merely quiet.
	LastError string
	// InFlight reports a review of this head SHA that is running right now.
	//
	// It exists because StartAttempt records the attempt as status='failed' before
	// spending any money, so for the four to ten minutes a review takes, the row is
	// indistinguishable from a real failure — and scan_repo, which runs every five
	// minutes, kept logging "held back by the failure backoff" at WARN about
	// reviews that were about to succeed. Such a row is excluded from Failures:
	// throttling a merge request for something that has not gone wrong yet is
	// wrong twice over, in the count and in the log.
	InFlight bool
}

// countedFailures excludes the live attempt from both the count and the
// last-failure timestamp, so the ladder never measures its wait from a review
// that is still running.
const failureStats = `
WITH attempts AS (
    SELECT created_at, id, error,
           (error = $4 AND created_at > $5) AS in_flight
    FROM mr_reviews
    WHERE project_id = $1 AND mr_iid = $2 AND head_sha = $3 AND status = 'failed'
)
SELECT count(*) FILTER (WHERE NOT in_flight)::bigint AS failures,
       max(created_at) FILTER (WHERE NOT in_flight) AS last_failure_at,
       (SELECT error FROM attempts WHERE NOT in_flight
         ORDER BY created_at DESC, id DESC
         LIMIT 1) AS last_error,
       coalesce(bool_or(in_flight), false) AS in_flight
FROM attempts`

// FailureStats counts failed attempts for one head SHA and reports when the
// last one happened, which is what scan_repo throttles on: 1 failure -> wait
// 15m, 2 -> 1h, 3 -> 6h, >=4 -> do not enqueue at all. The WHERE clause matches
// the partial index mr_reviews_failed_idx. A new push changes head_sha, so the
// counter resets by itself.
//
// An attempt row that is still in flight (see FailureStats.InFlight) is reported
// separately and counted nowhere.
//
// Hand-written rather than generated because sqlc infers `max(created_at)` as
// non-nullable (it is NULL over zero rows) and the zero-failure case is exactly
// the one the caller cares about.
func (q *Queries) FailureStats(ctx context.Context, arg FailureStatsParams) (FailureStats, error) {
	var (
		out     FailureStats
		last    sql.NullTime
		lastErr sql.NullString
	)
	err := q.db.
		QueryRow(ctx, failureStats,
			arg.ProjectID, arg.MrIid, arg.HeadSha, arg.InFlightMarker, arg.InFlightSince).
		Scan(&out.Failures, &last, &lastErr, &out.InFlight)
	if err != nil {
		return FailureStats{}, err
	}
	out.LastFailureAt = last.Time
	out.LastError = lastErr.String
	return out, nil
}
