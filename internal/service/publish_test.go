package service

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/tkcrm/mx/logger"

	"github.com/sxwebdev/ai-reviewer/internal/dbtypes"
	"github.com/sxwebdev/ai-reviewer/internal/gitlab"
)

// reviewWithFindings runs a real review so the rows under test are the ones the
// review path actually writes, then returns its id.
func (h *harness) reviewedMR(t *testing.T, titles ...string) uuid.UUID {
	t.Helper()
	seedGitLab(h.fake, testProject(), testMR(testMRIID))
	h.llm.Response = multiFindingResponse(titles...)

	out, err := h.svc.RunReview(t.Context(), h.reviewRequest(true), enqueueMarker(t, new(int), nil))
	if err != nil {
		t.Fatalf("RunReview: %v", err)
	}
	if out.Findings != len(titles) {
		t.Fatalf("seeded %d findings, want %d", out.Findings, len(titles))
	}
	// The review path performs no GitLab writes; publication starts from zero.
	h.fake.WriteLog = nil
	h.fake.CreatedNotes = nil
	h.fake.CreatedDiscussions = nil
	return out.ReviewID
}

func (h *harness) findingNoteIDs(t *testing.T, reviewID uuid.UUID) map[string]*int64 {
	t.Helper()
	rows, err := h.pool.Query(t.Context(),
		`SELECT title, note_id FROM mr_findings WHERE review_id = $1 ORDER BY created_at, id`, reviewID)
	if err != nil {
		t.Fatalf("read findings: %v", err)
	}
	defer rows.Close()
	out := map[string]*int64{}
	for rows.Next() {
		var (
			title string
			id    *int64
		)
		if err := rows.Scan(&title, &id); err != nil {
			t.Fatalf("scan finding: %v", err)
		}
		out[title] = id
	}
	return out
}

func (h *harness) reviewStatus(t *testing.T, reviewID uuid.UUID) string {
	t.Helper()
	var status string
	if err := h.pool.QueryRow(t.Context(), `SELECT status FROM mr_reviews WHERE id = $1`, reviewID).
		Scan(&status); err != nil {
		t.Fatalf("read review status: %v", err)
	}
	return status
}

// TestPublishReviewPostsTheSummaryMarkerLast is the §17 ordering test. The
// summary marker means "the whole review landed", so nothing may be posted
// after it — move the CreateMRNote call before the finding loop and this fails.
func TestPublishReviewPostsTheSummaryMarkerLast(t *testing.T) {
	h := newHarness(t, withDB)
	reviewID := h.reviewedMR(t, "leaks a connection", "unchecked error", "missing timeout")

	published, err := h.svc.PublishReview(t.Context(), reviewID)
	if err != nil {
		t.Fatalf("PublishReview: %v", err)
	}
	if published != 3 {
		t.Errorf("published = %d, want 3", published)
	}

	want := []string{"discussion", "discussion", "discussion", "note"}
	if !slices.Equal(h.fake.WriteLog, want) {
		t.Fatalf("write order = %v, want %v (the summary note must be last)", h.fake.WriteLog, want)
	}
	if len(h.fake.CreatedNotes) != 1 {
		t.Fatalf("summary notes = %d, want 1", len(h.fake.CreatedNotes))
	}
	marker, ok := gitlab.ParseReviewMarker(h.fake.CreatedNotes[0])
	if !ok {
		t.Fatalf("the summary note carries no review marker:\n%s", h.fake.CreatedNotes[0])
	}
	if marker.HeadSHA != testHeadSHA || marker.Findings != 3 {
		t.Errorf("marker = %+v, want head %q and 3 findings", marker, testHeadSHA)
	}
	if h.reviewStatus(t, reviewID) != StatusSucceeded {
		t.Errorf("status = %q, want %q only after the summary landed",
			h.reviewStatus(t, reviewID), StatusSucceeded)
	}
	// Every finding carries its own fingerprint marker so a later run can
	// recognise it without the database.
	for _, body := range h.fake.CreatedDiscussions {
		if len(gitlab.ParseFindingMarkers(body)) != 1 {
			t.Errorf("published finding carries no fingerprint marker:\n%s", body)
		}
	}
}

