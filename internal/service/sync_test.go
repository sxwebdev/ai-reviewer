package service

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/sxwebdev/ai-reviewer/internal/gitlab"
	"github.com/sxwebdev/ai-reviewer/internal/state"
)

func testDB(t *testing.T) *state.DB {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	return db
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestSyncAssignedMRs(t *testing.T) {
	ctx := t.Context()
	db := testDB(t)

	fake := gitlab.NewFake()
	fake.Projects["10"] = &gitlab.Project{ID: 10, PathWithNamespace: "group/repo", DefaultBranch: "main"}
	fake.ReviewerMRs = []gitlab.MergeRequest{
		{IID: 1, ProjectID: 10, Title: "First", Author: gitlab.User{Username: "bob"},
			SourceBranch: "feat", TargetBranch: "main", State: "opened", SHA: "sha1",
			DiffRefs:  gitlab.DiffRefs{BaseSHA: "base1", HeadSHA: "sha1", StartSHA: "start1"},
			UpdatedAt: "2026-06-01T10:00:00Z"},
		{IID: 2, ProjectID: 10, Title: "Second", Author: gitlab.User{Username: "carol"},
			State: "opened", SHA: "sha2"},
	}

	svc := NewSyncService(fake, db, "https://gitlab.test", discardLogger())
	res, err := svc.SyncAssignedMRs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 2 || res.Tracked != 2 {
		t.Fatalf("result = %+v, want total=2 tracked=2", res)
	}

	// Project was upserted.
	p, err := db.GetProjectByGitLabID(ctx, "https://gitlab.test", 10)
	if err != nil || p.PathWithNamespace != "group/repo" {
		t.Errorf("project not tracked: %+v err=%v", p, err)
	}

	// MRs were upserted with diff refs.
	mr, err := db.GetMergeRequestByIID(ctx, "https://gitlab.test", 10, 1)
	if err != nil {
		t.Fatal(err)
	}
	if mr.Title != "First" || mr.HeadSHA != "sha1" || mr.BaseSHA != "base1" || mr.AuthorUsername != "bob" {
		t.Errorf("mr fields wrong: %+v", mr)
	}
	if mr.UpdatedAt == 0 {
		t.Error("updated_at should be parsed")
	}

	list, err := db.ListMergeRequests(ctx)
	if err != nil || len(list) != 2 {
		t.Errorf("list = %d, want 2 (err=%v)", len(list), err)
	}

	// Re-sync is idempotent (upsert, no duplicates).
	if _, err := svc.SyncAssignedMRs(ctx); err != nil {
		t.Fatal(err)
	}
	list, _ = db.ListMergeRequests(ctx)
	if len(list) != 2 {
		t.Errorf("re-sync duplicated MRs: %d", len(list))
	}
}
