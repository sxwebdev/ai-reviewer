package metrics

import (
	"errors"
	"fmt"
	"maps"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// allNames is the exact list from plan §14.3. The strings are duplicated here
// on purpose: a rename in metrics.go must break this test, because dashboards
// and alerts outside this repository key off these names.
var allNames = []string{
	"ai_reviewer_scans_total",
	"ai_reviewer_scan_duration_seconds",
	"ai_reviews_total",
	"ai_reviews_failed_total",
	"ai_reviews_skipped_total",
	"ai_review_duration_seconds",
	"ai_review_cost_usd_total",
	"ai_review_findings_suppressed_total",
	"gitlab_requests_total",
	"gitlab_request_errors_total",
	"ai_reviewer_linear_requests_total",
	"ai_reviewer_linear_request_duration_seconds",
	"ai_reviewer_linear_issues_in_review",
	"ai_reviewer_digest_source_errors_total",
	"merge_requests_scanned_total",
	"merge_requests_waiting_human_review_total",
	"merge_requests_with_changes_requested_total",
	"merge_requests_with_unresolved_threads_total",
	"merge_requests_with_conflicts_total",
	"merge_requests_with_failed_pipeline_total",
	"slack_digest_runs_total",
	"slack_messages_sent_total",
	"slack_send_errors_total",
	"slack_resend_uncertain_total",
	"slack_user_match_total",
	"river_jobs_total",
	"river_job_duration_seconds",
	"river_job_retries_total",
}

// vecCase pins one collector's label set. get returns the error the *Vec
// reports when the supplied labels do not match its declared label names, which
// is how cardinality is asserted without reaching for the protobuf model.
type vecCase struct {
	name   string
	labels prometheus.Labels
	get    func(prometheus.Labels) error
}

func vecCases() []vecCase {
	return []vecCase{
		{"ai_reviewer_scans_total", prometheus.Labels{"result": ResultOK},
			func(l prometheus.Labels) error { _, err := ScansTotal.GetMetricWith(l); return err }},
		{"ai_reviews_total", prometheus.Labels{"team": "payments"},
			func(l prometheus.Labels) error { _, err := ReviewsTotal.GetMetricWith(l); return err }},
		{"ai_reviews_failed_total", prometheus.Labels{"team": "payments", "reason": "llm"},
			func(l prometheus.Labels) error { _, err := ReviewsFailedTotal.GetMetricWith(l); return err }},
		{"ai_reviews_skipped_total", prometheus.Labels{"team": "payments", "reason": SkipUpToDate},
			func(l prometheus.Labels) error { _, err := ReviewsSkippedTotal.GetMetricWith(l); return err }},
		{"ai_review_duration_seconds", prometheus.Labels{"team": "payments"},
			func(l prometheus.Labels) error { _, err := ReviewDurationSeconds.GetMetricWith(l); return err }},
		{"ai_review_cost_usd_total", prometheus.Labels{"team": "payments"},
			func(l prometheus.Labels) error { _, err := ReviewCostUSDTotal.GetMetricWith(l); return err }},
		{"ai_review_findings_suppressed_total", prometheus.Labels{"team": "payments", "stage": "not_in_diff"},
			func(l prometheus.Labels) error {
				_, err := ReviewFindingsSuppressedTotal.GetMetricWith(l)
				return err
			}},
		{"gitlab_requests_total", prometheus.Labels{"endpoint": "/projects/:key", "method": "GET"},
			func(l prometheus.Labels) error { _, err := GitLabRequestsTotal.GetMetricWith(l); return err }},
		{"gitlab_request_errors_total", prometheus.Labels{"endpoint": "/projects/:key", "status": "500"},
			func(l prometheus.Labels) error { _, err := GitLabRequestErrorsTotal.GetMetricWith(l); return err }},
		{"ai_reviewer_linear_requests_total", prometheus.Labels{"operation": "viewer", "result": ResultOK},
			func(l prometheus.Labels) error { _, err := LinearRequestsTotal.GetMetricWith(l); return err }},
		{"ai_reviewer_linear_request_duration_seconds", prometheus.Labels{"operation": "viewer"},
			func(l prometheus.Labels) error { _, err := LinearRequestDurationSeconds.GetMetricWith(l); return err }},
		{"ai_reviewer_linear_issues_in_review", prometheus.Labels{"team": "payments"},
			func(l prometheus.Labels) error { _, err := LinearIssuesInReview.GetMetricWith(l); return err }},
		{"ai_reviewer_digest_source_errors_total", prometheus.Labels{"team": "payments", "source": SourceLinear},
			func(l prometheus.Labels) error { _, err := DigestSourceErrorsTotal.GetMetricWith(l); return err }},
		{"merge_requests_scanned_total", prometheus.Labels{"team": "payments"},
			func(l prometheus.Labels) error { _, err := MergeRequestsScannedTotal.GetMetricWith(l); return err }},
		{"merge_requests_waiting_human_review_total", prometheus.Labels{"team": "payments"},
			func(l prometheus.Labels) error {
				_, err := MergeRequestsWaitingHumanReview.GetMetricWith(l)
				return err
			}},
		{"merge_requests_with_changes_requested_total", prometheus.Labels{"team": "payments"},
			func(l prometheus.Labels) error {
				_, err := MergeRequestsWithChangesRequested.GetMetricWith(l)
				return err
			}},
		{"merge_requests_with_unresolved_threads_total", prometheus.Labels{"team": "payments"},
			func(l prometheus.Labels) error {
				_, err := MergeRequestsWithUnresolvedThreads.GetMetricWith(l)
				return err
			}},
		{"merge_requests_with_conflicts_total", prometheus.Labels{"team": "payments"},
			func(l prometheus.Labels) error { _, err := MergeRequestsWithConflicts.GetMetricWith(l); return err }},
		{"merge_requests_with_failed_pipeline_total", prometheus.Labels{"team": "payments"},
			func(l prometheus.Labels) error {
				_, err := MergeRequestsWithFailedPipeline.GetMetricWith(l)
				return err
			}},
		{"slack_digest_runs_total", prometheus.Labels{"team": "payments", "result": ResultOK},
			func(l prometheus.Labels) error { _, err := DigestRunsTotal.GetMetricWith(l); return err }},
		{"slack_messages_sent_total", prometheus.Labels{"team": "payments"},
			func(l prometheus.Labels) error { _, err := SlackMessagesSentTotal.GetMetricWith(l); return err }},
		{"slack_send_errors_total", prometheus.Labels{"team": "payments", "reason": SlackErrRateLimited},
			func(l prometheus.Labels) error { _, err := SlackSendErrorsTotal.GetMetricWith(l); return err }},
		{"slack_resend_uncertain_total", prometheus.Labels{"team": "payments"},
			func(l prometheus.Labels) error { _, err := SlackResendUncertainTotal.GetMetricWith(l); return err }},
		{"slack_user_match_total", prometheus.Labels{"result": MatchMatched},
			func(l prometheus.Labels) error { _, err := SlackUserMatchTotal.GetMetricWith(l); return err }},
		{"river_jobs_total", prometheus.Labels{"kind": "review", "state": "completed"},
			func(l prometheus.Labels) error { _, err := RiverJobsTotal.GetMetricWith(l); return err }},
		{"river_job_duration_seconds", prometheus.Labels{"kind": "review"},
			func(l prometheus.Labels) error { _, err := RiverJobDurationSeconds.GetMetricWith(l); return err }},
		{"river_job_retries_total", prometheus.Labels{"kind": "review"},
			func(l prometheus.Labels) error { _, err := RiverJobRetriesTotal.GetMetricWith(l); return err }},
	}
}

// TestCollectorsExposedOnDefaultRegistry is the duplicate-registration guard:
// promauto registers at package init, so a colliding name would panic before
// any test body runs. Reaching the assertions therefore already proves the set
// is conflict-free; the gather then proves every name is actually exposed on
// the registry mx serves (promhttp.Handler → prometheus.DefaultGatherer).
func TestCollectorsExposedOnDefaultRegistry(t *testing.T) {
	// Vecs only materialise a series once a label set is used, so touch them all.
	for _, c := range vecCases() {
		if err := c.get(c.labels); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
	}

	for _, name := range allNames {
		n, err := testutil.GatherAndCount(prometheus.DefaultGatherer, name)
		if err != nil {
			t.Fatalf("gather %s: %v", name, err)
		}
		if n == 0 {
			t.Errorf("metric %q is not exposed on the default registry", name)
		}
	}
}

// TestNamesAreTakenOnDefaultRegistry proves the collectors live on the default
// registry specifically — the one mx serves — by showing the registry refuses
// the name a second time. Registering an identical descriptor yields
// AlreadyRegisteredError; a colliding one with different help/labels (what a
// careless second declaration elsewhere in the tree would look like) yields a
// plain error. Both are rejections, and both are asserted.
func TestNamesAreTakenOnDefaultRegistry(t *testing.T) {
	identical := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "ai_reviews_total",
		Help: "AI reviews that ran to completion.",
	}, []string{"team"})
	var already prometheus.AlreadyRegisteredError
	if err := prometheus.Register(identical); !errors.As(err, &already) {
		t.Errorf("re-registering an identical ai_reviews_total returned %v, want AlreadyRegisteredError", err)
	}

	colliding := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "ai_reviews_total",
		Help: "a second, conflicting declaration",
	}, []string{"team", "extra"})
	if err := prometheus.Register(colliding); err == nil {
		t.Error("a conflicting ai_reviews_total was accepted; the name is not held on the default registry")
	}
}

