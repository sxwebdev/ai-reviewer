package service

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tkcrm/mx/logger"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/sxwebdev/ai-reviewer/internal/dbtypes"
	"github.com/sxwebdev/ai-reviewer/internal/gitlab"
	"github.com/sxwebdev/ai-reviewer/internal/store/repos/repo_review"
)

// TestReviewDue walks the §6.5 ladder rung by rung, on both sides of each
// threshold. The thresholds are fixed by the plan and not configurable, so a
// table is the whole contract.
func TestReviewDue(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		failures int64
		since    time.Duration // how long ago the last failure was
		wantDue  bool
		wantWait time.Duration
	}{
		{"never failed", 0, 0, true, 0},
		{"one failure, 14m ago", 1, 14 * time.Minute, false, time.Minute},
		{"one failure, exactly 15m ago", 1, 15 * time.Minute, true, 0},
		{"one failure, 16m ago", 1, 16 * time.Minute, true, 0},
		{"two failures, 59m ago", 2, 59 * time.Minute, false, time.Minute},
		{"two failures, 1h ago", 2, time.Hour, true, 0},
		{"three failures, 5h ago", 3, 5 * time.Hour, false, time.Hour},
		{"three failures, 6h ago", 3, 6 * time.Hour, true, 0},
		// Four failures is the permanent stop: only a new head SHA clears it,
		// and a new head SHA is a different row set, so the counter resets by
		// itself.
		{"four failures, a day ago", 4, 24 * time.Hour, false, 0},
		{"ten failures, a week ago", 10, 7 * 24 * time.Hour, false, 0},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			stats := repo_review.FailureStats{Failures: c.failures}
			if c.failures > 0 {
				stats.LastFailureAt = testNow.Add(-c.since)
			}
			due, wait := reviewDue(stats, testNow)
			if due != c.wantDue {
				t.Errorf("reviewDue = %v, want %v", due, c.wantDue)
			}
			if wait != c.wantWait {
				t.Errorf("wait = %v, want %v", wait, c.wantWait)
			}
		})
	}
}

func (h *harness) seedFailures(t *testing.T, headSHA string, n int, at time.Time) {
	t.Helper()
	for i := range n {
		rev, err := h.st.Review().Create(t.Context(), repo_review.CreateParams{
			ProjectID:    testProjectID,
			ProjectPath:  "backend/payments",
			Team:         testTeam,
			MrIid:        testMRIID,
			HeadSha:      headSHA,
			Status:       StatusFailed,
			PipelineJson: dbtypes.EmptyObject(),
			RiskJson:     dbtypes.EmptyObject(),
			Error:        "boom",
			Attempt:      int32(i + 1),
		})
		if err != nil {
			t.Fatalf("seed failure: %v", err)
		}
		// created_at defaults to now(); the ladder measures from the newest
		// failure, so it has to be moved explicitly.
		if _, err := h.pool.Exec(t.Context(),
			`UPDATE mr_reviews SET created_at = $1 WHERE id = $2`, at, rev.ID); err != nil {
			t.Fatalf("backdate failure: %v", err)
		}
	}
}

// seedInFlight writes the row startAttempt leaves behind while a review is
// running: status='failed' carrying the in-flight marker, created at `at`.
func (h *harness) seedInFlight(t *testing.T, headSHA string, at time.Time) {
	t.Helper()
	rev, err := h.st.Review().Create(t.Context(), repo_review.CreateParams{
		ProjectID:    testProjectID,
		ProjectPath:  "backend/payments",
		Team:         testTeam,
		MrIid:        testMRIID,
		HeadSha:      headSHA,
		Status:       StatusFailed,
		PipelineJson: dbtypes.EmptyObject(),
		RiskJson:     dbtypes.EmptyObject(),
		Error:        inFlightMarker,
		Attempt:      1,
	})
	if err != nil {
		t.Fatalf("seed in-flight attempt: %v", err)
	}
	if _, err := h.pool.Exec(t.Context(),
		`UPDATE mr_reviews SET created_at = $1 WHERE id = $2`, at, rev.ID); err != nil {
		t.Fatalf("backdate in-flight attempt: %v", err)
	}
}

