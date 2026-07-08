package state

import (
	"context"
	"testing"
)

// TestDashboardRowsReviewedAt proves DashboardRows surfaces the creation time
// of the most recent review (0 while unreviewed) so the UI can sort MRs by
// review recency.
func TestDashboardRowsReviewedAt(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	id, err := db.UpsertMergeRequest(ctx, &MergeRequest{
		GitLabHost: "gitlab.test", ProjectID: 1, IID: 7, Title: "MR", HeadSHA: "abc",
	})
	if err != nil {
		t.Fatal(err)
	}

	rows, err := db.DashboardRows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ReviewedAt != 0 {
		t.Fatalf("unreviewed MR must surface ReviewedAt 0: %+v", rows)
	}

	const older, newer = int64(1700000001000), int64(1700000002000)
	for _, r := range []*Review{
		{ID: "rev-old", MRID: id, Status: "ready", HeadSHA: "abc", CreatedAt: older},
		{ID: "rev-new", MRID: id, Status: "ready", HeadSHA: "abc", CreatedAt: newer},
	} {
		if err := db.CreateReview(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	rows, err = db.DashboardRows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ReviewedAt != newer {
		t.Fatalf("ReviewedAt = %d, want the latest review's %d", rows[0].ReviewedAt, newer)
	}
}