// TestPublishReviewIsIdempotentAfterACrashMidway covers the §17 bullet: only
// findings with note_id IS NULL are posted, so a retry sends the rest and never
// duplicates what already landed.
func TestPublishReviewIsIdempotentAfterACrashMidway(t *testing.T) {
	h := newHarness(t, withDB)
	reviewID := h.reviewedMR(t, "leaks a connection", "unchecked error")

	// Simulate a crash after the first POST: one finding recorded, the review
	// still 'reviewed', no summary.
	before := h.findingNoteIDs(t, reviewID)
	var firstID uuid.UUID
	if err := h.pool.QueryRow(t.Context(),
		`SELECT id FROM mr_findings WHERE review_id = $1 AND title = $2`, reviewID, "leaks a connection").
		Scan(&firstID); err != nil {
		t.Fatalf("read finding: %v", err)
	}
	if before["leaks a connection"] != nil {
		t.Fatal("fixture is wrong: the finding must start unpublished")
	}
	if err := h.st.Finding().MarkPublished(t.Context(), 777, firstID); err != nil {
		t.Fatalf("MarkPublished: %v", err)
	}

	published, err := h.svc.PublishReview(t.Context(), reviewID)
	if err != nil {
		t.Fatalf("PublishReview: %v", err)
	}
	if published != 1 {
		t.Errorf("published = %d, want only the finding that was still missing", published)
	}
	if want := []string{"discussion", "note"}; !slices.Equal(h.fake.WriteLog, want) {
		t.Errorf("write order = %v, want %v", h.fake.WriteLog, want)
	}
	after := h.findingNoteIDs(t, reviewID)
	for title, id := range after {
		if id == nil {
			t.Errorf("finding %q is still unpublished", title)
		}
	}
	if got := after["leaks a connection"]; got == nil || *got != 777 {
		t.Errorf("the already-published finding was reposted (note id %v, want 777)", got)
	}
}

func TestPublishReviewSecondCallIsANoOp(t *testing.T) {
	h := newHarness(t, withDB)
	reviewID := h.reviewedMR(t, "leaks a connection")

	if _, err := h.svc.PublishReview(t.Context(), reviewID); err != nil {
		t.Fatalf("first PublishReview: %v", err)
	}
	writes := len(h.fake.WriteLog)

	published, err := h.svc.PublishReview(t.Context(), reviewID)
	if err != nil {
		t.Fatalf("second PublishReview: %v", err)
	}
	if published != 0 {
		t.Errorf("published = %d on a succeeded review, want 0", published)
	}
	if len(h.fake.WriteLog) != writes {
		t.Errorf("the second call wrote to GitLab again: %v", h.fake.WriteLog)
	}
}

// TestPublishReviewSkipsTheSummaryWhenItsMarkerIsAlreadyOnTheMR closes the
// window between the summary POST and the status update: without the check a
// retry would post a second summary for the same head SHA.
func TestPublishReviewSkipsTheSummaryWhenItsMarkerIsAlreadyOnTheMR(t *testing.T) {
	h := newHarness(t, withDB)
	proj := testProject()
	reviewID := h.reviewedMR(t, "leaks a connection")

	existing := gitlab.ReviewMarker{
		ProjectID: testProjectID, MRIID: testMRIID, HeadSHA: testHeadSHA, Findings: 1,
	}.Render()
	setDiscussions(h.fake, proj, testMRIID, []gitlab.Discussion{{
		ID: "summary", Notes: []gitlab.Note{note(21, "🤖 AI review\n\n"+existing)},
	}})

	if _, err := h.svc.PublishReview(t.Context(), reviewID); err != nil {
		t.Fatalf("PublishReview: %v", err)
	}
	if len(h.fake.CreatedNotes) != 0 {
		t.Errorf("a second summary was posted for the same head sha: %v", h.fake.CreatedNotes)
	}
	if h.reviewStatus(t, reviewID) != StatusSucceeded {
		t.Error("the review must still reach 'succeeded' — the summary is on the MR")
	}
}