// TestFailureStatsSeparatesRunningFromCrashed is the fix for the log line that
// cried wolf.
//
// startAttempt records the attempt before spending a cent, so for the four to ten
// minutes a review takes, its row is a status='failed' row like any other. The
// scanner runs every five minutes, so it kept reporting "held back by the failure
// backoff" at WARN — with a retry_in counted from a review that was running at
// that very moment — and then the review succeeded. Observed on MR !1394: the same
// warning at 13:05, 13:10 and 13:15, success at 13:16.
//
// The marker plus an age bound separates the two readings of that row. Both halves
// matter: a young row must not be a strike, and an old one must still be, because
// counting the crash is the entire reason the row is written up front.
func TestFailureStatsSeparatesRunningFromCrashed(t *testing.T) {
	cases := []struct {
		name         string
		age          time.Duration
		wantInFlight bool
		wantFailures int64
	}{
		{"started a moment ago", time.Minute, true, 0},
		{"a long review, still inside the job timeout", 25 * time.Minute, true, 0},
		// Past the grace the process cannot still be alive: River cancelled the job
		// at ReviewTimeout. So this is the OOM/eviction case, and it must count.
		{"older than the grace: the process died", 90 * time.Minute, false, 1},
		{"a day old", 24 * time.Hour, false, 1},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, withDB)
			seedGitLab(h.fake, testProject(), testMR(testMRIID))
			h.seedInFlight(t, testHeadSHA, testNow.Add(-c.age))

			stats, err := h.svc.failureStats(t.Context(), testProjectID, testMRIID, testHeadSHA)
			if err != nil {
				t.Fatalf("failureStats: %v", err)
			}
			if stats.InFlight != c.wantInFlight {
				t.Errorf("InFlight = %v, want %v", stats.InFlight, c.wantInFlight)
			}
			if stats.Failures != c.wantFailures {
				t.Errorf("Failures = %d, want %d", stats.Failures, c.wantFailures)
			}
			// A row that is not a strike must not contribute a timestamp either, or
			// the ladder would measure its wait from a review still in progress.
			if c.wantFailures == 0 && !stats.LastFailureAt.IsZero() {
				t.Errorf("LastFailureAt = %v, want the zero time for a live attempt", stats.LastFailureAt)
			}

			// And the scanner must not queue a second review of a SHA already being
			// reviewed — whichever reading applies, this MR is not a candidate now:
			// live means "wait for it", crashed means the ladder decides.
			res, err := h.svc.ScanRepository(t.Context(), testTeamConfig(), "backend/payments")
			if err != nil {
				t.Fatalf("ScanRepository: %v", err)
			}
			if c.wantInFlight && len(res.Candidates) != 0 {
				t.Errorf("candidates = %d while a review of the same SHA is running, want 0", len(res.Candidates))
			}
		})
	}
}

// TestScanRepositoryAppliesTheFailureBackoff exercises the ladder end to end
// through the scanner, including the reset a new push brings.
func TestScanRepositoryAppliesTheFailureBackoff(t *testing.T) {
	cases := []struct {
		name          string
		failures      int
		lastFailureAt time.Time
		wantCandidate bool
	}{
		{"no failures", 0, time.Time{}, true},
		{"one failure, too soon", 1, testNow.Add(-5 * time.Minute), false},
		{"one failure, 15m elapsed", 1, testNow.Add(-15 * time.Minute), true},
		{"two failures, too soon", 2, testNow.Add(-30 * time.Minute), false},
		{"two failures, an hour elapsed", 2, testNow.Add(-90 * time.Minute), true},
		{"three failures, too soon", 3, testNow.Add(-2 * time.Hour), false},
		{"three failures, six hours elapsed", 3, testNow.Add(-7 * time.Hour), true},
		{"four failures never retry", 4, testNow.Add(-30 * 24 * time.Hour), false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, withDB)
			seedGitLab(h.fake, testProject(), testMR(testMRIID))
			h.seedFailures(t, testHeadSHA, c.failures, c.lastFailureAt)

			res, err := h.svc.ScanRepository(t.Context(), testTeamConfig(), "backend/payments")
			if err != nil {
				t.Fatalf("ScanRepository: %v", err)
			}
			if got := len(res.Candidates) == 1; got != c.wantCandidate {
				t.Errorf("candidates = %d, want candidate=%v", len(res.Candidates), c.wantCandidate)
			}
			if len(res.Snapshots) != 1 {
				t.Errorf("snapshots = %d, want 1: a held-back MR is still inspected for the digest", len(res.Snapshots))
			}
		})
	}
}

