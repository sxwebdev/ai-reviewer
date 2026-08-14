// Package metrics declares every Prometheus collector the service exposes.
//
// mx's ops server mounts promhttp.Handler(), i.e. the *default* registry
// (verified in mx@v0.6.0 launcher/ops/metrics.go), so collectors are declared
// with promauto on that registry and nothing here builds its own. Declaration
// happens at package init: a duplicate metric name panics at startup rather
// than silently losing a series.
//
// Metric names are written out in full instead of being composed from a
// Namespace prefix — the plan (§14.3) fixes the exact strings, and they do not
// share one prefix (ai_reviewer_*, ai_review*_*, gitlab_*, merge_requests_*,
// slack_*, river_*). Composing them would rename them.
//
// Call sites use the helpers below rather than the raw handles wherever a
// single event updates more than one collector, so the pair cannot drift.
package metrics

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Label values. Every label in this package is a closed set defined here —
// team names are the only operator-supplied label, and they come from config,
// not from GitLab or Slack payloads, so cardinality stays bounded.
const (
	// Terminal outcome of a scan or a digest run.
	ResultOK      = "ok"
	ResultPartial = "partial" // some repositories failed, the rest completed (§35)
	ResultError   = "error"

	// ai_reviews_skipped_total reasons.
	SkipUpToDate = "up_to_date"
	SkipDraft    = "draft"
	SkipDisabled = "disabled"

	// slack_send_errors_total reasons.
	SlackErrRateLimited = "ratelimited"
	SlackErrAPI         = "api_error"
	SlackErrNetwork     = "network"

	// slack_user_match_total results.
	MatchMatched   = "matched"
	MatchNotFound  = "not_found"
	MatchAmbiguous = "ambiguous"
)

// statusTransport labels a GitLab failure that never produced an HTTP status
// (DNS, dial, TLS, timeout). Without it the status label would be "0".
const statusTransport = "transport"