// TestPublishReviewAdoptsFindingsAlreadyOnTheMR is the database-loss case:
// markers in GitLab are the secondary source of truth, so a finding whose
// marker is already posted is recorded, not reposted.
func TestPublishReviewAdoptsFindingsAlreadyOnTheMR(t *testing.T) {
	h := newHarness(t, withDB)
	proj := testProject()
	reviewID := h.reviewedMR(t, "leaks a connection")

	setDiscussions(h.fake, proj, testMRIID, []gitlab.Discussion{{
		ID: "d1",
		Notes: []gitlab.Note{{
			ID:   909,
			Body: "an earlier posting\n\n" + gitlab.RenderFindingMarker(fingerprintOf("leaks a connection")),
		}},
	}})

	published, err := h.svc.PublishReview(t.Context(), reviewID)
	if err != nil {
		t.Fatalf("PublishReview: %v", err)
	}
	if published != 0 {
		t.Errorf("published = %d, want 0: the finding is already on the MR", published)
	}
	if len(h.fake.CreatedDiscussions) != 0 {
		t.Errorf("the finding was reposted: %v", h.fake.CreatedDiscussions)
	}
	if got := h.findingNoteIDs(t, reviewID)["leaks a connection"]; got == nil || *got != 909 {
		t.Errorf("adopted note id = %v, want the note already on the MR (909)", got)
	}
}

func TestPublishReviewNeverPublishesADryRun(t *testing.T) {
	h := newHarness(t, withDB, withLLM(reviewResponse("leaks a connection")))
	seedGitLab(h.fake, testProject(), testMR(testMRIID))

	out, err := h.svc.RunReview(t.Context(), h.reviewRequest(false), nil)
	if err != nil {
		t.Fatalf("RunReview: %v", err)
	}

	published, err := h.svc.PublishReview(t.Context(), out.ReviewID)
	if err != nil {
		t.Fatalf("PublishReview: %v", err)
	}
	if published != 0 || len(h.fake.WriteLog) != 0 {
		t.Errorf("a dry run reached GitLab: published=%d writes=%v", published, h.fake.WriteLog)
	}
	if h.reviewStatus(t, out.ReviewID) != StatusDryRun {
		t.Error("a dry run must stay a dry run")
	}
}

// TestPublishReviewStopsAtTheFirstFailure keeps the summary marker honest: it
// must not be posted while findings are still missing, and the retry has to
// resume from where the failure happened.
func TestPublishReviewStopsAtTheFirstFailure(t *testing.T) {
	h := newHarness(t, withDB)
	reviewID := h.reviewedMR(t, "leaks a connection", "unchecked error")

	boom := errors.New("gitlab is down")
	failing := &discussionFailure{countingGitLab: h.gl, failAfter: 1, err: boom}
	h.svc.gl = failing

	published, err := h.svc.PublishReview(t.Context(), reviewID)
	if !errors.Is(err, boom) {
		t.Fatalf("PublishReview error = %v, want it to wrap %v", err, boom)
	}
	if published != 1 {
		t.Errorf("published = %d, want the one that succeeded before the failure", published)
	}
	if slices.Contains(h.fake.WriteLog, "note") {
		t.Error("the summary marker was posted while findings were still missing")
	}
	if h.reviewStatus(t, reviewID) != StatusReviewed {
		t.Errorf("status = %q, want it to stay %q so the retry finishes the job",
			h.reviewStatus(t, reviewID), StatusReviewed)
	}

	// The retry sends only what is missing.
	h.svc.gl = h.gl
	published, err = h.svc.PublishReview(t.Context(), reviewID)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if published != 1 {
		t.Errorf("retry published = %d, want 1", published)
	}
	if h.reviewStatus(t, reviewID) != StatusSucceeded {
		t.Error("the retry must finish the review")
	}
}

