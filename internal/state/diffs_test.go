package state

import (
	"context"
	"testing"
)

// TestMRDiffRoundTrip proves diffs persist, upsert idempotently, preserve flags,
// and that ListMRDiffFiles sorts vendored files last and scopes by head sha.
func TestMRDiffRoundTrip(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	mrID, err := db.UpsertMergeRequest(ctx, &MergeRequest{GitLabHost: "h", ProjectID: 1, IID: 1, HeadSHA: "sha1"})
	if err != nil {
		t.Fatal(err)
	}

	for _, d := range []*MRDiff{
		{MRID: mrID, HeadSHA: "sha1", NewPath: "main.go", OldPath: "main.go", Diff: "orig"},
		{MRID: mrID, HeadSHA: "sha1", NewPath: "vendor/x.go", OldPath: "vendor/x.go", Diff: "v", IsVendored: true},
		{MRID: mrID, HeadSHA: "sha1", NewPath: "logo.png", OldPath: "logo.png", Diff: "Binary files differ", IsBinary: true, NewFile: true},
	} {
		if err := db.UpsertMRDiff(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	// Re-upsert main.go with a new body — must update in place, not duplicate.
	if err := db.UpsertMRDiff(ctx, &MRDiff{MRID: mrID, HeadSHA: "sha1", NewPath: "main.go", OldPath: "main.go", Diff: "updated"}); err != nil {
		t.Fatal(err)
	}

	files, err := db.ListMRDiffFiles(ctx, mrID, "sha1")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 {
		t.Fatalf("want 3 files, got %d", len(files))
	}
	if last := files[len(files)-1]; last.NewPath != "vendor/x.go" || !last.IsVendored {
		t.Errorf("vendored file should sort last, got %q", last.NewPath)
	}
	for _, f := range files {
		switch f.NewPath {
		case "main.go":
			if f.Diff != "updated" {
				t.Errorf("upsert did not update main.go diff: %q", f.Diff)
			}
		case "logo.png":
			if !f.IsBinary || !f.NewFile {
				t.Errorf("logo.png lost binary/new flags: %+v", f)
			}
		}
	}

	empty, err := db.ListMRDiffFiles(ctx, mrID, "unknown-sha")
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Errorf("want empty for unknown head, got %d", len(empty))
	}
}

// TestFindingEditedAt proves SetFindingBody stamps edited_at (migration 0003).
func TestFindingEditedAt(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	mrID, err := db.UpsertMergeRequest(ctx, &MergeRequest{GitLabHost: "h", ProjectID: 1, IID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateReview(ctx, &Review{ID: "rv", MRID: mrID}); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertFinding(ctx, &Finding{ID: "f1", ReviewID: "rv", MRID: mrID, Severity: "low", Title: "t", Body: "b", Fingerprint: "fp", Status: FindingProposed}); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetFinding(ctx, "f1")
	if err != nil {
		t.Fatal(err)
	}
	if got.EditedAt != 0 {
		t.Errorf("new finding EditedAt = %d, want 0", got.EditedAt)
	}
	if err := db.SetFindingBody(ctx, "f1", "edited body"); err != nil {
		t.Fatal(err)
	}
	got, err = db.GetFinding(ctx, "f1")
	if err != nil {
		t.Fatal(err)
	}
	if got.EditedAt == 0 {
		t.Error("SetFindingBody should stamp EditedAt")
	}
	if got.Body != "edited body" {
		t.Errorf("body not updated: %q", got.Body)
	}
}
