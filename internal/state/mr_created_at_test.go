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

// TestUpsertMergeRequestPreservesCreatedAt proves created_at is write-once: a
// later partial upsert that omits it (created_at = 0, as the review flow does)
// must not clobber the stored value. Regression for the "opened time resets
// after review" bug.
func TestUpsertMergeRequestPreservesCreatedAt(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	const created = int64(1700000000000)

	id, err := db.UpsertMergeRequest(ctx, &MergeRequest{
		GitLabHost: "gitlab.test", ProjectID: 1, IID: 7, Title: "MR", HeadSHA: "abc", CreatedAt: created,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Re-upsert with a new head but CreatedAt left at zero (the review-flow path).
	if _, err := db.UpsertMergeRequest(ctx, &MergeRequest{
		GitLabHost: "gitlab.test", ProjectID: 1, IID: 7, Title: "MR", HeadSHA: "def", CreatedAt: 0,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := db.GetMergeRequest(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.CreatedAt != created {
		t.Errorf("CreatedAt = %d after zero-value re-upsert, want preserved %d", got.CreatedAt, created)
	}
	if got.HeadSHA != "def" {
		t.Errorf("HeadSHA = %q, want the re-upserted %q", got.HeadSHA, "def")
	}
}

// TestUpsertMergeRequestHealsZeroCreatedAt proves a row zeroed by a prior partial
// upsert is repaired when sync later supplies the real creation time — the
// COALESCE keeps write-once semantics without losing self-healing.
func TestUpsertMergeRequestHealsZeroCreatedAt(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	const created = int64(1700000000000)

	// First seen via the review flow with no creation time → stored as 0.
	id, err := db.UpsertMergeRequest(ctx, &MergeRequest{
		GitLabHost: "gitlab.test", ProjectID: 1, IID: 7, Title: "MR", HeadSHA: "abc", CreatedAt: 0,
	})
	if err != nil {
		t.Fatal(err)
	}

	// A later sync carries the real created_at → heals the zeroed value.
	if _, err := db.UpsertMergeRequest(ctx, &MergeRequest{
		GitLabHost: "gitlab.test", ProjectID: 1, IID: 7, Title: "MR", HeadSHA: "abc", CreatedAt: created,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := db.GetMergeRequest(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.CreatedAt != created {
		t.Errorf("CreatedAt = %d, want healed to %d", got.CreatedAt, created)
	}
}