// TestPublishReviewNeverSucceedsWithUnpublishedFindings is §10.4's "no false
// success" as an assertion rather than an assumption.
//
// 'succeeded' is a claim about GitLab's state, and the only evidence for it is
// that every finding this review owns carries a note id. The interleaving that
// breaks it is real: publish_review's uniqueness is ByArgs(review_id), so two
// publishers for two reviews of the same MR run concurrently, and one of them
// re-attaching a finding mid-publication would leave the other marking a review
// succeeded having posted none of it.
func TestPublishReviewNeverSucceedsWithUnpublishedFindings(t *testing.T) {
	h := newHarness(t, withDB)
	reviewID := h.reviewedMR(t, "leaks a connection")

	// A concurrent persist attaches a finding to this review after its pending
	// list was snapshotted. Only the re-read before the summary can see it.
	// (The DO UPDATE guard is what keeps the reverse — a finding being taken
	// AWAY — from happening; see
	// store.TestFindingInsertNeverStealsFromAPendingPublication.)
	h.svc.gl = &findingSmuggler{countingGitLab: h.gl, h: h, reviewID: reviewID}

	_, err := h.svc.PublishReview(t.Context(), reviewID)
	if err == nil {
		t.Fatal("PublishReview succeeded with a finding that is on no merge request")
	}
	if !strings.Contains(err.Error(), "unpublished finding") {
		t.Errorf("error = %v, want it to name the unpublished findings", err)
	}
	if slices.Contains(h.fake.WriteLog, "note") {
		t.Error("the summary marker was posted while a finding was still missing")
	}
	if got := h.reviewStatus(t, reviewID); got != StatusReviewed {
		t.Errorf("status = %q, want it to stay %q", got, StatusReviewed)
	}
}

// findingSmuggler attaches an extra unpublished finding to the review while the
// publication loop is already past its snapshot — what a concurrent persist
// transaction does.
type findingSmuggler struct {
	*countingGitLab
	h        *harness
	reviewID uuid.UUID
	done     bool
}

func (f *findingSmuggler) CreateDiscussion(ctx context.Context, pk string, iid int64, body string, pos *gitlab.Position) (*gitlab.Discussion, error) {
	d, err := f.countingGitLab.CreateDiscussion(ctx, pk, iid, body, pos)
	if err != nil || f.done {
		return d, err
	}
	f.done = true
	if _, err := f.h.pool.Exec(ctx,
		`INSERT INTO mr_findings (review_id, project_id, mr_iid, fingerprint, severity, category, file_path, title, body)
		 VALUES ($1, $2, $3, 'fp-smuggled', 'high', 'correctness', 'main.go', 'smuggled in', 'body')`,
		f.reviewID, testProjectID, testMRIID); err != nil {
		return nil, err
	}
	return d, nil
}

// TestPublishReviewStopsWhenTheNoteIDCannotBeRecorded is the other half of the
// same invariant.
//
// A swallowed markPublished error left the row at note_id NULL forever:
// MarkSucceeded then short-circuited every later PublishReview, the finding was
// invisible to ListPublishedFingerprints, and the next review reposted a comment
// already on the merge request. Aborting is safe because the note carries a
// fingerprint marker — the retry adopts it instead of posting a second one,
// which is what the second half of this test checks.
func TestPublishReviewStopsWhenTheNoteIDCannotBeRecorded(t *testing.T) {
	h := newHarness(t, withDB)
	reviewID := h.reviewedMR(t, "leaks a connection", "unchecked error")

	// A constraint that only the note_id write can violate. The cleanup is
	// registered immediately: this is DDL, and storetest's per-test truncate
	// does not undo DDL — a constraint left behind by a failing assertion would
	// poison every later test in the binary, and the next run of it too.
	dropConstraint := func() {
		if _, err := h.pool.Exec(context.WithoutCancel(t.Context()),
			`ALTER TABLE mr_findings DROP CONSTRAINT IF EXISTS no_note_ids`); err != nil {
			t.Errorf("drop constraint: %v", err)
		}
	}
	if _, err := h.pool.Exec(t.Context(),
		`ALTER TABLE mr_findings ADD CONSTRAINT no_note_ids CHECK (note_id IS NULL)`); err != nil {
		t.Fatalf("add constraint: %v", err)
	}
	t.Cleanup(dropConstraint)

	if _, err := h.svc.PublishReview(t.Context(), reviewID); err == nil {
		t.Fatal("PublishReview ignored a failure to record the note id")
	}
	if got := h.reviewStatus(t, reviewID); got != StatusReviewed {
		t.Errorf("status = %q, want it to stay %q so the retry can finish", got, StatusReviewed)
	}
	if slices.Contains(h.fake.WriteLog, "note") {
		t.Error("the summary marker was posted before the findings were recorded")
	}
	// One comment reached GitLab and the loop stopped there.
	if n := len(h.fake.CreatedDiscussions); n != 1 {
		t.Fatalf("created discussions = %d, want the loop to stop at the first failure", n)
	}

	// The retry adopts the comment already on the MR rather than posting a
	// second one — the reason aborting is the right response. The fake does not
	// echo its own writes back through ListMRDiscussions, so the comment GitLab
	// would now be serving is installed explicitly.
	setDiscussions(h.fake, testProject(), testMRIID, []gitlab.Discussion{{
		ID:    "d1",
		Notes: []gitlab.Note{{ID: 909, Body: h.fake.CreatedDiscussions[0]}},
	}})
	dropConstraint()
	published, err := h.svc.PublishReview(t.Context(), reviewID)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if published != 1 {
		t.Errorf("retry published = %d, want only the finding that was still missing", published)
	}
	if n := len(h.fake.CreatedDiscussions); n != 2 {
		t.Errorf("created discussions = %d, want 2 in total (the first was adopted, not reposted)", n)
	}
	if got := h.findingNoteIDs(t, reviewID)["leaks a connection"]; got == nil || *got != 909 {
		t.Errorf("adopted note id = %v, want the note already on the MR (909)", got)
	}
	if got := h.reviewStatus(t, reviewID); got != StatusSucceeded {
		t.Errorf("status = %q, want the retry to finish the review", got)
	}
}