var (
	// ScansTotal counts scan passes by terminal result.
	ScansTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ai_reviewer_scans_total",
		Help: "Scan passes by terminal result (ok|partial|error).",
	}, []string{"result"})

	// ScanDurationSeconds measures a full scan pass. Buckets run to ~8m so the
	// 2m scan timeout and its overruns land inside the range, not in +Inf.
	ScanDurationSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "ai_reviewer_scan_duration_seconds",
		Help:    "Duration of a scan pass.",
		Buckets: prometheus.ExponentialBuckets(1, 2, 10), // 1s … 512s
	})

	// ReviewsTotal counts completed AI reviews (the expensive path).
	ReviewsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ai_reviews_total",
		Help: "AI reviews that ran to completion.",
	}, []string{"team"})

	ReviewsFailedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ai_reviews_failed_total",
		Help: "AI reviews that failed, by reason.",
	}, []string{"team", "reason"})

	ReviewsSkippedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ai_reviews_skipped_total",
		Help: "Merge requests not reviewed, by reason (up_to_date|draft|disabled).",
	}, []string{"team", "reason"})

	// ReviewDurationSeconds measures one review end to end. Buckets reach ~85m
	// to cover the 30m job timeout with headroom.
	ReviewDurationSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "ai_review_duration_seconds",
		Help:    "Duration of one AI review, end to end.",
		Buckets: prometheus.ExponentialBuckets(5, 2, 10), // 5s … 2560s
	}, []string{"team"})

	// ReviewCostUSDTotal is the LLM spend per team — the metric that justifies
	// review.MaxAttempts = 1 and the poison-MR backoff (§6.5).
	ReviewCostUSDTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ai_review_cost_usd_total",
		Help: "Cumulative LLM cost in USD, as reported by the provider.",
	}, []string{"team"})

	// GitLabRequestsTotal is labelled by endpoint *template* (e.g.
	// "/projects/:key/merge_requests/:iid"), never by a concrete path: real ids
	// would make cardinality unbounded.
	GitLabRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gitlab_requests_total",
		Help: "GitLab API requests by endpoint template and HTTP method.",
	}, []string{"endpoint", "method"})

	GitLabRequestErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gitlab_request_errors_total",
		Help: "Failed GitLab API requests by endpoint template and HTTP status (or 'transport').",
	}, []string{"endpoint", "status"})

	MergeRequestsScannedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "merge_requests_scanned_total",
		Help: "Open merge requests inspected during scans.",
	}, []string{"team"})

	// The four gauges below are the digest's state, not events: they are
	// overwritten every scan pass (see SetTeamState) so a team that drops to
	// zero reports zero instead of keeping its last non-zero value forever.
	MergeRequestsWaitingHumanReview = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "merge_requests_waiting_human_review_total",
		Help: "Open merge requests where at least one reviewer still owes an action.",
	}, []string{"team"})

	MergeRequestsWithUnresolvedThreads = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "merge_requests_with_unresolved_threads_total",
		Help: "Open merge requests with unresolved resolvable threads.",
	}, []string{"team"})

	MergeRequestsWithConflicts = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "merge_requests_with_conflicts_total",
		Help: "Open merge requests with known merge conflicts.",
	}, []string{"team"})

	MergeRequestsWithFailedPipeline = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "merge_requests_with_failed_pipeline_total",
		Help: "Open merge requests whose head pipeline failed.",
	}, []string{"team"})

	DigestRunsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "slack_digest_runs_total",
		Help: "Digest runs by terminal result.",
	}, []string{"team", "result"})

	SlackMessagesSentTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "slack_messages_sent_total",
		Help: "Slack messages delivered (one per digest part).",
	}, []string{"team"})

	SlackSendErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "slack_send_errors_total",
		Help: "Slack delivery failures by reason (ratelimited|api_error|network).",
	}, []string{"team", "reason"})

	// SlackResendUncertainTotal counts the deliberate duplicate: the worker died
	// between chat.postMessage and recording the result, so the message is sent
	// again (§6.4). A non-zero value means someone may have seen it twice.
	SlackResendUncertainTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "slack_resend_uncertain_total",
		Help: "Digest parts resent after a crash between POST and result persistence.",
	}, []string{"team"})

	// SlackUserMatchTotal is how unmatched people become visible: the mapping is
	// deliberately not stored in Postgres (§5.2), so this counter and the log
	// are the whole operational surface.
	SlackUserMatchTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "slack_user_match_total",
		Help: "GitLab → Slack user resolutions by result (matched|not_found|ambiguous).",
	}, []string{"result"})

	RiverJobsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "river_jobs_total",
		Help: "River jobs by kind and terminal state.",
	}, []string{"kind", "state"})

	// RiverJobDurationSeconds spans both cheap dispatchers (scan, ~1s) and full
	// reviews (tens of minutes), hence the wide exponential range.
	RiverJobDurationSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "river_job_duration_seconds",
		Help:    "River job execution time by kind.",
		Buckets: prometheus.ExponentialBuckets(0.1, 3, 10), // 0.1s … ~2000s
	}, []string{"kind"})

	RiverJobRetriesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "river_job_retries_total",
		Help: "River job attempts beyond the first, by kind.",
	}, []string{"kind"})
)

// ObserveScan records one scan pass. Result and duration are always recorded
// together so a scan can never be counted without being timed.
func ObserveScan(result string, d time.Duration) {
	ScansTotal.WithLabelValues(result).Inc()
	ScanDurationSeconds.Observe(d.Seconds())
}

// ObserveReview records one completed review: count, duration and spend. Cost
// comes straight from the provider envelope; a zero cost is still recorded so
// the counter exists for the team from its first review.
func ObserveReview(team string, d time.Duration, costUSD float64) {
	ReviewsTotal.WithLabelValues(team).Inc()
	ReviewDurationSeconds.WithLabelValues(team).Observe(d.Seconds())
	ReviewCostUSDTotal.WithLabelValues(team).Add(costUSD)
}