// TestScanRepositoryNewHeadSHAResetsTheBackoff is the other half of §6.5: the
// failure counter is keyed by head SHA, so a push clears a poisoned MR without
// any explicit reset.
func TestScanRepositoryNewHeadSHAResetsTheBackoff(t *testing.T) {
	h := newHarness(t, withDB)
	proj := testProject()
	seedGitLab(h.fake, proj, testMR(testMRIID))
	h.seedFailures(t, testHeadSHA, 4, testNow.Add(-time.Minute))

	res, err := h.svc.ScanRepository(t.Context(), testTeamConfig(), "backend/payments")
	if err != nil {
		t.Fatalf("ScanRepository: %v", err)
	}
	if len(res.Candidates) != 0 {
		t.Fatalf("candidates = %d, want 0 at the top of the ladder", len(res.Candidates))
	}

	// A push: new head SHA, so the four failures no longer apply.
	seedGitLab(h.fake, proj, testMR(testMRIID, withHeadSHA("cccc333")))
	res, err = h.svc.ScanRepository(t.Context(), testTeamConfig(), "backend/payments")
	if err != nil {
		t.Fatalf("ScanRepository: %v", err)
	}
	if len(res.Candidates) != 1 {
		t.Fatalf("candidates = %d, want 1 after a new push", len(res.Candidates))
	}
	if got := res.Candidates[0].HeadSHA; got != "cccc333" {
		t.Errorf("candidate head sha = %q, want the new one", got)
	}
}

// TestScanRepositoryReviewStateMatrix is the §17 review-state matrix: no row
// means review, a row at the current SHA means skip, a row at an older SHA
// means re-review, and a failed row does not count as a review at all.
func TestScanRepositoryReviewStateMatrix(t *testing.T) {
	cases := []struct {
		name          string
		seed          func(h *harness)
		wantCandidate bool
	}{
		{"no review recorded", func(*harness) {}, true},
		{"reviewed at the current sha", func(h *harness) {
			h.seedReview(h.t, testHeadSHA, StatusReviewed)
		}, false},
		{"succeeded at the current sha", func(h *harness) {
			h.seedReview(h.t, testHeadSHA, StatusSucceeded)
		}, false},
		{"dry run at the current sha still counts", func(h *harness) {
			h.seedReview(h.t, testHeadSHA, StatusDryRun)
		}, false},
		{"reviewed at an older sha", func(h *harness) {
			h.seedReview(h.t, "older00", StatusSucceeded)
		}, true},
		{"failed at the current sha is not a review", func(h *harness) {
			h.seedFailures(h.t, testHeadSHA, 1, testNow.Add(-time.Hour))
		}, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, withDB)
			seedGitLab(h.fake, testProject(), testMR(testMRIID))
			c.seed(h)

			res, err := h.svc.ScanRepository(t.Context(), testTeamConfig(), "backend/payments")
			if err != nil {
				t.Fatalf("ScanRepository: %v", err)
			}
			if got := len(res.Candidates) == 1; got != c.wantCandidate {
				t.Errorf("candidates = %d, want candidate=%v", len(res.Candidates), c.wantCandidate)
			}
		})
	}
}

// TestScanRepositoryPicksUpStalePublications covers the §6.3 safety net: a
// review whose publication never finished is re-enqueued for delivery only —
// the LLM run is never repeated.
//
// The threshold the plan fixes is 15 minutes, so the fixtures sit on both sides
// of it. Backdating only by an hour would let any threshold under an hour pass.
func TestScanRepositoryPicksUpStalePublications(t *testing.T) {
	h := newHarness(t, withDB)
	seedGitLab(h.fake, testProject(), testMR(testMRIID))

	// A review younger than the threshold must not be picked up: publication
	// for it is probably still in flight.
	h.seedReview(t, "fresh00", StatusReviewed) // created now
	almost := h.seedReview(t, "almost0", StatusReviewed)
	stale := h.seedReview(t, "stale00", StatusReviewed)
	done := h.seedReview(t, "done000", StatusSucceeded)
	backdate := map[uuid.UUID]time.Duration{
		almost: 14 * time.Minute, // one minute short of the threshold
		stale:  16 * time.Minute,
		done:   time.Hour,
	}
	for id, age := range backdate {
		if _, err := h.pool.Exec(t.Context(),
			`UPDATE mr_reviews SET created_at = $1 WHERE id = $2`, testNow.Add(-age), id); err != nil {
			t.Fatalf("backdate: %v", err)
		}
	}

	res, err := h.svc.ScanRepository(t.Context(), testTeamConfig(), "backend/payments")
	if err != nil {
		t.Fatalf("ScanRepository: %v", err)
	}
	if len(res.StalePublish) != 1 || res.StalePublish[0] != stale {
		t.Errorf("stale publications = %v, want only %v (the 14-minute-old one is not stale yet)",
			res.StalePublish, stale)
	}
	if h.llm.Calls != 0 {
		t.Errorf("the LLM ran %d times during a scan; scanning must never review", h.llm.Calls)
	}
}

