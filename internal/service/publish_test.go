package service

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/sxwebdev/ai-reviewer/internal/gitlab"
	"github.com/sxwebdev/ai-reviewer/internal/state"
)

// seedApprovedReview creates an MR, a review, and one approved finding with an
// inline position; returns the review id.
func seedApprovedReview(t *testing.T, db *state.DB) string {
	t.Helper()
	ctx := t.Context()
	mrID, err := db.UpsertMergeRequest(ctx, &state.MergeRequest{
		GitLabHost: "https://gitlab.test", ProjectID: 10, IID: 5, Title: "MR",
	})
	if err != nil {
		t.Fatal(err)
	}
	reviewID := uuid.NewString()
	if err := db.CreateReview(ctx, &state.Review{
		ID: reviewID, MRID: mrID, ProjectID: 10, MRIID: 5, Status: state.ReviewReady,
	}); err != nil {
		t.Fatal(err)
	}
	nl := int64(12)
	if err := db.InsertFinding(ctx, &state.Finding{
		ID: uuid.NewString(), ReviewID: reviewID, MRID: mrID, Severity: "high",
		Category: "correctness", FilePath: "main.go", NewLine: &nl, Title: "bug", Body: "fix it",
		Status:             state.FindingApproved,
		GitLabPositionJSON: `{"base_sha":"b","head_sha":"h","start_sha":"s","position_type":"text","old_path":"main.go","new_path":"main.go","new_line":12}`,
	}); err != nil {
		t.Fatal(err)
	}
	return reviewID
}

func auditCount(t *testing.T, db *state.DB, ctx context.Context, kind string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_events WHERE kind = ?`, kind).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPublishFlowRequiresConfirmation(t *testing.T) {
	ctx := t.Context()
	db := testDB(t)
	reviewID := seedApprovedReview(t, db)
	fake := gitlab.NewFake()
	pub := NewPublishService(fake, db, discardLogger())

	// Create drafts from approved findings.
	created, err := pub.CreateDrafts(ctx, reviewID)
	if err != nil {
		t.Fatal(err)
	}
	if created != 1 || len(fake.CreatedDrafts) != 1 {
		t.Fatalf("want 1 draft created, got created=%d recorded=%d", created, len(fake.CreatedDrafts))
	}
	// The inline position must have been forwarded to GitLab.
	if dp := fake.CreatedDrafts[0].Position; dp == nil || dp.NewLine == nil || *dp.NewLine != 12 {
		t.Errorf("draft note missing inline position: %+v", fake.CreatedDrafts[0].Position)
	}
	if auditCount(t, db, ctx, state.AuditCreateDraft) != 1 {
		t.Error("create_draft audit event missing")
	}

	// Wrong confirmation must NOT publish anything.
	if _, err := pub.PublishDrafts(ctx, reviewID, "yes do it"); err == nil {
		t.Fatal("expected error on wrong confirmation phrase")
	}
	if len(fake.PublishedDraftIDs) != 0 {
		t.Fatalf("nothing must publish without confirmation, got %d", len(fake.PublishedDraftIDs))
	}
	if auditCount(t, db, ctx, state.AuditPublish) != 0 {
		t.Error("publish audit event must not exist yet")
	}

	// Correct confirmation phrase publishes.
	published, err := pub.PublishDrafts(ctx, reviewID, ConfirmPhrase(1))
	if err != nil {
		t.Fatal(err)
	}
	if published != 1 || len(fake.PublishedDraftIDs) != 1 {
		t.Fatalf("want 1 published, got published=%d recorded=%d", published, len(fake.PublishedDraftIDs))
	}
	if auditCount(t, db, ctx, state.AuditPublish) != 1 {
		t.Error("publish audit event missing")
	}
}

func TestRejectSavesFalsePositiveMemory(t *testing.T) {
	ctx := t.Context()
	db := testDB(t)
	reviewID := seedApprovedReview(t, db)
	findings, _ := db.ListFindingsByReview(ctx, reviewID)
	if len(findings) == 0 {
		t.Fatal("no findings")
	}

	fs := NewFindingService(db)
	if err := fs.Reject(ctx, findings[0].ID, "not a real issue", true); err != nil {
		t.Fatal(err)
	}

	got, _ := db.GetFinding(ctx, findings[0].ID)
	if got.Status != state.FindingRejected {
		t.Errorf("status = %q, want rejected", got.Status)
	}
	mem, _ := db.ListReviewMemory(ctx)
	if len(mem) != 1 || mem[0].Type != state.MemFalsePositive {
		t.Errorf("false-positive memory not saved: %+v", mem)
	}
}