// TestPublishReviewAbandonsWhenGitLabRefusesDeterministically closes the loop
// §6.5 solved for reviews and left open for publications: an MR that was
// deleted, a project that was archived or a token that lost its api scope
// answers 4xx forever, so the job burns ten attempts and the §6.3 sweep
// re-enqueues it on every scan — through a one- or two-worker queue that then
// starves the publications which could still land.
func TestPublishReviewAbandonsWhenGitLabRefusesDeterministically(t *testing.T) {
	h := newHarness(t, withDB)
	reviewID := h.reviewedMR(t, "leaks a connection")

	gone := &gitlab.APIError{Status: 404, Body: "404 Not found", Method: "POST", Path: "7"}
	h.svc.gl = &discussionFailure{countingGitLab: h.gl, failAfter: 0, err: gone}

	published, err := h.svc.PublishReview(t.Context(), reviewID)
	if err != nil {
		t.Fatalf("PublishReview error = %v, want the job to succeed having given up", err)
	}
	if published != 0 {
		t.Errorf("published = %d, want 0", published)
	}
	if got := h.reviewStatus(t, reviewID); got != StatusAbandoned {
		t.Errorf("status = %q, want %q so the sweep stops re-enqueuing it", got, StatusAbandoned)
	}
	// A second call is a no-op, and the row keeps the SHA out of re-review while
	// contributing nothing to the §6.5 ladder.
	if _, err := h.svc.PublishReview(t.Context(), reviewID); err != nil {
		t.Errorf("publishing an abandoned review = %v, want a no-op", err)
	}
	if n := h.count(t, `SELECT count(*) FROM mr_reviews WHERE status = $1`, StatusFailed); n != 0 {
		t.Errorf("failed rows = %d, want 0: GitLab refusing is not the merge request's fault", n)
	}
}

// A transient failure must NOT be abandoned — the retry is the whole point.
func TestPublishReviewKeepsRetryingATransientFailure(t *testing.T) {
	h := newHarness(t, withDB)
	reviewID := h.reviewedMR(t, "leaks a connection")

	h.svc.gl = &discussionFailure{
		countingGitLab: h.gl, failAfter: 0,
		err: &gitlab.APIError{Status: 503, Body: "gateway", Method: "POST", Path: "7"},
	}
	if _, err := h.svc.PublishReview(t.Context(), reviewID); err == nil {
		t.Fatal("a 5xx must reach the job so it retries")
	}
	if got := h.reviewStatus(t, reviewID); got != StatusReviewed {
		t.Errorf("status = %q, want %q: a 5xx is not GitLab's final answer", got, StatusReviewed)
	}
}

// discussionFailure lets the first N discussion posts through and fails the
// rest.
type discussionFailure struct {
	*countingGitLab
	failAfter int
	err       error
	seen      int
}

