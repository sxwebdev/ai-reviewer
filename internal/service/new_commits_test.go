package service

import (
	"testing"

	"github.com/sxwebdev/ai-reviewer/internal/gitlab"
	"github.com/sxwebdev/ai-reviewer/internal/review"
)

// TestNewCommitsSince covers the walk that turns the MR commit list into the
// "new commits since the reviewed head" set: the reviewed SHA is the boundary,
// and its absence (rewritten history) yields found=false rather than a guess.
func TestNewCommitsSince(t *testing.T) {
	ctx := t.Context()
	fake := gitlab.NewFake()
	// GitLab returns commits newest-first.
	fake.Commits["10/5"] = []gitlab.Commit{ // FakeClient keys are "projectKey/iid"
		{ID: "c3", ShortID: "c3", Title: "third"},
		{ID: "c2", ShortID: "c2", Title: "second"},
		{ID: "c1", ShortID: "c1", Title: "first (reviewed head)"},
	}
	svc := NewReviewService(fake, testDB(t), review.NewEngine(nil, discardLogger()),
		ReviewConfig{Profile: review.DefaultProfile()}, discardLogger())

	t.Run("boundary found returns the newer prefix", func(t *testing.T) {
		commits, total, found, err := svc.NewCommitsSince(ctx, "10", 5, "c1")
		if err != nil {
			t.Fatal(err)
		}
		if !found {
			t.Fatal("found = false, want true")
		}
		if total != 2 {
			t.Fatalf("total = %d, want 2", total)
		}
		if len(commits) != 2 || commits[0].ID != "c3" || commits[1].ID != "c2" {
			t.Fatalf("commits = %+v, want [c3 c2]", commits)
		}
	})

	t.Run("head already current returns empty", func(t *testing.T) {
		commits, total, found, err := svc.NewCommitsSince(ctx, "10", 5, "c3")
		if err != nil {
			t.Fatal(err)
		}
		if !found || total != 0 || len(commits) != 0 {
			t.Fatalf("commits = %+v, total = %d, found = %v, want [] 0 true", commits, total, found)
		}
	})

	t.Run("boundary absent means rewritten history", func(t *testing.T) {
		commits, total, found, err := svc.NewCommitsSince(ctx, "10", 5, "gone")
		if err != nil {
			t.Fatal(err)
		}
		if found || total != 0 || commits != nil {
			t.Fatalf("commits = %+v, total = %d, found = %v, want nil 0 false", commits, total, found)
		}
	})

	t.Run("total exceeds the display cap", func(t *testing.T) {
		big := make([]gitlab.Commit, 0, maxNewCommits+6)
		for i := 0; i < maxNewCommits+5; i++ {
			big = append(big, gitlab.Commit{ID: string(rune('A' + i))}) // arbitrary distinct new commits
		}
		big = append(big, gitlab.Commit{ID: "base"}) // the reviewed head, at the tail
		fake.Commits["10/9"] = big

		commits, total, found, err := svc.NewCommitsSince(ctx, "10", 9, "base")
		if err != nil {
			t.Fatal(err)
		}
		if !found {
			t.Fatal("found = false, want true")
		}
		if total != maxNewCommits+5 {
			t.Fatalf("total = %d, want %d (full count, uncapped)", total, maxNewCommits+5)
		}
		if len(commits) != maxNewCommits {
			t.Fatalf("len(commits) = %d, want display cap %d", len(commits), maxNewCommits)
		}
	})
}
