package store_test

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sxwebdev/ai-reviewer/internal/dbtypes"
	"github.com/sxwebdev/ai-reviewer/internal/store"
	"github.com/sxwebdev/ai-reviewer/internal/store/storetest"
)

// A duplicate fingerprint is an expected outcome, not an error: it must not
// abort the transaction that persists the rest of the review (plan section 5.2).
func TestFindingInsertDuplicateDoesNotAbortTransaction(t *testing.T) {
	st, _ := storetest.Store(t)

	// A dry run: nobody is going to publish its findings, so the next review may
	// take them over. (A 'reviewed' owner may not — see
	// TestFindingInsertNeverStealsFromAPendingPublication.)
	first := createReview(t, st, reviewParams("dry_run"))
	if n := insertFinding(t, st, findingParams(first.ID, "fp-dup")); n != 1 {
		t.Fatalf("first insert affected %d rows, want 1", n)
	}

	second := reviewParams("reviewed")
	second.HeadSha = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	rev := createReview(t, st, second)

	err := st.RunInTx(t.Context(), func(tx pgx.Tx) error {
		// The conflicting row is still unpublished, so it is re-attached to this
		// review rather than skipped.
		n, err := st.Finding(store.WithTx(tx)).Insert(t.Context(), findingParams(rev.ID, "fp-dup"))
		if err != nil {
			return err
		}
		if n != 1 {
			t.Errorf("duplicate insert affected %d rows, want 1 (re-attached)", n)
		}
		// The transaction must still be usable — this is the assertion that
		// fails if the ON CONFLICT clause is ever dropped from the query.
		n, err = st.Finding(store.WithTx(tx)).Insert(t.Context(), findingParams(rev.ID, "fp-new"))
		if err != nil {
			return err
		}
		if n != 1 {
			t.Errorf("follow-up insert affected %d rows, want 1", n)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("transaction with a duplicate finding: %v", err)
	}

	rows, err := st.Finding().ListUnpublishedByReview(t.Context(), rev.ID)
	if err != nil {
		t.Fatalf("ListUnpublishedByReview: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d findings on the second review, want 2", len(rows))
	}
	// And exactly one row exists per fingerprint — re-attaching must not
	// duplicate.
	var total int64
	if err := st.Pool().QueryRow(t.Context(), `SELECT count(*) FROM mr_findings`).Scan(&total); err != nil {
		t.Fatalf("count findings: %v", err)
	}
	if total != 2 {
		t.Errorf("got %d finding rows in total, want 2", total)
	}
}

// The other half of the section 10.3 trap, reached through the persist path.
// A dry-run records the fingerprint with note_id NULL; the next review must be
// able to publish it, which it can only do if the row is re-attached to it.
// Under ON CONFLICT DO NOTHING the row stays bound to the dry-run review, which
// is never published, and the finding is lost forever — no later push frees it,
// because the fingerprint does not depend on the head SHA.
func TestFindingDryRunFindingIsPublishableByTheNextReview(t *testing.T) {
	st, _ := storetest.Store(t)

	dryRun := createReview(t, st, reviewParams("dry_run"))
	insertFinding(t, st, findingParams(dryRun.ID, "fp-stuck"))

	// Publishing is now enabled and the MR gets a new push.
	next := reviewParams("reviewed")
	next.HeadSha = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	rev := createReview(t, st, next)

	// The engine re-emits it, because the dedupe list only holds published ones.
	fps, err := st.Finding().ListPublishedFingerprints(t.Context(), testProject, testMR)
	if err != nil {
		t.Fatalf("ListPublishedFingerprints: %v", err)
	}
	if len(fps) != 0 {
		t.Fatalf("got %v, want the unpublished finding excluded from dedupe", fps)
	}
	if n := insertFinding(t, st, findingParams(rev.ID, "fp-stuck")); n != 1 {
		t.Fatalf("re-persisting the finding affected %d rows, want 1", n)
	}

	// This is the assertion that fails under DO NOTHING: the publish job for the
	// new review must actually see the finding.
	pending, err := st.Finding().ListUnpublishedByReview(t.Context(), rev.ID)
	if err != nil {
		t.Fatalf("ListUnpublishedByReview: %v", err)
	}
	if len(pending) != 1 || pending[0].Fingerprint != "fp-stuck" {
		t.Fatalf("got %d findings to publish for the new review, want fp-stuck", len(pending))
	}

	// The dry-run review must no longer own it.
	orphaned, err := st.Finding().ListUnpublishedByReview(t.Context(), dryRun.ID)
	if err != nil {
		t.Fatalf("ListUnpublishedByReview(dry run): %v", err)
	}
	if len(orphaned) != 0 {
		t.Errorf("the dry-run review still owns %d findings", len(orphaned))
	}

	if err := st.Finding().MarkPublished(t.Context(), 4242, pending[0].ID); err != nil {
		t.Fatalf("MarkPublished: %v", err)
	}
	fps, err = st.Finding().ListPublishedFingerprints(t.Context(), testProject, testMR)
	if err != nil {
		t.Fatalf("ListPublishedFingerprints after publication: %v", err)
	}
	if !slices.Equal(fps, []string{"fp-stuck"}) {
		t.Errorf("got %v, want [fp-stuck]", fps)
	}
}

// The counterweight to the re-attach: a row that already reached GitLab keeps
// its note_id, its published_at and its owning review. Clobbering any of them
// would either re-publish the comment or lose the audit trail.
func TestFindingInsertNeverTouchesAPublishedRow(t *testing.T) {
	st, _ := storetest.Store(t)

	first := createReview(t, st, reviewParams("succeeded"))
	insertFinding(t, st, findingParams(first.ID, "fp-live"))
	rows, err := st.Finding().ListUnpublishedByReview(t.Context(), first.ID)
	if err != nil {
		t.Fatalf("ListUnpublishedByReview: %v", err)
	}
	if err := st.Finding().MarkPublished(t.Context(), 777, rows[0].ID); err != nil {
		t.Fatalf("MarkPublished: %v", err)
	}

	second := reviewParams("reviewed")
	second.HeadSha = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	rev := createReview(t, st, second)

	p := findingParams(rev.ID, "fp-live")
	p.Body = "a different body from a later run"
	p.Severity = "low"
	if n := insertFinding(t, st, p); n != 0 {
		t.Errorf("insert over a published finding affected %d rows, want 0", n)
	}

	var (
		reviewID uuid.UUID
		noteID   int64
		body     string
		severity string
	)
	if err := st.Pool().QueryRow(t.Context(),
		`SELECT review_id, note_id, body, severity FROM mr_findings WHERE fingerprint = 'fp-live'`,
	).Scan(&reviewID, &noteID, &body, &severity); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if reviewID != first.ID {
		t.Errorf("published finding was re-attached to %s, want %s", reviewID, first.ID)
	}
	if noteID != 777 {
		t.Errorf("got note_id %d, want 777", noteID)
	}
	if body != "the rows are never closed" || severity != "high" {
		t.Errorf("published finding was rewritten: severity=%q body=%q", severity, body)
	}

	// It stays in the dedupe list, so the engine will not emit it again.
	fps, err := st.Finding().ListPublishedFingerprints(t.Context(), testProject, testMR)
	if err != nil {
		t.Fatalf("ListPublishedFingerprints: %v", err)
	}
	if !slices.Equal(fps, []string{"fp-live"}) {
		t.Errorf("got %v, want [fp-live]", fps)
	}
	// And no publish job will pick it up again.
	pending, err := st.Finding().ListUnpublishedByReview(t.Context(), rev.ID)
	if err != nil {
		t.Fatalf("ListUnpublishedByReview: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("got %d findings queued for re-publication, want 0", len(pending))
	}
}

// TestFindingInsertNeverStealsFromAPendingPublication is the other half of the
// re-attach rule, and the one that keeps publication and persistence from
// interleaving destructively.
//
// publish_review's uniqueness is ByArgs(review_id), so two publishers for two
// reviews of the SAME merge request run concurrently. If review B's persist
// transaction could move A's still-unposted finding while publish_review(A) is
// between its snapshot read and its POST, the result is either the same comment
// twice (both publishers hold the row) or — when B is a dry run — A setting
// 'succeeded' having posted none of its findings, because they were stolen into
// a review that will never publish. §10.4 says neither can happen.
func TestFindingInsertNeverStealsFromAPendingPublication(t *testing.T) {
	st, _ := storetest.Store(t)

	// A is persisted and its publication is pending.
	pendingReview := createReview(t, st, reviewParams("reviewed"))
	insertFinding(t, st, findingParams(pendingReview.ID, "fp-inflight"))

	// B — a dry run, the worst case: it would never publish what it took.
	second := reviewParams("dry_run")
	second.HeadSha = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	dry := createReview(t, st, second)

	if n := insertFinding(t, st, findingParams(dry.ID, "fp-inflight")); n != 0 {
		t.Errorf("stealing from a pending publication affected %d rows, want 0", n)
	}
	owner := findingOwner(t, st, "fp-inflight")
	if owner != pendingReview.ID {
		t.Fatalf("finding moved to %s, want it to stay with the publishing review %s", owner, pendingReview.ID)
	}
	// The publisher still sees it, which is what makes "succeeded with findings
	// on no merge request" unrepresentable.
	rows, err := st.Finding().ListUnpublishedByReview(t.Context(), pendingReview.ID)
	if err != nil {
		t.Fatalf("ListUnpublishedByReview: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("the publishing review has %d pending findings, want 1", len(rows))
	}

	// Once nobody is going to publish it there — the publication was abandoned —
	// the finding becomes reachable again, which is the §10.3 escape the
	// DO UPDATE exists for.
	if _, err := st.Pool().Exec(t.Context(),
		`UPDATE mr_reviews SET status = 'abandoned' WHERE id = $1`, pendingReview.ID); err != nil {
		t.Fatalf("abandon: %v", err)
	}
	if n := insertFinding(t, st, findingParams(dry.ID, "fp-inflight")); n != 1 {
		t.Errorf("re-attach from an abandoned review affected %d rows, want 1", n)
	}
	if owner := findingOwner(t, st, "fp-inflight"); owner != dry.ID {
		t.Errorf("finding is owned by %s, want the newer review %s", owner, dry.ID)
	}
}

func findingOwner(t *testing.T, st *store.Store, fingerprint string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := st.Pool().QueryRow(t.Context(),
		`SELECT review_id FROM mr_findings WHERE fingerprint = $1`, fingerprint).Scan(&id); err != nil {
		t.Fatalf("read finding owner: %v", err)
	}
	return id
}

// Re-attaching refreshes what the newer run learned, but only the fields the
// fingerprint does not already pin (file, category and title are inputs to it).
func TestFindingInsertRefreshesAnUnpublishedRow(t *testing.T) {
	st, _ := storetest.Store(t)

	first := createReview(t, st, reviewParams("dry_run"))
	insertFinding(t, st, findingParams(first.ID, "fp-refresh"))

	second := reviewParams("reviewed")
	second.HeadSha = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	rev := createReview(t, st, second)

	p := findingParams(rev.ID, "fp-refresh")
	p.Severity = "medium"
	p.Body = "re-stated after the rebase"
	// The spaces inside the string value are the part dbtypes.JSON promises to
	// preserve; jsonb normalizes structural whitespace on the way out, so the
	// comparison below is on the decoded value.
	p.PositionJson = dbtypes.JSON(`{"new_line":42,"side":"new  line"}`)
	p.Pass = "security"
	p.Verification = "refuted-demoted"
	if n := insertFinding(t, st, p); n != 1 {
		t.Fatalf("re-attach affected %d rows, want 1", n)
	}

	rows, err := st.Finding().ListUnpublishedByReview(t.Context(), rev.ID)
	if err != nil {
		t.Fatalf("ListUnpublishedByReview: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d findings, want 1", len(rows))
	}
	got := rows[0]
	if got.Severity != "medium" || got.Body != p.Body || got.Pass != "security" || got.Verification != "refuted-demoted" {
		t.Errorf("stale row after re-attach: %+v", got)
	}

	var position struct {
		NewLine int    `json:"new_line"`
		Side    string `json:"side"`
	}
	if err := json.Unmarshal(got.PositionJson, &position); err != nil {
		t.Fatalf("decode position %s: %v", got.PositionJson, err)
	}
	if position.NewLine != 42 {
		t.Errorf("got new_line %d, want the refreshed 42", position.NewLine)
	}
	if position.Side != "new  line" {
		t.Errorf("got side %q, want the double space preserved", position.Side)
	}
}

// Dedupe must mean "already hanging in GitLab", not "we once computed it"
// (plan section 10.3). Without the note_id IS NOT NULL condition a dry-run poisons
// the MR forever: the fingerprint does not depend on the head SHA, so no later
// push can rescue the finding.
func TestFindingListPublishedFingerprintsOnlyPublished(t *testing.T) {
	st, _ := storetest.Store(t)

	rev := createReview(t, st, reviewParams("reviewed"))
	insertFinding(t, st, findingParams(rev.ID, "fp-published"))
	insertFinding(t, st, findingParams(rev.ID, "fp-unpublished"))

	unpublished, err := st.Finding().ListUnpublishedByReview(t.Context(), rev.ID)
	if err != nil {
		t.Fatalf("ListUnpublishedByReview: %v", err)
	}
	if len(unpublished) != 2 {
		t.Fatalf("got %d unpublished findings, want 2", len(unpublished))
	}

	got, err := st.Finding().ListPublishedFingerprints(t.Context(), testProject, testMR)
	if err != nil {
		t.Fatalf("ListPublishedFingerprints: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %v, want no fingerprints before publication", got)
	}

	var published string
	for _, f := range unpublished {
		if f.Fingerprint == "fp-published" {
			published = f.Fingerprint
			if err := st.Finding().MarkPublished(t.Context(), 9001, f.ID); err != nil {
				t.Fatalf("MarkPublished: %v", err)
			}
		}
	}
	if published == "" {
		t.Fatal("fp-published missing from the unpublished list")
	}

	got, err = st.Finding().ListPublishedFingerprints(t.Context(), testProject, testMR)
	if err != nil {
		t.Fatalf("ListPublishedFingerprints: %v", err)
	}
	if !slices.Equal(got, []string{"fp-published"}) {
		t.Errorf("got %v, want [fp-published]", got)
	}

	// The publish job must now see only what is still missing.
	unpublished, err = st.Finding().ListUnpublishedByReview(t.Context(), rev.ID)
	if err != nil {
		t.Fatalf("ListUnpublishedByReview after publication: %v", err)
	}
	if len(unpublished) != 1 || unpublished[0].Fingerprint != "fp-unpublished" {
		t.Errorf("got %d findings left to publish, want only fp-unpublished", len(unpublished))
	}
}

func TestFindingMarkPublishedRecordsNoteAndTime(t *testing.T) {
	st, _ := storetest.Store(t)

	rev := createReview(t, st, reviewParams("reviewed"))
	insertFinding(t, st, findingParams(rev.ID, "fp-1"))

	rows, err := st.Finding().ListUnpublishedByReview(t.Context(), rev.ID)
	if err != nil {
		t.Fatalf("ListUnpublishedByReview: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d findings, want 1", len(rows))
	}
	if rows[0].NoteID.Valid || rows[0].PublishedAt.Valid {
		t.Fatalf("fresh finding already has note_id/published_at: %+v", rows[0])
	}

	if err := st.Finding().MarkPublished(t.Context(), 12345, rows[0].ID); err != nil {
		t.Fatalf("MarkPublished: %v", err)
	}

	var noteID int64
	var publishedAt any
	if err := st.Pool().QueryRow(t.Context(),
		`SELECT note_id, published_at FROM mr_findings WHERE id = $1`, rows[0].ID,
	).Scan(&noteID, &publishedAt); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if noteID != 12345 {
		t.Errorf("got note_id %d, want 12345", noteID)
	}
	if publishedAt == nil {
		t.Error("published_at is still NULL")
	}
}

// Deleting a review takes its findings with it, so a purged review cannot leave
// fingerprints behind that would silence the next run.
func TestFindingCascadesWithReview(t *testing.T) {
	st, _ := storetest.Store(t)

	rev := createReview(t, st, reviewParams("reviewed"))
	insertFinding(t, st, findingParams(rev.ID, "fp-cascade"))

	if _, err := st.Pool().Exec(t.Context(), `DELETE FROM mr_reviews WHERE id = $1`, rev.ID); err != nil {
		t.Fatalf("delete review: %v", err)
	}

	var n int64
	if err := st.Pool().QueryRow(t.Context(), `SELECT count(*) FROM mr_findings`).Scan(&n); err != nil {
		t.Fatalf("count findings: %v", err)
	}
	if n != 0 {
		t.Errorf("got %d orphaned findings, want 0", n)
	}
}
