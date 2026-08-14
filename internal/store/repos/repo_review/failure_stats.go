package repo_review

import (
	"context"
	"database/sql"
	"time"
)

// FailureStatsParams identifies one (MR, head SHA) pair.
type FailureStatsParams struct {
	ProjectID int64
	MrIid     int64
	HeadSha   string
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
}

const failureStats = `
SELECT count(*)::bigint AS failures,
       max(created_at) AS last_failure_at,
       (SELECT error FROM mr_reviews
         WHERE project_id = $1 AND mr_iid = $2 AND head_sha = $3 AND status = 'failed'
         ORDER BY created_at DESC, id DESC
         LIMIT 1) AS last_error
FROM mr_reviews
WHERE project_id = $1 AND mr_iid = $2 AND head_sha = $3 AND status = 'failed'`

// FailureStats counts failed attempts for one head SHA and reports when the
// last one happened, which is what scan_repo throttles on: 1 failure -> wait
// 15m, 2 -> 1h, 3 -> 6h, >=4 -> do not enqueue at all. The WHERE clause matches
// the partial index mr_reviews_failed_idx. A new push changes head_sha, so the
// counter resets by itself.
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
		QueryRow(ctx, failureStats, arg.ProjectID, arg.MrIid, arg.HeadSha).
		Scan(&out.Failures, &last, &lastErr)
	if err != nil {
		return FailureStats{}, err
	}
	out.LastFailureAt = last.Time
	out.LastError = lastErr.String
	return out, nil
}