// TestLabelCardinality pins each collector's label names. GetMetricWith reports
// an error when the supplied label names do not match the declared ones, so an
// added or renamed label fails here rather than silently producing a new series.
func TestLabelCardinality(t *testing.T) {
	for _, c := range vecCases() {
		t.Run(c.name, func(t *testing.T) {
			if err := c.get(c.labels); err != nil {
				t.Fatalf("declared labels %v rejected: %v", c.labels, err)
			}

			extra := prometheus.Labels{"unexpected_label": "x"}
			maps.Copy(extra, c.labels)
			if err := c.get(extra); err == nil {
				t.Errorf("an extra label was accepted; declared label set is wider than expected")
			}

			if len(c.labels) > 0 {
				missing := prometheus.Labels{}
				first := true
				for k, v := range c.labels {
					if first { // drop one declared label
						first = false
						continue
					}
					missing[k] = v
				}
				if err := c.get(missing); err == nil {
					t.Errorf("a missing label was accepted; declared label set is narrower than expected")
				}
			}
		})
	}
}

// TestScanDurationIsUnlabelledHistogram guards the one collector the plan marks
// as a histogram with no labels: it must always report exactly one series.
func TestScanDurationIsUnlabelledHistogram(t *testing.T) {
	if got := testutil.CollectAndCount(ScanDurationSeconds, "ai_reviewer_scan_duration_seconds"); got != 1 {
		t.Errorf("series count = %d, want exactly 1 (an unlabelled histogram)", got)
	}
}

