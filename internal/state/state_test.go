package state

import (
	"path/filepath"
	"testing"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func TestMigrateAndFTS5(t *testing.T) {
	db := openTestDB(t)
	if err := db.FTS5Available(); err != nil {
		t.Fatalf("FTS5 should be available: %v", err)
	}
	// Migrate is idempotent.
	if err := db.Migrate(); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	db := openTestDB(t)
	ctx := t.Context()
	if _, ok, _ := db.GetSetting(ctx, "missing"); ok {
		t.Error("missing key should not exist")
	}
	if err := db.SetSetting(ctx, "k", "v1"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetSetting(ctx, "k", "v2"); err != nil {
		t.Fatal(err)
	}
	v, ok, err := db.GetSetting(ctx, "k")
	if err != nil || !ok || v != "v2" {
		t.Errorf("got (%q,%v,%v), want (v2,true,nil)", v, ok, err)
	}
}

func TestProjectAndMRUpsert(t *testing.T) {
	db := openTestDB(t)
	ctx := t.Context()

	pid, err := db.UpsertProject(ctx, &Project{
		GitLabHost: "https://gitlab.test", ProjectID: 42,
		PathWithNamespace: "group/repo", DefaultBranch: "main",
	})
	if err != nil || pid == 0 {
		t.Fatalf("upsert project: id=%d err=%v", pid, err)
	}
	// Idempotent upsert returns same id.
	pid2, _ := db.UpsertProject(ctx, &Project{GitLabHost: "https://gitlab.test", ProjectID: 42, PathWithNamespace: "group/repo2"})
	if pid != pid2 {
		t.Errorf("upsert changed id: %d != %d", pid, pid2)
	}
	p, err := db.GetProjectByGitLabID(ctx, "https://gitlab.test", 42)
	if err != nil || p.PathWithNamespace != "group/repo2" {
		t.Errorf("get project: %+v err=%v", p, err)
	}

	mrID, err := db.UpsertMergeRequest(ctx, &MergeRequest{
		GitLabHost: "https://gitlab.test", ProjectID: 42, IID: 7,
		Title: "Add feature", AuthorUsername: "alice", HeadSHA: "abc", State: "opened",
	})
	if err != nil || mrID == 0 {
		t.Fatalf("upsert mr: %v", err)
	}
	got, err := db.GetMergeRequestByIID(ctx, "https://gitlab.test", 42, 7)
	if err != nil || got.Title != "Add feature" || got.ID != mrID {
		t.Errorf("get mr: %+v err=%v", got, err)
	}
	list, err := db.ListMergeRequests(ctx)
	if err != nil || len(list) != 1 {
		t.Errorf("list mrs: n=%d err=%v", len(list), err)
	}
}

func TestReviewMemoryFTS(t *testing.T) {
	db := openTestDB(t)
	ctx := t.Context()

	items := []*ReviewMemory{
		{ID: "m1", Scope: ScopeProject, Type: MemRepoRule, Enabled: true, Source: SourceUser,
			Title: "Context propagation", Body: "Handlers must pass context to the DB layer."},
		{ID: "m2", Scope: ScopeProject, Type: MemTestPolicy, Enabled: true, Source: SourceUser,
			Title: "Integration tests", Body: "New endpoints require integration tests."},
		{ID: "m3", Scope: ScopeProject, Type: MemArchNote, Enabled: false, Source: SourceUser,
			Title: "Disabled rule", Body: "Do not call external APIs inside DB transactions."},
	}
	for _, m := range items {
		if err := db.UpsertReviewMemory(ctx, m); err != nil {
			t.Fatalf("upsert memory %s: %v", m.ID, err)
		}
	}

	all, err := db.ListReviewMemory(ctx)
	if err != nil || len(all) != 3 {
		t.Fatalf("list memory: n=%d err=%v", len(all), err)
	}

	hits, err := db.SearchReviewMemory(ctx, "context", 5)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 || hits[0].ID != "m1" {
		t.Errorf("search 'context' = %d hits, want m1", len(hits))
	}

	// Disabled item excluded even if it matches.
	hits, err = db.SearchReviewMemory(ctx, "transactions", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Errorf("disabled memory should be excluded, got %d", len(hits))
	}

	// Update keeps FTS in sync: re-title m1 so it no longer matches "context".
	items[0].Title = "Renamed"
	items[0].Body = "Unrelated body about widgets."
	if err := db.UpsertReviewMemory(ctx, items[0]); err != nil {
		t.Fatal(err)
	}
	hits, _ = db.SearchReviewMemory(ctx, "context", 5)
	if len(hits) != 0 {
		t.Errorf("stale FTS row: 'context' still matches after update, got %d", len(hits))
	}
}
