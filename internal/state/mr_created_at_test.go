package state

import (
	"context"
	"testing"
)

// TestMergeRequestCreatedAtRoundTrip proves the 0002 migration added created_at
// and that it flows through upsert → scan → dashboard.
func TestMergeRequestCreatedAtRoundTrip(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	const created = int64(1700000000000)

	id, err := db.UpsertMergeRequest(ctx, &MergeRequest{
		GitLabHost: "gitlab.test", ProjectID: 1, IID: 7, Title: "MR", HeadSHA: "abc", CreatedAt: created,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := db.GetMergeRequest(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.CreatedAt != created {
		t.Errorf("GetMergeRequest CreatedAt = %d, want %d", got.CreatedAt, created)
	}
	rows, err := db.DashboardRows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].CreatedAt != created {
		t.Fatalf("DashboardRows did not surface CreatedAt: %+v", rows)
	}
}