// TestScanRepositoryStopsRetryingAnUnpublishableReview bounds the §6.3 sweep.
//
// Nothing used to end a review that can never be published — the MR was
// deleted, the project archived, the token lost its scope. The sweep had no age
// ceiling, no attempt count and no LIMIT, so such a review was re-enqueued every
// scan_interval forever through a one- or two-worker publish queue, starving the
// publications that could still land.
func TestScanRepositoryStopsRetryingAnUnpublishableReview(t *testing.T) {
	h := newHarness(t, withDB)
	seedGitLab(h.fake, testProject(), testMR(testMRIID))

	stuck := h.seedReview(t, "stuck00", StatusReviewed)
	if _, err := h.pool.Exec(t.Context(),
		`UPDATE mr_reviews SET created_at = $1 WHERE id = $2`, testNow.Add(-time.Hour), stuck); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	// The budget is a literal here on purpose: comparing against the constant
	// would make any change to it invisible, which is the one thing this test
	// exists to notice. `maxPublishSweeps` and this number must agree.
	const wantSweeps = 20
	// 100 is a runaway guard, not an expectation: a sweep with no ceiling would
	// otherwise spin until the test times out.
	sweeps := 0
	for pass := 1; pass <= 100; pass++ {
		res, err := h.svc.ScanRepository(t.Context(), testTeamConfig(), "backend/payments")
		if err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		if len(res.StalePublish) == 0 {
			break
		}
		if len(res.StalePublish) != 1 {
			t.Fatalf("pass %d re-enqueued %d publications, want 1", pass, len(res.StalePublish))
		}
		sweeps++
	}
	if sweeps != wantSweeps {
		t.Errorf("the sweep handed this review to publication %d times, want it bounded at %d", sweeps, wantSweeps)
	}
	var status, errText string
	if err := h.pool.QueryRow(t.Context(),
		`SELECT status, error FROM mr_reviews WHERE id = $1`, stuck).Scan(&status, &errText); err != nil {
		t.Fatalf("read review: %v", err)
	}
	if status != StatusAbandoned {
		t.Errorf("status = %q, want %q", status, StatusAbandoned)
	}
	if !strings.Contains(errText, "never completed") {
		t.Errorf("error = %q, want it to say why the review was retired", errText)
	}
	// It must NOT become a §6.5 strike — GitLab refusing is not the merge
	// request's fault — and it must keep the SHA out of re-review.
	stats, err := h.st.Review().FailureStats(t.Context(), repo_review.FailureStatsParams{
		ProjectID: testProjectID, MrIid: testMRIID, HeadSha: "stuck00",
	})
	if err != nil {
		t.Fatalf("FailureStats: %v", err)
	}
	if stats.Failures != 0 {
		t.Errorf("failures = %d, want 0", stats.Failures)
	}
	got, err := h.svc.alreadyReviewed(t.Context(), testProjectID, testMRIID, "stuck00")
	if err != nil {
		t.Fatalf("alreadyReviewed: %v", err)
	}
	if got != "stuck00" {
		t.Errorf("alreadyReviewed = %q, want the abandoned review to still count as reviewed", got)
	}
}

// One repository's backlog must not be able to fill the publish queue in a
// single pass; the remainder is picked up next scan.
func TestScanRepositoryBoundsTheStalePublicationBatch(t *testing.T) {
	h := newHarness(t, withDB)
	seedGitLab(h.fake, testProject(), testMR(testMRIID))

	for i := range maxStalePublishPerPass + 5 {
		id := h.seedReview(t, fmt.Sprintf("stale%03d", i), StatusReviewed)
		if _, err := h.pool.Exec(t.Context(),
			`UPDATE mr_reviews SET created_at = $1 WHERE id = $2`, testNow.Add(-time.Hour), id); err != nil {
			t.Fatalf("backdate: %v", err)
		}
	}

	res, err := h.svc.ScanRepository(t.Context(), testTeamConfig(), "backend/payments")
	if err != nil {
		t.Fatalf("ScanRepository: %v", err)
	}
	if len(res.StalePublish) != maxStalePublishPerPass {
		t.Errorf("stale publications = %d, want the batch capped at %d", len(res.StalePublish), maxStalePublishPerPass)
	}
}

