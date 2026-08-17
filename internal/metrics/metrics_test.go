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
		SetTeamState(team, TeamState{WaitingHumanReview: 5, UnresolvedThreads: 4, Conflicts: 3, FailedPipeline: 2})
		SetTeamState(team, TeamState{}) // a later scan found nothing — must read zero
		for name, g := range map[string]*prometheus.GaugeVec{
			"waiting_human_review": MergeRequestsWaitingHumanReview,
			"unresolved_threads":   MergeRequestsWithUnresolvedThreads,
			"conflicts":            MergeRequestsWithConflicts,
			"failed_pipeline":      MergeRequestsWithFailedPipeline,
		} {
			if got := testutil.ToFloat64(g.WithLabelValues(team)); got != 0 {
				t.Errorf("%s = %v, want 0 after a clean scan", name, got)
			}
		}
	})

	t.Run("ObserveJob", func(t *testing.T) {
		kind := uniqueLabel("observejob-test-kind")
		ObserveJob(kind, "completed", time.Second)
		if got := testutil.ToFloat64(RiverJobsTotal.WithLabelValues(kind, "completed")); got != 1 {
			t.Errorf("river_jobs_total = %v, want 1", got)
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