func (d *discussionFailure) CreateDiscussion(ctx context.Context, pk string, iid int64, body string, pos *gitlab.Position) (*gitlab.Discussion, error) {
	d.seen++
	if d.seen > d.failAfter {
		return nil, d.err
	}
	return d.countingGitLab.CreateDiscussion(ctx, pk, iid, body, pos)
}

// TestPublishReviewFallsBackToAnOverviewCommentOnARejectedPosition prevents the
// worst publication loop: GitLab deterministically refuses a position it cannot
// resolve, so retrying the same request forever would exhaust the job's
// attempts and then be re-enqueued by the stale-review safety net for good.
func TestPublishReviewFallsBackToAnOverviewCommentOnARejectedPosition(t *testing.T) {
	h := newHarness(t, withDB)
	reviewID := h.reviewedMR(t, "leaks a connection")

	rejecting := &positionRejecting{countingGitLab: h.gl}
	h.svc.gl = rejecting

	published, err := h.svc.PublishReview(t.Context(), reviewID)
	if err != nil {
		t.Fatalf("PublishReview: %v", err)
	}
	if published != 1 {
		t.Errorf("published = %d, want the finding to land without its line anchor", published)
	}
	if rejecting.positioned != 1 || rejecting.unpositioned != 1 {
		t.Errorf("positioned=%d unpositioned=%d, want one attempt of each",
			rejecting.positioned, rejecting.unpositioned)
	}
	if h.reviewStatus(t, reviewID) != StatusSucceeded {
		t.Error("the review must complete")
	}
}

type positionRejecting struct {
	*countingGitLab
	positioned   int
	unpositioned int
}

func (p *positionRejecting) CreateDiscussion(ctx context.Context, pk string, iid int64, body string, pos *gitlab.Position) (*gitlab.Discussion, error) {
	if pos != nil {
		p.positioned++
		return nil, &gitlab.APIError{Status: 400, Body: "line_code is invalid", Method: "POST", Path: pk}
	}
	p.unpositioned++
	return p.countingGitLab.CreateDiscussion(ctx, pk, iid, body, nil)
}

func TestPositionRejected(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"bad request", &gitlab.APIError{Status: 400}, true},
		{"unprocessable", &gitlab.APIError{Status: 422}, true},
		{"rate limited is transient", &gitlab.APIError{Status: 429}, false},
		{"server error is transient", &gitlab.APIError{Status: 502}, false},
		{"transport failure", errors.New("dial tcp: connection refused"), false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := positionRejected(c.err); got != c.want {
				t.Errorf("positionRejected(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

func TestDecodePosition(t *testing.T) {
	t.Parallel()
	log := logger.ForTests(t)
	cases := []struct {
		name string
		raw  string
		want bool // a position is expected
	}{
		{"empty object", "{}", false},
		{"empty", "", false},
		{"null", "null", false},
		{"unreadable", "{not json", false},
		{"no paths", `{"base_sha":"a"}`, false},
		{"inline", `{"new_path":"main.go","new_line":3}`, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := decodePosition(dbtypes.JSON(c.raw), log)
			if (got != nil) != c.want {
				t.Errorf("decodePosition(%q) = %v, want position=%v", c.raw, got, c.want)
			}
		})
	}
}

func TestFindingNotesByFingerprintKeepsTheOldestNote(t *testing.T) {
	t.Parallel()
	fp := "9f2c1ab3"
	got := findingNotesByFingerprint([]gitlab.Discussion{
		{ID: "a", Notes: []gitlab.Note{{ID: 1, Body: gitlab.RenderFindingMarker(fp)}}},
		{ID: "b", Notes: []gitlab.Note{{ID: 2, Body: gitlab.RenderFindingMarker(fp)}}},
	})
	if got[fp] != 1 {
		t.Errorf("adopted note = %d, want the original posting (1)", got[fp])
	}
}

func TestRenderFindingCarriesTheHeaderAndMarker(t *testing.T) {
	t.Parallel()
	body := renderFinding(&mrFindingFixture)
	if !strings.HasPrefix(body, "**[high/correctness] leaks a connection**") {
		t.Errorf("finding body does not start with its header:\n%s", body)
	}
	if fps := gitlab.ParseFindingMarkers(body); len(fps) != 1 || fps[0] != "9f2c1ab3" {
		t.Errorf("finding markers = %v, want the fingerprint", fps)
	}
}
