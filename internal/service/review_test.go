package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/tkcrm/mx/logger"

	"github.com/sxwebdev/ai-reviewer/internal/dbtypes"
	"github.com/sxwebdev/ai-reviewer/internal/domain"
	"github.com/sxwebdev/ai-reviewer/internal/gitlab"
	"github.com/sxwebdev/ai-reviewer/internal/llm"
	"github.com/sxwebdev/ai-reviewer/internal/review"
	"github.com/sxwebdev/ai-reviewer/internal/store/repos/repo_finding"
	"github.com/sxwebdev/ai-reviewer/internal/store/repos/repo_review"
)

// jobMarkerTeam labels the row an OnPersist callback writes. It stands in for
// the river_job insert the real jobs layer performs: both are writes made with
// the caller's transaction, so a rollback must take them both.
const jobMarkerTeam = "job-marker"

// enqueueMarker is an OnPersist that behaves like the real one: it writes
// through the transaction it was handed, and it verifies that the review row is
// already visible on that transaction — which is only true if the service put
// them in the same one.
func enqueueMarker(t *testing.T, calls *int, failWith error) OnPersist {
	t.Helper()
	return func(ctx context.Context, tx pgx.Tx, reviewID uuid.UUID) error {
		*calls++
		var visible int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM mr_reviews WHERE id = $1`, reviewID).Scan(&visible); err != nil {
			return err
		}
		if visible != 1 {
			t.Errorf("the review row is not visible to the enqueue callback (%d rows); "+
				"the write and the enqueue are not in one transaction", visible)
		}
		// The review id goes in the slot so repeated calls cannot collide on the
		// slot's unique index.
		if _, err := tx.Exec(ctx,
			`INSERT INTO digest_runs (team, slot, run_date, attempt, status, error)
			 VALUES ($1, $2, DATE '2026-08-13', 0, 'built', $2)`,
			jobMarkerTeam, reviewID.String()); err != nil {
			return err
		}
		return failWith
	}
}

func (h *harness) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := h.pool.QueryRow(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	return n
}

func (h *harness) enqueuedMarkers(t *testing.T) int {
	t.Helper()
	return h.count(t, `SELECT count(*) FROM digest_runs WHERE team = $1`, jobMarkerTeam)
}

func (h *harness) reviewRequest(publish bool) ReviewRequest {
	return h.reviewRequestAt(testHeadSHA, publish)
}

// reviewRequestAt names the head SHA the job was queued for. It matters because
// a request whose SHA is not the MR's current head is a head_moved skip: the
// queue's uniqueness key is the queued SHA, so retargeting would silently
// disable deduplication.
func (h *harness) reviewRequestAt(headSHA string, publish bool) ReviewRequest {
	return ReviewRequest{
		Team:        testTeam,
		ProjectPath: "backend/payments",
		ProjectID:   testProjectID,
		MRIID:       testMRIID,
		HeadSHA:     headSHA,
		Publish:     publish,
	}
}

// seedReview inserts a review row directly, standing in for an earlier run.
func (h *harness) seedReview(t *testing.T, headSHA, status string) uuid.UUID {
	t.Helper()
	rev, err := h.st.Review().Create(t.Context(), repo_review.CreateParams{
		ProjectID:    testProjectID,
		ProjectPath:  "backend/payments",
		Team:         testTeam,
		MrIid:        testMRIID,
		HeadSha:      headSHA,
		Status:       status,
		PipelineJson: dbtypes.EmptyObject(),
		RiskJson:     dbtypes.EmptyObject(),
		Attempt:      1,
	})
	if err != nil {
		t.Fatalf("seed review: %v", err)
	}
	return rev.ID
}

// seedFinding inserts a finding row for a review, unpublished by default.
func (h *harness) seedFinding(t *testing.T, reviewID uuid.UUID, fingerprint string) uuid.UUID {
	t.Helper()
	if _, err := h.st.Finding().Insert(t.Context(), repo_finding.InsertParams{
		ReviewID:     reviewID,
		ProjectID:    testProjectID,
		MrIid:        testMRIID,
		Fingerprint:  fingerprint,
		Severity:     "high",
		Category:     "correctness",
		FilePath:     "main.go",
		Title:        "leaks a connection",
		Body:         "body",
		PositionJson: dbtypes.EmptyObject(),
	}); err != nil {
		t.Fatalf("seed finding: %v", err)
	}
	var id uuid.UUID
	if err := h.pool.QueryRow(t.Context(),
		`SELECT id FROM mr_findings WHERE project_id = $1 AND mr_iid = $2 AND fingerprint = $3`,
		testProjectID, testMRIID, fingerprint).Scan(&id); err != nil {
		t.Fatalf("read seeded finding: %v", err)
	}
	return id
}

func TestRunReviewPersistsAndEnqueuesInOneTransaction(t *testing.T) {
	h := newHarness(t, withDB, withLLM(reviewResponse("leaks a connection")))
	seedGitLab(h.fake, testProject(), testMR(testMRIID))

	calls := 0
	out, err := h.svc.RunReview(t.Context(), h.reviewRequest(true), enqueueMarker(t, &calls, nil))
	if err != nil {
		t.Fatalf("RunReview: %v", err)
	}

	if out.Status != StatusReviewed {
		t.Errorf("status = %q, want %q", out.Status, StatusReviewed)
	}
	if out.Findings != 1 {
		t.Errorf("findings = %d, want 1", out.Findings)
	}
	if calls != 1 {
		t.Errorf("the enqueue callback ran %d times, want exactly 1", calls)
	}
	if n := h.count(t, `SELECT count(*) FROM mr_reviews WHERE id = $1`, out.ReviewID); n != 1 {
		t.Errorf("mr_reviews rows = %d, want 1", n)
	}
	if n := h.count(t, `SELECT count(*) FROM mr_findings WHERE review_id = $1`, out.ReviewID); n != 1 {
		t.Errorf("mr_findings rows = %d, want 1", n)
	}
	if n := h.enqueuedMarkers(t); n != 1 {
		t.Errorf("enqueued jobs = %d, want 1 committed with the review", n)
	}
	// RunReview never writes to GitLab; publication is a separate job.
	if len(h.fake.WriteLog) != 0 {
		t.Errorf("RunReview wrote to GitLab: %v", h.fake.WriteLog)
	}
}

// cost_usd is the one numeric column, and this is the only place a float64
// becomes one: `claude` reports total_cost_usd as a JSON number, the engine sums
// it across passes, and persistReview converts. Everything downstream of the
// conversion is exact, so a mistake here (truncating, rounding, taking the
// float32 path) is invisible except in the digits that land in the row.
//
// The cost carries eight significant digits on purpose: fewer and a float32 hop
// survives the column's six decimal places unchanged, which would leave this
// test agreeing with a conversion that quietly loses precision on bigger sums.
func TestRunReviewPersistsTheCostAtFullPrecision(t *testing.T) {
	resp := reviewResponse("leaks a connection")
	resp.CostUSD = 99.123456
	h := newHarness(t, withDB, withLLM(resp))
	seedGitLab(h.fake, testProject(), testMR(testMRIID))

	out, err := h.svc.RunReview(t.Context(), h.reviewRequest(true), enqueueMarker(t, new(int), nil))
	if err != nil {
		t.Fatalf("RunReview: %v", err)
	}

	// Read it as text so the assertion is about what Postgres holds, not about
	// how any Go type chooses to render it.
	var stored string
	if err := h.pool.QueryRow(t.Context(),
		`SELECT cost_usd::text FROM mr_reviews WHERE id = $1`, out.ReviewID).Scan(&stored); err != nil {
		t.Fatalf("read cost_usd: %v", err)
	}
	if stored != "99.123456" {
		t.Errorf("cost_usd = %s, want 99.123456", stored)
	}
	if out.CostUSD != resp.CostUSD {
		t.Errorf("outcome cost = %v, want %v", out.CostUSD, resp.CostUSD)
	}
}

// TestRunReviewRollsBackTheWholeCommitWhenTheEnqueueFails is the §17 rollback
// test: a failed InsertTx must leave neither the job nor the review behind.
// Break the transaction (call onPersist outside RunInTx, or ignore its error)
// and this fails.
func TestRunReviewRollsBackTheWholeCommitWhenTheEnqueueFails(t *testing.T) {
	h := newHarness(t, withDB, withLLM(reviewResponse("leaks a connection")))
	seedGitLab(h.fake, testProject(), testMR(testMRIID))

	boom := errors.New("river is unreachable")
	calls := 0
	_, err := h.svc.RunReview(t.Context(), h.reviewRequest(true), enqueueMarker(t, &calls, boom))
	if !errors.Is(err, boom) {
		t.Fatalf("RunReview error = %v, want it to wrap %v", err, boom)
	}
	if calls != 1 {
		t.Fatalf("the enqueue callback ran %d times, want 1", calls)
	}

	// The callback did insert its row before failing, so a row surviving here
	// proves the two writes were not in one transaction.
	if n := h.enqueuedMarkers(t); n != 0 {
		t.Errorf("enqueued jobs = %d, want 0 after the rollback", n)
	}
	if n := h.count(t, `SELECT count(*) FROM mr_reviews WHERE status <> $1`, StatusFailed); n != 0 {
		t.Errorf("non-failed mr_reviews rows = %d, want 0 after the rollback", n)
	}
	if n := h.count(t, `SELECT count(*) FROM mr_findings`); n != 0 {
		t.Errorf("mr_findings rows = %d, want 0 after the rollback", n)
	}
	// The attempt is still recorded as a failure so the backoff can count it.
	if n := h.count(t, `SELECT count(*) FROM mr_reviews WHERE status = $1`, StatusFailed); n != 1 {
		t.Errorf("failed mr_reviews rows = %d, want 1", n)
	}
}

func TestRunReviewDryRunPersistsButNeverPublishes(t *testing.T) {
	h := newHarness(t, withDB, withLLM(reviewResponse("leaks a connection")))
	seedGitLab(h.fake, testProject(), testMR(testMRIID))

	calls := 0
	out, err := h.svc.RunReview(t.Context(), h.reviewRequest(false), enqueueMarker(t, &calls, nil))
	if err != nil {
		t.Fatalf("RunReview: %v", err)
	}
	if out.Status != StatusDryRun {
		t.Errorf("status = %q, want %q", out.Status, StatusDryRun)
	}
	if calls != 0 {
		t.Errorf("the enqueue callback ran %d times, want 0 in a dry run", calls)
	}
	if n := h.enqueuedMarkers(t); n != 0 {
		t.Errorf("enqueued jobs = %d, want 0 in a dry run", n)
	}
	if len(h.fake.WriteLog) != 0 {
		t.Errorf("a dry run wrote to GitLab: %v", h.fake.WriteLog)
	}
	if n := h.count(t, `SELECT count(*) FROM mr_findings WHERE review_id = $1`, out.ReviewID); n != 1 {
		t.Errorf("mr_findings rows = %d, want the findings recorded even in a dry run", n)
	}

	// The dry-run row is what stops the scanner reviewing the same SHA again
	// every five minutes — that hole is closed by the database, not by a flag.
	before := h.llm.Calls
	again, err := h.svc.RunReview(t.Context(), h.reviewRequest(false), nil)
	if err != nil {
		t.Fatalf("second RunReview: %v", err)
	}
	if again.SkipReason != domain.ReasonUpToDate {
		t.Errorf("second run skip reason = %q, want %q", again.SkipReason, domain.ReasonUpToDate)
	}
	if h.llm.Calls != before {
		t.Errorf("the LLM was called again (%d -> %d) for an already-reviewed SHA", before, h.llm.Calls)
	}
}

// TestRunReviewDedupeIgnoresUnpublishedFindings is the §10.3 invariant.
//
// A finding row with note_id IS NULL means "we computed it", not "it is hanging
// in GitLab". If such a row entered ExistingFingerprints, every dry run would
// permanently suppress its own findings once publication was switched on: the
// fingerprint does not depend on the head SHA, so no future push could clear
// it. Widen existingFingerprints to all rows and this test fails.
func TestRunReviewDedupeIgnoresUnpublishedFindings(t *testing.T) {
	h := newHarness(t, withDB, withLLM(reviewResponse("leaks a connection")))
	fp := fingerprintOf("leaks a connection")

	priorID := h.seedReview(t, "older00", StatusReviewed)
	findingID := h.seedFinding(t, priorID, fp)

	seedGitLab(h.fake, testProject(), testMR(testMRIID))

	t.Run("unpublished does not suppress", func(t *testing.T) {
		out, err := h.svc.RunReview(t.Context(), h.reviewRequest(true), enqueueMarker(t, new(int), nil))
		if err != nil {
			t.Fatalf("RunReview: %v", err)
		}
		if out.Findings != 1 {
			t.Fatalf("findings = %d, want 1: an unpublished fingerprint must not dedupe", out.Findings)
		}
	})

	t.Run("published suppresses", func(t *testing.T) {
		if err := h.st.Finding().MarkPublished(t.Context(), 4242, findingID); err != nil {
			t.Fatalf("MarkPublished: %v", err)
		}
		// A different head SHA so the run is not skipped as up to date.
		h.fake.MRs[fakeKey("7", testMRIID)] = testMR(testMRIID, withHeadSHA("bbbb222"))

		out, err := h.svc.RunReview(t.Context(), h.reviewRequest(true), enqueueMarker(t, new(int), nil))
		if err != nil {
			t.Fatalf("RunReview: %v", err)
		}
		if out.Findings != 0 {
			t.Errorf("findings = %d, want 0: a published fingerprint must dedupe", out.Findings)
		}
	})
}

// TestDryRunFindingIsPublishedOncePublicationIsEnabled is the §10.3 scenario
// end to end, and the reason mr_findings.Insert re-attaches instead of doing
// nothing.
//
// A dry run records fingerprint F with a NULL note_id. ListPublishedFingerprints
// rightly omits it, so the next review emits F again — but the row it collides
// with belongs to the dry run, whose review is never published. Unless that row
// is re-attached to the new review, PublishReview finds nothing to post and F is
// stuck forever: the fingerprint does not depend on the head SHA, so no later
// push can free it.
func TestDryRunFindingIsPublishedOncePublicationIsEnabled(t *testing.T) {
	h := newHarness(t, withDB, withLLM(reviewResponse("leaks a connection")))
	proj := testProject()
	seedGitLab(h.fake, proj, testMR(testMRIID))
	fp := fingerprintOf("leaks a connection")

	// 1. ai_review_publish_enabled = false.
	dry, err := h.svc.RunReview(t.Context(), h.reviewRequest(false), nil)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if dry.Status != StatusDryRun || dry.Findings != 1 {
		t.Fatalf("dry run outcome = %+v, want one finding recorded as a dry run", dry)
	}
	if len(h.fake.WriteLog) != 0 {
		t.Fatalf("the dry run reached GitLab: %v", h.fake.WriteLog)
	}

	// 2. Publication is switched on, and the author pushes.
	seedGitLab(h.fake, proj, testMR(testMRIID, withHeadSHA("bbbb222")))
	live, err := h.svc.RunReview(t.Context(), h.reviewRequestAt("bbbb222", true), enqueueMarker(t, new(int), nil))
	if err != nil {
		t.Fatalf("live review: %v", err)
	}
	if live.Findings != 1 {
		t.Fatalf("live findings = %d, want the dry run's fingerprint not to suppress it", live.Findings)
	}

	// The single row for F now belongs to the review that will publish it.
	if rows := h.count(t, `SELECT count(*) FROM mr_findings WHERE fingerprint = $1`, fp); rows != 1 {
		t.Fatalf("mr_findings rows for the fingerprint = %d, want 1", rows)
	}
	var reviewID uuid.UUID
	if err := h.pool.QueryRow(t.Context(),
		`SELECT review_id FROM mr_findings WHERE fingerprint = $1`, fp).
		Scan(&reviewID); err != nil {
		t.Fatalf("read finding: %v", err)
	}
	if reviewID != live.ReviewID {
		t.Errorf("finding is attached to review %s, want the publishing review %s "+
			"— it would never be posted otherwise", reviewID, live.ReviewID)
	}

	// 3. Publication delivers it.
	published, err := h.svc.PublishReview(t.Context(), live.ReviewID)
	if err != nil {
		t.Fatalf("PublishReview: %v", err)
	}
	if published != 1 {
		t.Fatalf("published = %d, want the dry run's finding to finally reach GitLab", published)
	}
	if len(h.fake.CreatedDiscussions) != 1 {
		t.Fatalf("created discussions = %d, want 1", len(h.fake.CreatedDiscussions))
	}
	if fps := gitlab.ParseFindingMarkers(h.fake.CreatedDiscussions[0]); len(fps) != 1 || fps[0] != fp {
		t.Errorf("published comment carries %v, want the fingerprint %s", fps, fp)
	}

	// 4. The counterweight: once a finding is in GitLab its row stays bound to
	// the review that put it there, so the audit trail and the note id survive.
	seedGitLab(h.fake, proj, testMR(testMRIID, withHeadSHA("cccc333")))
	third, err := h.svc.RunReview(t.Context(), h.reviewRequestAt("cccc333", true), enqueueMarker(t, new(int), nil))
	if err != nil {
		t.Fatalf("third review: %v", err)
	}
	if third.Findings != 0 {
		t.Errorf("third review findings = %d, want 0: F is published now", third.Findings)
	}
	var (
		stillOwnedBy uuid.UUID
		noteID       *int64
	)
	if err := h.pool.QueryRow(t.Context(),
		`SELECT review_id, note_id FROM mr_findings WHERE fingerprint = $1`, fp).
		Scan(&stillOwnedBy, &noteID); err != nil {
		t.Fatalf("read finding: %v", err)
	}
	if stillOwnedBy != live.ReviewID {
		t.Errorf("a published finding was re-attached to review %s, want it to stay with %s",
			stillOwnedBy, live.ReviewID)
	}
	if noteID == nil {
		t.Error("the note id of a published finding must never be cleared")
	}
}

// TestRunReviewDuplicateFingerprintDoesNotAbortPersistence covers the §17
// bullet: Insert carries ON CONFLICT DO NOTHING precisely so a fingerprint that
// is already recorded cannot take the whole review's persistence down with it.
func TestRunReviewDuplicateFingerprintDoesNotAbortPersistence(t *testing.T) {
	h := newHarness(t, withDB, withLLM(reviewResponse("leaks a connection")))
	fp := fingerprintOf("leaks a connection")

	priorID := h.seedReview(t, "older00", StatusReviewed)
	h.seedFinding(t, priorID, fp)
	seedGitLab(h.fake, testProject(), testMR(testMRIID))

	out, err := h.svc.RunReview(t.Context(), h.reviewRequest(true), enqueueMarker(t, new(int), nil))
	if err != nil {
		t.Fatalf("RunReview must survive a duplicate fingerprint: %v", err)
	}
	if out.Status != StatusReviewed {
		t.Errorf("status = %q, want the review to be persisted anyway", out.Status)
	}
	if n := h.count(t, `SELECT count(*) FROM mr_findings WHERE fingerprint = $1`, fp); n != 1 {
		t.Errorf("mr_findings rows for the fingerprint = %d, want 1 (the duplicate was swallowed)", n)
	}
	if n := h.enqueuedMarkers(t); n != 1 {
		t.Errorf("enqueued jobs = %d, want 1 — the duplicate must not roll the transaction back", n)
	}
}

// TestRunReviewFingerprintMarkersInDiscussionsSuppressFindings pins the other
// half of the dedupe union: a finding whose marker is on the MR is already
// hanging in GitLab even if this database has never heard of it.
func TestRunReviewFingerprintMarkersInDiscussionsSuppressFindings(t *testing.T) {
	h := newHarness(t, withDB, withLLM(reviewResponse("leaks a connection")))
	proj := testProject()
	seedGitLab(h.fake, proj, testMR(testMRIID))
	setDiscussions(h.fake, proj, testMRIID, []gitlab.Discussion{{
		ID:    "d1",
		Notes: []gitlab.Note{note(11, "old finding\n\n"+gitlab.RenderFindingMarker(fingerprintOf("leaks a connection")))},
	}})

	out, err := h.svc.RunReview(t.Context(), h.reviewRequest(true), enqueueMarker(t, new(int), nil))
	if err != nil {
		t.Fatalf("RunReview: %v", err)
	}
	if out.Findings != 0 {
		t.Errorf("findings = %d, want 0: the finding is already posted on the MR", out.Findings)
	}
}

func TestRunReviewRecordsFailureForTheBackoff(t *testing.T) {
	h := newHarness(t, withDB)
	h.llm.Err = errors.New("model unavailable")
	seedGitLab(h.fake, testProject(), testMR(testMRIID))

	_, err := h.svc.RunReview(t.Context(), h.reviewRequest(true), enqueueMarker(t, new(int), nil))
	if err == nil {
		t.Fatal("RunReview must report the engine failure")
	}

	var (
		status  string
		attempt int32
		errText string
	)
	if scanErr := h.pool.QueryRow(t.Context(),
		`SELECT status, attempt, error FROM mr_reviews WHERE head_sha = $1`, testHeadSHA).
		Scan(&status, &attempt, &errText); scanErr != nil {
		t.Fatalf("read the failed review: %v", scanErr)
	}
	if status != StatusFailed {
		t.Errorf("status = %q, want %q", status, StatusFailed)
	}
	if attempt != 1 {
		t.Errorf("attempt = %d, want 1", attempt)
	}
	if errText == "" {
		t.Error("the failure must record why, or the operator cannot tell a poisoned MR from a flaky one")
	}

	// A failed row does not count as reviewed, so the next scan tries again.
	got, err := h.svc.alreadyReviewed(t.Context(), testProjectID, testMRIID, testHeadSHA)
	if err != nil {
		t.Fatalf("alreadyReviewed: %v", err)
	}
	if got != "" {
		t.Errorf("alreadyReviewed = %q, want empty: a failed attempt is not a review", got)
	}
	// Exactly one row: the attempt recorded before the expensive work is the row
	// the failure fills in, not a second one beside it.
	if n := h.count(t, `SELECT count(*) FROM mr_reviews`); n != 1 {
		t.Errorf("mr_reviews rows = %d, want 1", n)
	}
}

// TestRunReviewCountsACrashedAttempt is the under-counting half of §6.5.
//
// review.MaxAttempts is 1, so an OOM kill or a node eviction inside the LLM pass
// makes River discard the job having written nothing: FailureStats stays at 0
// and scan_repo re-runs the same pathological MR at full price every interval —
// which is exactly the failure mode (huge diff → huge prompt → OOM) the ladder
// was written for. The attempt row is written before the money is spent, so the
// crash is countable.
func TestRunReviewCountsACrashedAttempt(t *testing.T) {
	h := newHarness(t, withDB, withLLM(reviewResponse("leaks a connection")))
	seedGitLab(h.fake, testProject(), testMR(testMRIID))

	// A SIGKILL cannot be staged from inside the process, so the observation is
	// taken at the instant one would land — inside the LLM call, which is where
	// the memory goes. Whatever the database holds here is what a crash leaves
	// behind.
	var midReview, aged repo_review.FailureStats
	h.llm.ReviewFn = func(llm.Request) (*llm.ReviewResponse, error) {
		midReview, _ = h.svc.failureStats(context.Background(), testProjectID, testMRIID, testHeadSHA)
		// The same row read as it would be *after* the grace expires, which is what
		// a crash actually leaves behind: nobody comes back to rewrite it.
		//
		// The threshold is derived from the wall clock, not from testNow: created_at
		// is filled by the database's now(), which the harness's fake clock does not
		// move. Anything later than the row's real timestamp puts it past the grace.
		aged, _ = h.st.Review().FailureStats(context.Background(), repo_review.FailureStatsParams{
			ProjectID: testProjectID, MrIid: testMRIID, HeadSha: testHeadSHA,
			InFlightMarker: inFlightMarker,
			InFlightSince:  time.Now().Add(time.Minute),
		})
		return reviewResponse("leaks a connection"), nil
	}

	if _, err := h.svc.RunReview(t.Context(), h.reviewRequest(true), enqueueMarker(t, new(int), nil)); err != nil {
		t.Fatalf("RunReview: %v", err)
	}

	// While the process is alive the row is a live attempt, not a strike — that is
	// what stops the scanner warning about a review it is watching succeed.
	if !midReview.InFlight {
		t.Error("the attempt row must be recognised as in flight while the review runs")
	}
	if midReview.Failures != 0 {
		t.Errorf("failures mid-review = %d, want 0: a running review is not a failure", midReview.Failures)
	}
	// Once it can no longer be alive, the very same row is the countable crash.
	if aged.Failures != 1 {
		t.Fatalf("failures for an abandoned attempt = %d, want 1: an invisible crash is re-run at full price every scan",
			aged.Failures)
	}
	if !strings.Contains(aged.LastError, "did not report an outcome") {
		t.Errorf("last error = %q, want it to say the process never came back", aged.LastError)
	}
	// …and the row is gone once the process did come back, so a review that
	// completes never scores a strike against itself.
	after, err := h.svc.failureStats(t.Context(), testProjectID, testMRIID, testHeadSHA)
	if err != nil {
		t.Fatalf("failureStats: %v", err)
	}
	if after.Failures != 0 || after.InFlight {
		t.Errorf("after a completed review: failures = %d, in flight = %v; want 0 and false",
			after.Failures, after.InFlight)
	}
}

// TestRunReviewSucceedingLeavesNoAttemptRow is the counterweight: the row
// written up front must vanish with the success, or every review would score a
// §6.5 strike against itself.
func TestRunReviewSucceedingLeavesNoAttemptRow(t *testing.T) {
	h := newHarness(t, withDB, withLLM(reviewResponse("leaks a connection")))
	seedGitLab(h.fake, testProject(), testMR(testMRIID))

	if _, err := h.svc.RunReview(t.Context(), h.reviewRequest(true), enqueueMarker(t, new(int), nil)); err != nil {
		t.Fatalf("RunReview: %v", err)
	}
	if n := h.count(t, `SELECT count(*) FROM mr_reviews WHERE status = $1`, StatusFailed); n != 0 {
		t.Errorf("failed rows after a successful review = %d, want 0", n)
	}
	if n := h.count(t, `SELECT count(*) FROM mr_reviews`); n != 1 {
		t.Errorf("mr_reviews rows = %d, want exactly the successful review", n)
	}
}

// TestRunReviewDoesNotCountOurOwnShutdown is the over-counting half of §6.5.
//
// River's SoftStopTimeout cancels in-flight work on every rolling deploy, and
// that cancellation used to reach recordFailure as an ordinary failure — so four
// deploys landing during long reviews would blacklist a perfectly healthy head
// SHA until somebody pushed. A job TIMEOUT is a different thing and must still
// count, which is what the second case pins.
func TestRunReviewDoesNotCountOurOwnShutdown(t *testing.T) {
	cases := []struct {
		name string
		// stop ends the review's context the way this failure mode does, from
		// inside the LLM call — which is where a rolling deploy and a job timeout
		// both land, and after the attempt row has been written.
		stop func(ctx context.Context, cancel context.CancelFunc)
		// llmErr overrides what the LLM call reports; the default is ctx.Err(),
		// which is what both a cancellation and a timeout produce.
		llmErr       error
		deadline     time.Duration
		wantFailures int64
	}{
		{
			name:     "shutdown cancels the work",
			stop:     func(_ context.Context, cancel context.CancelFunc) { cancel() },
			deadline: time.Hour, wantFailures: 0,
		},
		// A deadline is the job's own 30-minute budget running out: an MR whose
		// pipeline cannot finish is precisely what the ladder exists to throttle.
		{
			name:     "the job timeout counts",
			stop:     func(ctx context.Context, _ context.CancelFunc) { <-ctx.Done() },
			deadline: 20 * time.Millisecond, wantFailures: 1,
		},
		// The context is still live here, and that is the point: a terminal sends
		// Ctrl-C to the whole process group, so our claude and git children can be
		// dead — with their error already on its way up — a moment before the
		// cancellation reaches us. Counted, it would put a strike on a healthy SHA
		// for the operator's stop.
		{
			name:     "a child killed with us does not count",
			stop:     func(context.Context, context.CancelFunc) {},
			llmErr:   fmt.Errorf("claude failed (signal: interrupt): %w", context.Canceled),
			deadline: time.Hour, wantFailures: 0,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, withDB)
			seedGitLab(h.fake, testProject(), testMR(testMRIID))

			ctx, cancel := context.WithTimeout(t.Context(), c.deadline)
			defer cancel()
			h.llm.ReviewFn = func(llm.Request) (*llm.ReviewResponse, error) {
				c.stop(ctx, cancel)
				if c.llmErr != nil {
					return nil, c.llmErr
				}
				return nil, ctx.Err()
			}

			if _, err := h.svc.RunReview(ctx, h.reviewRequest(true), enqueueMarker(t, new(int), nil)); err == nil {
				t.Fatal("RunReview must report the failure")
			}

			stats, err := h.st.Review().FailureStats(context.Background(), repo_review.FailureStatsParams{
				ProjectID: testProjectID, MrIid: testMRIID, HeadSha: testHeadSHA,
			})
			if err != nil {
				t.Fatalf("FailureStats: %v", err)
			}
			if stats.Failures != c.wantFailures {
				t.Errorf("failures = %d, want %d", stats.Failures, c.wantFailures)
			}
		})
	}
}

// TestRunReviewSkipsWhenTheHeadMovedUnderTheJob is D1: the queue's uniqueness
// key is the head SHA as QUEUED, so a worker that retargets the live head makes
// River stop deduplicating — the next scan finds no row for the new SHA and
// enqueues a second job under a different key, and two full pipelines run the
// same review. The loser then violates mr_reviews_success_uniq and is recorded
// as a failure, feeding a §6.5 strike to a healthy MR.
func TestRunReviewSkipsWhenTheHeadMovedUnderTheJob(t *testing.T) {
	h := newHarness(t, withDB, withLLM(reviewResponse("leaks a connection")))
	seedGitLab(h.fake, testProject(), testMR(testMRIID, withHeadSHA("pushed1")))

	out, err := h.svc.RunReview(t.Context(), h.reviewRequestAt("queued0", true), enqueueMarker(t, new(int), nil))
	if err != nil {
		t.Fatalf("RunReview: %v", err)
	}
	if out.SkipReason != domain.ReasonHeadMoved {
		t.Errorf("skip reason = %q, want %q", out.SkipReason, domain.ReasonHeadMoved)
	}
	if h.llm.Calls != 0 {
		t.Errorf("the LLM ran %d times on a superseded SHA", h.llm.Calls)
	}
	if n := h.count(t, `SELECT count(*) FROM mr_reviews`); n != 0 {
		t.Errorf("mr_reviews rows = %d, want 0: nothing may be recorded under either SHA", n)
	}

	// The scanner's next pass queues the SHA that is actually there, and that
	// one runs.
	again, err := h.svc.RunReview(t.Context(), h.reviewRequestAt("pushed1", true), enqueueMarker(t, new(int), nil))
	if err != nil {
		t.Fatalf("RunReview: %v", err)
	}
	if again.SkipReason != "" || again.Status != StatusReviewed {
		t.Errorf("outcome = %+v, want a review of the current head", again)
	}
}

// TestRunReviewPublishesAPreviouslyDryRunSHA is the §10.3 trap the operator
// otherwise cannot escape: after turning ai_review_publish_enabled on, every SHA
// already reviewed in dry-run mode reports up_to_date (the dry_run row satisfies
// GetByHeadSHA) while PublishReview is a no-op for dry runs and the §6.3 sweep
// only looks at 'reviewed'. Even `review <ref> --publish` answered "skipped".
func TestRunReviewPublishesAPreviouslyDryRunSHA(t *testing.T) {
	h := newHarness(t, withDB, withLLM(reviewResponse("leaks a connection")))
	seedGitLab(h.fake, testProject(), testMR(testMRIID))

	dry, err := h.svc.RunReview(t.Context(), h.reviewRequest(false), nil)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if dry.Status != StatusDryRun || dry.Findings != 1 {
		t.Fatalf("dry run outcome = %+v, want one finding recorded as a dry run", dry)
	}

	// Publication switched on; nothing pushed.
	calls := 0
	out, err := h.svc.RunReview(t.Context(), h.reviewRequest(true), enqueueMarker(t, &calls, nil))
	if err != nil {
		t.Fatalf("RunReview: %v", err)
	}
	if out.SkipReason != "" {
		t.Fatalf("skip reason = %q, want the request to be carried out", out.SkipReason)
	}
	if out.ReviewID != dry.ReviewID || out.Status != StatusReviewed || out.Findings != 1 {
		t.Errorf("outcome = %+v, want the existing review promoted to %q", out, StatusReviewed)
	}
	// No tokens: the findings were already computed.
	if h.llm.Calls != 1 {
		t.Errorf("the LLM ran %d times, want only the dry run's single call", h.llm.Calls)
	}
	if calls != 1 {
		t.Errorf("publication was enqueued %d times, want 1", calls)
	}
	if n := h.enqueuedMarkers(t); n != 1 {
		t.Errorf("enqueued jobs = %d, want the promotion and the enqueue in one transaction", n)
	}
	// One row, still the only non-failed one for this SHA, so the unique index
	// is intact.
	if n := h.count(t, `SELECT count(*) FROM mr_reviews`); n != 1 {
		t.Errorf("mr_reviews rows = %d, want the same row promoted", n)
	}

	// And it publishes for real.
	published, err := h.svc.PublishReview(t.Context(), out.ReviewID)
	if err != nil {
		t.Fatalf("PublishReview: %v", err)
	}
	if published != 1 {
		t.Errorf("published = %d, want the dry run's finding to reach GitLab", published)
	}

	// A second promotion attempt is not a second publication.
	after, err := h.svc.RunReview(t.Context(), h.reviewRequest(true), enqueueMarker(t, &calls, nil))
	if err != nil {
		t.Fatalf("RunReview: %v", err)
	}
	if after.SkipReason != domain.ReasonUpToDate {
		t.Errorf("skip reason = %q, want %q once the review has been published", after.SkipReason, domain.ReasonUpToDate)
	}
}

// A merge request that is closed, merged or a draft must not have a stale dry
// run published onto it: up_to_date is the only refusal with work behind it.
func TestRunReviewDoesNotPromoteOntoAnUnreviewableMR(t *testing.T) {
	h := newHarness(t, withDB, withLLM(reviewResponse("leaks a connection")))
	proj := testProject()
	seedGitLab(h.fake, proj, testMR(testMRIID))

	if _, err := h.svc.RunReview(t.Context(), h.reviewRequest(false), nil); err != nil {
		t.Fatalf("dry run: %v", err)
	}

	// The MR is merged before publication was ever switched on.
	seedGitLab(h.fake, proj, testMR(testMRIID, withState("merged")))
	calls := 0
	out, err := h.svc.RunReview(t.Context(), h.reviewRequest(true), enqueueMarker(t, &calls, nil))
	if err != nil {
		t.Fatalf("RunReview: %v", err)
	}
	if out.SkipReason != domain.ReasonNotOpen {
		t.Errorf("skip reason = %q, want %q", out.SkipReason, domain.ReasonNotOpen)
	}
	if calls != 0 {
		t.Errorf("publication was enqueued %d times for a merged MR", calls)
	}
	if n := h.count(t, `SELECT count(*) FROM mr_reviews WHERE status = $1`, StatusDryRun); n != 1 {
		t.Errorf("dry-run rows = %d, want the review left as a dry run", n)
	}
}

// A dry-run request must not promote anything: that would publish behind the
// operator's back.
func TestRunReviewDoesNotPromoteWithoutAPublishRequest(t *testing.T) {
	h := newHarness(t, withDB, withLLM(reviewResponse("leaks a connection")))
	seedGitLab(h.fake, testProject(), testMR(testMRIID))

	dry, err := h.svc.RunReview(t.Context(), h.reviewRequest(false), nil)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	out, err := h.svc.RunReview(t.Context(), h.reviewRequest(false), nil)
	if err != nil {
		t.Fatalf("second dry run: %v", err)
	}
	if out.SkipReason != domain.ReasonUpToDate {
		t.Errorf("skip reason = %q, want %q", out.SkipReason, domain.ReasonUpToDate)
	}
	if got := h.reviewStatus(t, dry.ReviewID); got != StatusDryRun {
		t.Errorf("status = %q, want it to stay %q", got, StatusDryRun)
	}
}

// TestRunReviewMasksModelOutput closes the exfiltration sink for a prompt
// injection. The engine masks the finding body and nothing else, but title,
// suggestion and the run summary all reach GitLab — so "put what you read into
// `suggestion`" is one instruction away from a token in a public comment.
func TestRunReviewMasksModelOutput(t *testing.T) {
	const token = "glpat-abcdefghijklmnopqrstuvwx"

	h := newHarness(t, withDB, withLLM(&llm.ReviewResponse{
		Summary:               "found " + token + " in the config",
		RiskLevel:             "high",
		OverallRecommendation: "request_changes",
		Findings: []llm.Finding{{
			Severity: "high", Category: "correctness", FilePath: "main.go",
			LineKind: "new", Line: 3,
			Title:      "credential " + token + " is hardcoded",
			Body:       "the body mentions " + token,
			Suggestion: "replace it with " + token,
		}},
	}))
	seedGitLab(h.fake, testProject(), testMR(testMRIID))

	out, err := h.svc.RunReview(t.Context(), h.reviewRequest(true), enqueueMarker(t, new(int), nil))
	if err != nil {
		t.Fatalf("RunReview: %v", err)
	}
	if _, err := h.svc.PublishReview(t.Context(), out.ReviewID); err != nil {
		t.Fatalf("PublishReview: %v", err)
	}

	// At rest…
	var summary, title, body string
	if err := h.pool.QueryRow(t.Context(),
		`SELECT r.summary, f.title, f.body FROM mr_reviews r JOIN mr_findings f ON f.review_id = r.id WHERE r.id = $1`,
		out.ReviewID).Scan(&summary, &title, &body); err != nil {
		t.Fatalf("read the persisted review: %v", err)
	}
	for name, got := range map[string]string{"summary": summary, "title": title, "body": body} {
		if strings.Contains(got, token) {
			t.Errorf("%s stored unmasked: %q", name, got)
		}
	}
	if !strings.Contains(body, "[REDACTED]") {
		t.Errorf("body = %q, want the suggestion folded in and masked", body)
	}

	// …and on the way to GitLab.
	for _, posted := range append(append([]string{}, h.fake.CreatedDiscussions...), h.fake.CreatedNotes...) {
		if strings.Contains(posted, token) {
			t.Errorf("a comment published to GitLab carries the secret: %q", posted)
		}
	}
}

// TestRunReviewMasksThePersistedError is the at-rest half of "no secrets in
// logs": the logging core catches these strings on the way out, but the copy in
// mr_reviews.error was only truncated. gitlab.APIError.Error() embeds the raw
// response body.
func TestRunReviewMasksThePersistedError(t *testing.T) {
	h := newHarness(t, withDB)
	seedGitLab(h.fake, testProject(), testMR(testMRIID))
	h.gl.failDiffs = &gitlab.APIError{
		Status: 403, Method: "GET", Path: "/diffs",
		Body: `{"message":"401 Unauthorized","token":"glpat-abcdefghijklmnopqrstuvwx"}`,
	}

	if _, err := h.svc.RunReview(t.Context(), h.reviewRequest(true), enqueueMarker(t, new(int), nil)); err == nil {
		t.Fatal("RunReview must report the GitLab failure")
	}

	var stored string
	if err := h.pool.QueryRow(t.Context(),
		`SELECT error FROM mr_reviews WHERE status = $1`, StatusFailed).Scan(&stored); err != nil {
		t.Fatalf("read the failed review: %v", err)
	}
	if strings.Contains(stored, "glpat-") {
		t.Errorf("mr_reviews.error = %q, want the token masked", stored)
	}
	if !strings.Contains(stored, "[REDACTED]") {
		t.Errorf("mr_reviews.error = %q, want the redaction placeholder", stored)
	}
}

func TestRunReviewSkipsMergeRequestsThatNeedNoReview(t *testing.T) {
	cases := []struct {
		name   string
		opt    mrOption
		queued string // the head SHA the job carries
		want   domain.Reason
	}{
		{"draft", withDraft(), testHeadSHA, domain.ReasonDraft},
		{"merged", withState("merged"), testHeadSHA, domain.ReasonNotOpen},
		{"closed", withState("closed"), testHeadSHA, domain.ReasonNotOpen},
		// An empty queued SHA is the CLI's "review whatever is there now", which
		// is the only way to reach the no_head_sha branch: a job queued for a
		// concrete SHA against an MR that has none is head_moved.
		{"no head sha", withHeadSHA(""), "", domain.ReasonNoHeadSHA},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, withDB, withLLM(reviewResponse("leaks a connection")))
			seedGitLab(h.fake, testProject(), testMR(testMRIID, c.opt))

			out, err := h.svc.RunReview(t.Context(), h.reviewRequestAt(c.queued, true), enqueueMarker(t, new(int), nil))
			if err != nil {
				t.Fatalf("RunReview: %v", err)
			}
			if out.SkipReason != c.want {
				t.Errorf("skip reason = %q, want %q", out.SkipReason, c.want)
			}
			if h.llm.Calls != 0 {
				t.Errorf("the LLM ran %d times for an MR that needed no review", h.llm.Calls)
			}
			if n := h.count(t, `SELECT count(*) FROM mr_reviews`); n != 0 {
				t.Errorf("mr_reviews rows = %d, want 0", n)
			}
		})
	}
}

func TestRunReviewWithATeamThatHasReviewSwitchedOff(t *testing.T) {
	h := newHarness(t, withDB, withConfig(func(c *Config) {
		team := testTeamConfig()
		team.AIReview = false
		c.Teams = []domain.Team{team}
	}))
	seedGitLab(h.fake, testProject(), testMR(testMRIID))

	out, err := h.svc.RunReview(t.Context(), h.reviewRequest(true), nil)
	if err != nil {
		t.Fatalf("RunReview: %v", err)
	}
	if out.SkipReason != domain.ReasonDisabled {
		t.Errorf("skip reason = %q, want %q", out.SkipReason, domain.ReasonDisabled)
	}
}

// TestRunReviewWithoutReviewableFilesIsAFailure pins that an MR the filters
// empty out is recorded as a failure: the row is what lets the backoff stop it
// after four attempts instead of re-listing its diffs every scan forever.
func TestRunReviewWithoutReviewableFilesIsAFailure(t *testing.T) {
	h := newHarness(t, withDB)
	proj := testProject()
	seedGitLab(h.fake, proj, testMR(testMRIID))
	setDiffs(h.fake, proj, testMRIID, []gitlab.MergeRequestDiff{
		{NewPath: "vendor/dep/dep.go", Diff: sampleDiff},
		{NewPath: "logo.png", Diff: "Binary files a/logo.png and b/logo.png differ\n"},
	})

	if _, err := h.svc.RunReview(t.Context(), h.reviewRequest(true), nil); err == nil {
		t.Fatal("an MR with nothing reviewable must report an error")
	}
	if h.llm.Calls != 0 {
		t.Errorf("the LLM ran %d times with no reviewable files", h.llm.Calls)
	}
	if n := h.count(t, `SELECT count(*) FROM mr_reviews WHERE status = $1`, StatusFailed); n != 1 {
		t.Errorf("failed mr_reviews rows = %d, want 1", n)
	}
}

func TestRunReviewWithoutAnEngine(t *testing.T) {
	h := newHarness(t, withDB)
	h.svc.eng = nil
	if _, err := h.svc.RunReview(t.Context(), h.reviewRequest(true), nil); err == nil {
		t.Fatal("RunReview without an engine must fail loudly")
	}
}

func TestFindingContent(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		body       string
		suggestion string
		want       string
	}{
		{"body only", " the connection leaks ", "", "the connection leaks"},
		{"with a suggestion", "the connection leaks", "defer c.Close()",
			"the connection leaks\n\n**Suggested change:**\n\ndefer c.Close()"},
		{"blank suggestion is not a section", "leak", "   ", "leak"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := findingContent(review.ValidatedFinding{Body: c.body, Suggestion: c.suggestion})
			if got != c.want {
				t.Errorf("findingContent = %q, want %q", got, c.want)
			}
		})
	}
}

func TestJSONOrEmptyNeverProducesSQLNull(t *testing.T) {
	t.Parallel()
	log := logger.ForTests(t)
	// The jsonb columns are NOT NULL, and the zero dbtypes.JSON is SQL NULL.
	for _, v := range []any{nil, (*llm.ReviewResponse)(nil), struct{}{}} {
		got := jsonOrEmpty(v, log)
		if len(got) == 0 || string(got) == "null" {
			t.Errorf("jsonOrEmpty(%#v) = %q, want a non-null payload", v, string(got))
		}
	}
}

func TestNormalizeFingerprint(t *testing.T) {
	t.Parallel()
	if got := normalizeFingerprint("  9F2C1AB3  "); got != "9f2c1ab3" {
		t.Errorf("normalizeFingerprint = %q, want %q", got, "9f2c1ab3")
	}
}

func TestProjectKey(t *testing.T) {
	t.Parallel()
	if got := projectKey(42, "backend/payments"); got != "42" {
		t.Errorf("projectKey with an id = %q, want %q", got, "42")
	}
	if got := projectKey(0, "/backend/payments/"); got != "backend%2Fpayments" {
		t.Errorf("projectKey with a path = %q, want %q", got, "backend%2Fpayments")
	}
}

func TestAIReviewEnabled(t *testing.T) {
	t.Parallel()
	off := testTeamConfig()
	off.AIReview = false
	svc := newService(Deps{GitLab: gitlab.NewFake()}, Config{Teams: []domain.Team{off}})

	if svc.aiReviewEnabled(testTeam) {
		t.Error("a team with ai_review off must not be reviewed")
	}
	if svc.aiReviewEnabled("PAYMENTS") != false {
		t.Error("team lookup must be case-insensitive")
	}
	if !svc.aiReviewEnabled("some-other-team") {
		t.Error("an explicitly requested review of an unconfigured repository must be allowed")
	}
}

func TestPipelineStatusOnlyReportsWhatGitLabConfirmed(t *testing.T) {
	t.Parallel()
	known := domain.MergeRequestSnapshot{Pipeline: domain.Pipeline{Status: "failed", Known: true}}
	if got := pipelineStatus(known); got != "failed" {
		t.Errorf("pipelineStatus = %q, want %q", got, "failed")
	}
	// A pipeline we could not see must not be reported as anything: the prompt
	// would otherwise read an absent field as a passing build.
	unknown := domain.MergeRequestSnapshot{Pipeline: domain.Pipeline{Status: "failed"}}
	if got := pipelineStatus(unknown); got != "" {
		t.Errorf("pipelineStatus = %q, want empty when head_pipeline was never returned", got)
	}
}