// ReviewFailed records a failed review. reason must be a small closed set
// chosen by the caller (e.g. "clone", "llm", "validate", "persist").
func ReviewFailed(team, reason string) { ReviewsFailedTotal.WithLabelValues(team, reason).Inc() }

// ReviewSkipped records a merge request the scanner decided not to review.
func ReviewSkipped(team, reason string) { ReviewsSkippedTotal.WithLabelValues(team, reason).Inc() }

// MergeRequestsScanned adds n inspected merge requests for a team.
func MergeRequestsScanned(team string, n int) {
	MergeRequestsScannedTotal.WithLabelValues(team).Add(float64(n))
}

// GitLabRequest records one GitLab API call. endpoint must be the templated
// path, not the concrete one — see GitLabRequestsTotal.
func GitLabRequest(endpoint, method string) {
	GitLabRequestsTotal.WithLabelValues(endpoint, method).Inc()
}

// GitLabRequestError records a failed GitLab API call. Pass status 0 (or any
// non-positive value) when the request never reached a response.
func GitLabRequestError(endpoint string, status int) {
	GitLabRequestErrorsTotal.WithLabelValues(endpoint, statusLabel(status)).Inc()
}

// statusLabel keeps the status label a small closed set of HTTP codes plus one
// sentinel, instead of "0" for every transport failure.
func statusLabel(status int) string {
	if status <= 0 {
		return statusTransport
	}
	return strconv.Itoa(status)
}

// TeamState is the per-team MR classification a scan pass produced.
type TeamState struct {
	WaitingHumanReview int
	UnresolvedThreads  int
	Conflicts          int
	FailedPipeline     int
}

// SetTeamState publishes a team's MR classification. Call it once per scan pass
// per team, including when every count is zero: these are gauges, so a team
// that is skipped keeps reporting stale numbers until it is set again.
func SetTeamState(team string, s TeamState) {
	MergeRequestsWaitingHumanReview.WithLabelValues(team).Set(float64(s.WaitingHumanReview))
	MergeRequestsWithUnresolvedThreads.WithLabelValues(team).Set(float64(s.UnresolvedThreads))
	MergeRequestsWithConflicts.WithLabelValues(team).Set(float64(s.Conflicts))
	MergeRequestsWithFailedPipeline.WithLabelValues(team).Set(float64(s.FailedPipeline))
}

// DigestRun records the terminal result of one digest run.
func DigestRun(team, result string) { DigestRunsTotal.WithLabelValues(team, result).Inc() }

// SlackSent records one delivered digest part.
func SlackSent(team string) { SlackMessagesSentTotal.WithLabelValues(team).Inc() }

// SlackSendError records a delivery failure by reason.
func SlackSendError(team, reason string) { SlackSendErrorsTotal.WithLabelValues(team, reason).Inc() }

// SlackResendUncertain records a possible duplicate delivery (§6.4).
func SlackResendUncertain(team string) { SlackResendUncertainTotal.WithLabelValues(team).Inc() }

// SlackUserMatch records the outcome of one GitLab → Slack user resolution.
func SlackUserMatch(result string) { SlackUserMatchTotal.WithLabelValues(result).Inc() }

// ObserveJob records a River job's terminal state and how long it ran.
func ObserveJob(kind, state string, d time.Duration) {
	RiverJobsTotal.WithLabelValues(kind, state).Inc()
	RiverJobDurationSeconds.WithLabelValues(kind).Observe(d.Seconds())
}

// JobRetry records a retried attempt. Callers pass River's attempt number; the
// first attempt is not a retry.
func JobRetry(kind string, attempt int) {
	if attempt > 1 {
		RiverJobRetriesTotal.WithLabelValues(kind).Inc()
	}
}