func TestScanRepositoryHonoursTheTeamSwitch(t *testing.T) {
	h := newHarness(t, withDB)
	seedGitLab(h.fake, testProject(), testMR(testMRIID))

	team := testTeamConfig()
	team.AIReview = false
	res, err := h.svc.ScanRepository(t.Context(), team, "backend/payments")
	if err != nil {
		t.Fatalf("ScanRepository: %v", err)
	}
	if len(res.Candidates) != 0 {
		t.Errorf("candidates = %d, want 0 for a team with ai_review off", len(res.Candidates))
	}
	// §9.1: the detail endpoint is only for merge requests that passed the cheap
	// filter. A team with ai_review off has none, so a scan of it costs exactly
	// one GitLab request.
	if len(res.Snapshots) != 0 {
		t.Errorf("snapshots = %d, want 0: nothing may be fetched for a team with ai_review off", len(res.Snapshots))
	}
	if h.gl.discussionCalls != 0 {
		t.Errorf("the scan made %d discussion calls, want 0", h.gl.discussionCalls)
	}
}

// TestScanRepositoryDoesNotFetchDetailsForUnchangedMergeRequests is §9.1's
// budget rule. state, draft and the head SHA all arrive in the list response, so
// a repository whose merge requests have not moved must cost one request per
// pass — not one plus four per open MR, which at 40 repositories × 20 MRs every
// five minutes is ~920k GitLab requests a day against the §20.6 rate limit.
func TestScanRepositoryDoesNotFetchDetailsForUnchangedMergeRequests(t *testing.T) {
	h := newHarness(t, withDB)
	proj := testProject()
	seedGitLab(h.fake, proj, testMR(101), testMR(102, withHeadSHA("moved00")))

	// 101 has been reviewed at its current head; 102 has not.
	if _, err := h.st.Review().Create(t.Context(), repo_review.CreateParams{
		ProjectID: testProjectID, ProjectPath: "backend/payments", Team: testTeam,
		MrIid: 101, HeadSha: testHeadSHA, Status: StatusSucceeded,
		PipelineJson: dbtypes.EmptyObject(), RiskJson: dbtypes.EmptyObject(), Attempt: 1,
	}); err != nil {
		t.Fatalf("seed review: %v", err)
	}

	res, err := h.svc.ScanRepository(t.Context(), testTeamConfig(), "backend/payments")
	if err != nil {
		t.Fatalf("ScanRepository: %v", err)
	}
	if len(res.Snapshots) != 1 || res.Snapshots[0].MR.IID != 102 {
		t.Fatalf("inspected %d MRs (%+v), want only the one whose head moved", len(res.Snapshots), res.Snapshots)
	}
	if len(res.Candidates) != 1 || res.Candidates[0].MRIID != 102 {
		t.Errorf("candidates = %+v, want only 102", res.Candidates)
	}
	// One discussions call, for 102 alone. The reviewed MR cost nothing beyond
	// the list entry it arrived in.
	if h.gl.discussionCalls != 1 {
		t.Errorf("discussion calls = %d, want 1 (only the candidate's detail load)", h.gl.discussionCalls)
	}
	// §9.1 puts /versions and /approvals under digest, not scan.
	if h.gl.versionCalls != 0 || h.gl.approvalCalls != 0 {
		t.Errorf("scan called /versions %d times and /approvals %d times, want 0 of each",
			h.gl.versionCalls, h.gl.approvalCalls)
	}
}

func TestScanRepositoryFailsWhenTheRepositoryCannotBeListed(t *testing.T) {
	h := newHarness(t, withDB)
	h.gl.failProject["backend%2Fpayments"] = errors.New("500 from gitlab")

	if _, err := h.svc.ScanRepository(t.Context(), testTeamConfig(), "backend/payments"); err == nil {
		t.Fatal("a repository that cannot be listed at all must be an error so the job retries")
	}
}

