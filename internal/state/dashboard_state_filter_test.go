package state

import (
	"context"
	"testing"
)

// TestDashboardRowsFiltersClosed proves merged/closed MRs are excluded from the
// default (open-only) dashboard view and included when includeClosed is true,
// while open/locked/unknown-state MRs always appear. This is what keeps the
// default payload bounded as merged MRs accumulate in the table forever.
func TestDashboardRowsFiltersClosed(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	mrs := []*MergeRequest{
		{GitLabHost: "gitlab.test", ProjectID: 1, IID: 1, Title: "open", State: "opened", HeadSHA: "a"},
		{GitLabHost: "gitlab.test", ProjectID: 1, IID: 2, Title: "locked", State: "locked", HeadSHA: "b"},
		{GitLabHost: "gitlab.test", ProjectID: 1, IID: 3, Title: "unknown", State: "", HeadSHA: "c"},
		{GitLabHost: "gitlab.test", ProjectID: 1, IID: 4, Title: "merged", State: "merged", HeadSHA: "d"},
		{GitLabHost: "gitlab.test", ProjectID: 1, IID: 5, Title: "closed", State: "closed", HeadSHA: "e"},
	}
	for _, mr := range mrs {
		if _, err := db.UpsertMergeRequest(ctx, mr); err != nil {
			t.Fatal(err)
		}
	}

	// Default view: only the three open-ish MRs.
	openRows, err := db.DashboardRows(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(openRows) != 3 {
		t.Fatalf("open-only view = %d rows, want 3: %+v", len(openRows), openRows)
	}
	for _, r := range openRows {
		if r.State == "merged" || r.State == "closed" {
			t.Errorf("open-only view leaked a %q MR (iid %d)", r.State, r.IID)
		}
	}

	// Include-closed view: all five.
	allRows, err := db.DashboardRows(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(allRows) != 5 {
		t.Fatalf("all view = %d rows, want 5", len(allRows))
	}
}

// TestListOpenMergeRequests proves reconciliation's DB load is scoped to open
// MRs of the given host, so it never re-fetches settled (merged/closed) rows.
func TestListOpenMergeRequests(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	seed := []*MergeRequest{
		{GitLabHost: "gitlab.test", ProjectID: 1, IID: 1, State: "opened"},
		{GitLabHost: "gitlab.test", ProjectID: 1, IID: 2, State: "merged"},
		{GitLabHost: "gitlab.test", ProjectID: 1, IID: 3, State: ""},
		{GitLabHost: "other.test", ProjectID: 1, IID: 4, State: "opened"}, // different host
	}
	for _, mr := range seed {
		if _, err := db.UpsertMergeRequest(ctx, mr); err != nil {
			t.Fatal(err)
		}
	}

	got, err := db.ListOpenMergeRequests(ctx, "gitlab.test")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("ListOpenMergeRequests = %d, want 2 (open iids 1,3 on this host)", len(got))
	}
	for _, mr := range got {
		if mr.GitLabHost != "gitlab.test" {
			t.Errorf("leaked MR from host %q", mr.GitLabHost)
		}
		if mr.State == "merged" {
			t.Errorf("leaked merged MR iid %d", mr.IID)
		}
	}
}