// runSeq makes a label value unique per *run*, not just per test.
//
// These collectors are promauto-registered on the process-global default
// registry and have no Reset: a second execution of the same test function in
// the same binary — which is exactly what `go test -count=2` does, the standard
// way to smoke out flakes — sees the first run's counts. Either the assertion
// measures a delta, or the label value it measures is new. Both appear below:
// a delta where the label itself is the thing under test, a fresh label where
// "this created exactly one new series" is.
var runSeq atomic.Int64

func uniqueLabel(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, runSeq.Add(1))
}

// TestHelpersUpdateEveryCollector covers the helpers that touch more than one
// collector — the reason they exist is that a call site must not be able to
// update one and forget the other.
func TestHelpersUpdateEveryCollector(t *testing.T) {
	// Unique so neither another test nor a repeat run of this one can perturb it.
	team := uniqueLabel("helpers-test-team")

	t.Run("ObserveScan", func(t *testing.T) {
		before := testutil.ToFloat64(ScansTotal.WithLabelValues(ResultPartial))
		ObserveScan(ResultPartial, 3*time.Second)
		if got := testutil.ToFloat64(ScansTotal.WithLabelValues(ResultPartial)); got != before+1 {
			t.Errorf("scans_total = %v, want %v", got, before+1)
		}
	})

	t.Run("ObserveReview", func(t *testing.T) {
		// A histogram has no ToFloat64 equivalent, so the duration leg is checked
		// by series count: observing a team that has never been seen must create
		// exactly one new series. (Currying does not narrow Collect — a curried
		// vec still emits every child — so the delta is measured on the whole vec.)
		before := testutil.CollectAndCount(ReviewDurationSeconds)

		ObserveReview(team, 90*time.Second, 0.42)

		if got := testutil.ToFloat64(ReviewsTotal.WithLabelValues(team)); got != 1 {
			t.Errorf("ai_reviews_total = %v, want 1", got)
		}
		if got := testutil.ToFloat64(ReviewCostUSDTotal.WithLabelValues(team)); got != 0.42 {
			t.Errorf("ai_review_cost_usd_total = %v, want 0.42", got)
		}
		if got := testutil.CollectAndCount(ReviewDurationSeconds); got != before+1 {
			t.Errorf("ai_review_duration_seconds series = %d, want %d (one new team)", got, before+1)
		}
	})

	t.Run("SetTeamState overwrites, does not accumulate", func(t *testing.T) {
		// Every field SetTeamState owns is set, so a gauge added to TeamState and
		// forgotten here is a failing test rather than a series that never appears.
		// LinearNotReady is deliberately absent: it is the one field that can be
		// *unknown* rather than zero, so it is published by SetLinearNotReady and
		// covered by the subtest below.
		SetTeamState(team, TeamState{
			WaitingHumanReview: 5, ChangesRequested: 6, UnresolvedThreads: 4, Conflicts: 3,
			FailedPipeline: 2, LinearNotReady: 7, ApprovalsUnknown: 8,
		})
		if got := testutil.CollectAndCount(MergeRequestsLinearNotReady); got != 0 {
			t.Errorf("linear_not_ready published %d series from SetTeamState; it must go through SetLinearNotReady", got)
		}
		gauges := map[string]*prometheus.GaugeVec{
			"waiting_human_review": MergeRequestsWaitingHumanReview,
			"changes_requested":    MergeRequestsWithChangesRequested,
			"unresolved_threads":   MergeRequestsWithUnresolvedThreads,
			"conflicts":            MergeRequestsWithConflicts,
			"failed_pipeline":      MergeRequestsWithFailedPipeline,
			"unknown_approvals":    MergeRequestsWithUnknownApprovals,
		}
		want := map[string]float64{
			"waiting_human_review": 5, "changes_requested": 6, "unresolved_threads": 4,
			"conflicts": 3, "failed_pipeline": 2, "unknown_approvals": 8,
		}
		for name, g := range gauges {
			if got := testutil.ToFloat64(g.WithLabelValues(team)); got != want[name] {
				t.Errorf("%s = %v, want %v", name, got, want[name])
			}
		}
		SetTeamState(team, TeamState{}) // a later scan found nothing — must read zero
		for name, g := range gauges {
			if got := testutil.ToFloat64(g.WithLabelValues(team)); got != 0 {
				t.Errorf("%s = %v, want 0 after a clean scan", name, got)
			}
		}
	})

	// Absent, not zero. With the column order unreadable every card grades unknown,
	// so the count computes to zero while the merge requests are still parked — a
	// zero here says "nothing is stuck before In Review" at exactly the moment the
	// service lost the ability to tell.
	t.Run("SetLinearNotReady clears rather than zeroes", func(t *testing.T) {
		gateTeam := uniqueLabel("linear-not-ready-team")
		SetLinearNotReady(gateTeam, 5)
		if got := testutil.ToFloat64(MergeRequestsLinearNotReady.WithLabelValues(gateTeam)); got != 5 {
			t.Errorf("linear_not_ready = %v, want 5", got)
		}
		before := testutil.CollectAndCount(MergeRequestsLinearNotReady)
		ClearLinearNotReady(gateTeam)
		if got := testutil.CollectAndCount(MergeRequestsLinearNotReady); got != before-1 {
			t.Errorf("series count = %d, want %d — the team's series must be absent, not zero", got, before-1)
		}
	})

	t.Run("LinearGateAmbiguous", func(t *testing.T) {
		ambiguous := uniqueLabel("ambiguous-team")
		LinearGateAmbiguous(ambiguous)
		LinearGateAmbiguous(ambiguous)
		if got := testutil.ToFloat64(LinearGateAmbiguousTotal.WithLabelValues(ambiguous)); got != 2 {
			t.Errorf("linear_gate_ambiguous_total = %v, want 2", got)
		}
	})

	t.Run("ObserveJob", func(t *testing.T) {
		kind := uniqueLabel("observejob-test-kind")
		ObserveJob(kind, "completed", time.Second)
		if got := testutil.ToFloat64(RiverJobsTotal.WithLabelValues(kind, "completed")); got != 1 {
			t.Errorf("river_jobs_total = %v, want 1", got)
		}
	})

	t.Run("ObserveLinearRequest", func(t *testing.T) {
		operation := uniqueLabel("linear-operation")
		before := testutil.CollectAndCount(LinearRequestDurationSeconds)
		ObserveLinearRequest(operation, time.Second, nil)
		if got := testutil.ToFloat64(LinearRequestsTotal.WithLabelValues(operation, ResultOK)); got != 1 {
			t.Errorf("linear_requests_total = %v, want 1", got)
		}
		if got := testutil.CollectAndCount(LinearRequestDurationSeconds); got != before+1 {
			t.Errorf("linear_request_duration_seconds series = %d, want %d", got, before+1)
		}
	})

	t.Run("SetLinearIssuesInReview overwrites", func(t *testing.T) {
		linearTeam := uniqueLabel("linear-team")
		SetLinearIssuesInReview(linearTeam, 4)
		SetLinearIssuesInReview(linearTeam, 1)
		if got := testutil.ToFloat64(LinearIssuesInReview.WithLabelValues(linearTeam)); got != 1 {
			t.Errorf("linear_issues_in_review = %v, want 1", got)
		}
	})

	// An unknown count must go absent, never stale and never zero: this is the
	// gauge an operator watches for work piling up in review, so a Linear outage
	// that left the last good number behind read as a board that stopped moving,
	// and a zero would have read as a board that drained.
	t.Run("ClearLinearIssuesInReview removes the series", func(t *testing.T) {
		linearTeam := uniqueLabel("linear-cleared-team")
		before := testutil.CollectAndCount(LinearIssuesInReview)
		SetLinearIssuesInReview(linearTeam, 8)
		if got := testutil.CollectAndCount(LinearIssuesInReview); got != before+1 {
			t.Fatalf("series = %d, want %d after publishing a count", got, before+1)
		}
		ClearLinearIssuesInReview(linearTeam)
		if got := testutil.CollectAndCount(LinearIssuesInReview); got != before {
			t.Errorf("series = %d, want %d: the team's series is still exported", got, before)
		}
	})

	t.Run("DigestSourceError", func(t *testing.T) {
		sourceTeam := uniqueLabel("source-error-team")
		before := testutil.ToFloat64(DigestSourceErrorsTotal.WithLabelValues(sourceTeam, SourceLinear))
		DigestSourceError(sourceTeam, SourceLinear)
		if got := testutil.ToFloat64(DigestSourceErrorsTotal.WithLabelValues(sourceTeam, SourceLinear)); got != before+1 {
			t.Errorf("digest_source_errors_total = %v, want %v", got, before+1)
		}
	})

	t.Run("JobRetry counts only attempts past the first", func(t *testing.T) {
		kind := uniqueLabel("retry-test-kind")
		// WithLabelValues would itself create the series, so the "no series yet"
		// check has to go through the vec's total count.
		before := testutil.CollectAndCount(RiverJobRetriesTotal)
		JobRetry(kind, 1)
		if got := testutil.CollectAndCount(RiverJobRetriesTotal); got != before {
			t.Fatalf("first attempt created a series (%d → %d); it is not a retry", before, got)
		}
		JobRetry(kind, 2)
		if got := testutil.ToFloat64(RiverJobRetriesTotal.WithLabelValues(kind)); got != 1 {
			t.Errorf("river_job_retries_total = %v, want 1", got)
		}
	})
}