// TestScanRepositoryReportsPartialDataForOneUnreadableMR pins that one broken
// MR does not cost a repository its whole scan.
func TestScanRepositoryReportsPartialDataForOneUnreadableMR(t *testing.T) {
	h := newHarness(t, withDB)
	seedGitLab(h.fake, testProject(), testMR(101), testMR(102))
	h.gl.failMR[102] = &gitlab.APIError{Status: 500, Path: "/merge_requests/102"}

	res, err := h.svc.ScanRepository(t.Context(), testTeamConfig(), "backend/payments")
	if err != nil {
		t.Fatalf("ScanRepository: %v", err)
	}
	if !res.Failed {
		t.Error("the result must be marked partial")
	}
	if len(res.Snapshots) != 1 || res.Snapshots[0].MR.IID != 101 {
		t.Errorf("snapshots = %d, want the readable MR to survive", len(res.Snapshots))
	}
	if len(res.Candidates) != 1 || res.Candidates[0].MRIID != 101 {
		t.Errorf("candidates = %v, want only the readable MR", res.Candidates)
	}
}

func TestScanRepositoryCandidateCarriesEverythingTheJobNeeds(t *testing.T) {
	h := newHarness(t, withDB)
	seedGitLab(h.fake, testProject(), testMR(testMRIID))

	res, err := h.svc.ScanRepository(t.Context(), testTeamConfig(), "backend/payments")
	if err != nil {
		t.Fatalf("ScanRepository: %v", err)
	}
	if len(res.Candidates) != 1 {
		t.Fatalf("candidates = %d, want 1", len(res.Candidates))
	}
	got := res.Candidates[0]
	want := ReviewRequest{
		Team:        testTeam,
		ProjectPath: "backend/payments",
		ProjectID:   testProjectID,
		MRIID:       testMRIID,
		HeadSHA:     testHeadSHA,
	}
	if got != want {
		t.Errorf("candidate = %+v, want %+v", got, want)
	}
	// Publish is left false: resolving it from config and job args is the jobs
	// layer's job (§15), and guessing here would silently override a CLI flag.
	if got.Publish {
		t.Error("the scanner must not decide whether to publish")
	}
}

func TestScanRepositoryFailsWhenTheOpenMRListIsUnavailable(t *testing.T) {
	h := newHarness(t, withDB)
	seedGitLab(h.fake, testProject(), testMR(testMRIID))
	h.gl.failOpenMRs = errors.New("502 from gitlab")

	if _, err := h.svc.ScanRepository(t.Context(), testTeamConfig(), "backend/payments"); err == nil {
		t.Fatal("a repository whose MR list cannot be read must be an error, not an empty scan")
	}
}

// TestScanRepositoryBackoffWarningNamesTheReason is the other half of §6.5: an
// MR that has stopped costing money must not become invisible. Once the ladder
// gives up, the warning is the only place an operator learns *why* the MR is
// poisoned, so it has to carry the last error alongside the identifiers.
func TestScanRepositoryBackoffWarningNamesTheReason(t *testing.T) {
	h := newHarness(t, withDB)
	seedGitLab(h.fake, testProject(), testMR(testMRIID))

	core, logs := observer.New(zapcore.WarnLevel)
	h.svc.log = logger.New(logger.WithZapOption(zap.WrapCore(
		func(zapcore.Core) zapcore.Core { return core },
	)))

	// Four failures: the permanent stop.
	h.seedFailures(t, testHeadSHA, 4, testNow.Add(-time.Hour))
	if _, err := h.pool.Exec(t.Context(),
		`UPDATE mr_reviews SET error = $1 WHERE status = $2`,
		"all 1 review passes failed: schema-invalid JSON", StatusFailed); err != nil {
		t.Fatalf("set error: %v", err)
	}

	res, err := h.svc.ScanRepository(t.Context(), testTeamConfig(), "backend/payments")
	if err != nil {
		t.Fatalf("ScanRepository: %v", err)
	}
	if len(res.Candidates) != 0 {
		t.Fatalf("candidates = %d, want 0 at the top of the ladder", len(res.Candidates))
	}

	entries := logs.FilterMessageSnippet("failure backoff").All()
	if len(entries) != 1 {
		t.Fatalf("backoff warnings = %d, want exactly 1", len(entries))
	}
	fields := entries[0].ContextMap()
	for key, want := range map[string]any{
		"project":  "backend/payments",
		"iid":      int64(testMRIID),
		"head_sha": testHeadSHA,
		"failures": int64(4),
	} {
		if got := fields[key]; got != want {
			t.Errorf("warning field %q = %v, want %v", key, got, want)
		}
	}
	last, _ := fields["last_error"].(string)
	if !strings.Contains(last, "schema-invalid JSON") {
		t.Errorf("last_error = %q, want the reason the MR is poisoned", last)
	}
}
