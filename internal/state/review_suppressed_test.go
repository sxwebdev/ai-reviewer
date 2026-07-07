package state

import (
	"context"
	"testing"
)

// TestReviewSuppressedJSONRoundTrip proves the 0007 migration added
// suppressed_json and that it flows through CreateReview → scanReview.
func TestReviewSuppressedJSONRoundTrip(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	mrID, err := db.UpsertMergeRequest(ctx, &MergeRequest{GitLabHost: "h", ProjectID: 1, IID: 1, HeadSHA: "sha1"})
	if err != nil {
		t.Fatal(err)
	}
	const blob = `[{"title":"low nit","stage":"threshold","severity":"low"}]`
	if err := db.CreateReview(ctx, &Review{
		ID: "rev1", MRID: mrID, ProjectID: 1, MRIID: 1, HeadSHA: "sha1",
		Status: ReviewReady, SuppressedJSON: blob,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetReview(ctx, "rev1")
	if err != nil {
		t.Fatal(err)
	}
	if got.SuppressedJSON != blob {
		t.Errorf("SuppressedJSON = %q, want %q", got.SuppressedJSON, blob)
	}
}