// TestGitLabRequestErrorStatusLabel pins the transport sentinel: without it
// every DNS/dial failure would be labelled status="0".
func TestGitLabRequestErrorStatusLabel(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   string
	}{
		{500, "500"},
		{429, "429"},
		{0, statusTransport},
		{-1, statusTransport},
	} {
		if got := statusLabel(tc.status); got != tc.want {
			t.Errorf("statusLabel(%d) = %q, want %q", tc.status, got, tc.want)
		}
	}

	// The endpoint label is the thing under test here, so it stays literal and
	// the assertion measures the delta instead.
	before := testutil.ToFloat64(GitLabRequestErrorsTotal.WithLabelValues("/projects/:key", statusTransport))
	GitLabRequestError("/projects/:key", 0)
	if got := testutil.ToFloat64(GitLabRequestErrorsTotal.WithLabelValues("/projects/:key", statusTransport)); got != before+1 {
		t.Errorf("transport error counter = %v, want %v", got, before+1)
	}
}

// TestReviewFindingsSuppressedSkipsEmptyStages: a review that suppressed nothing
// must not materialise a series. Zero-valued series per stage per team would make
// "is this team's reviewer dropping everything?" unreadable on a dashboard, which
// is the only question the metric exists to answer.
func TestReviewFindingsSuppressedSkipsEmptyStages(t *testing.T) {
	const team = "suppress-test"

	// A delta rather than an absolute count: other tests in this package share the
	// collector and have already materialised series on it.
	before := testutil.CollectAndCount(ReviewFindingsSuppressedTotal, "ai_review_findings_suppressed_total")
	ReviewFindingsSuppressed(team, nil)
	ReviewFindingsSuppressed(team, map[string]int{"threshold": 0})
	if got := testutil.CollectAndCount(ReviewFindingsSuppressedTotal, "ai_review_findings_suppressed_total"); got != before {
		t.Errorf("series count moved from %d to %d on empty tallies; want no new series", before, got)
	}

	ReviewFindingsSuppressed(team, map[string]int{"not_in_diff": 2, "threshold": 1, "empty": 0})
	if got := testutil.ToFloat64(ReviewFindingsSuppressedTotal.WithLabelValues(team, "not_in_diff")); got != 2 {
		t.Errorf("not_in_diff = %v, want 2", got)
	}
	if got := testutil.ToFloat64(ReviewFindingsSuppressedTotal.WithLabelValues(team, "threshold")); got != 1 {
		t.Errorf("threshold = %v, want 1", got)
	}
}
