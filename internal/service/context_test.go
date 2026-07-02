package service

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sxwebdev/ai-reviewer/internal/gitlab"
	"github.com/sxwebdev/ai-reviewer/internal/review"
)

func contextTestService(t *testing.T, gl gitlab.API, budget review.ContextBudget) *ReviewService {
	t.Helper()
	return NewReviewService(gl, nil, nil, ReviewConfig{
		ReviewerUsername: "me",
		Profile:          review.DefaultProfile(),
		Context:          budget,
	}, discardLogger())
}

func genLines(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	return b.String()
}

const ctxDiff = "@@ -1,2 +1,3 @@\n line 1\n+line 2\n line 3\n"

func parsedFile(t *testing.T, path, diff string) *review.FileDiff {
	t.Helper()
	hunks, err := review.ParseHunks(diff)
	if err != nil {
		t.Fatal(err)
	}
	return &review.FileDiff{OldPath: path, NewPath: path, Hunks: hunks}
}

func TestBuildFileContextsFromWorktree(t *testing.T) {
	wt := t.TempDir()
	if err := os.WriteFile(filepath.Join(wt, "main.go"), []byte(genLines(10)), 0o600); err != nil {
		t.Fatal(err)
	}
	svc := contextTestService(t, gitlab.NewFake(), review.DefaultContextBudget())
	mr := &gitlab.MergeRequest{DiffRefs: gitlab.DiffRefs{HeadSHA: "head"}}
	files := []*review.FileDiff{parsedFile(t, "main.go", ctxDiff)}

	got := svc.buildFileContexts(t.Context(), "10", mr, files, wt)
	if len(got) != 1 || got[0].Path != "main.go" || got[0].Truncated {
		t.Fatalf("want whole-file context for main.go, got %+v", got)
	}
}

func TestBuildFileContextsFromGitLabFallback(t *testing.T) {
	fake := gitlab.NewFake()
	fake.RawFiles["main.go@head"] = []byte(genLines(5))
	svc := contextTestService(t, fake, review.DefaultContextBudget())
	mr := &gitlab.MergeRequest{DiffRefs: gitlab.DiffRefs{HeadSHA: "head"}}
	files := []*review.FileDiff{parsedFile(t, "main.go", ctxDiff)}

	got := svc.buildFileContexts(t.Context(), "10", mr, files, "")
	if len(got) != 1 || len(got[0].Segments) != 1 || len(got[0].Segments[0].Lines) != 5 {
		t.Fatalf("want context via GetRawFile, got %+v", got)
	}
}

func TestBuildFileContextsSkipsBinaryDeletedAndUnreadable(t *testing.T) {
	wt := t.TempDir()
	if err := os.WriteFile(filepath.Join(wt, "bin.dat"), []byte{0x00, 0x01, 0x02}, 0o600); err != nil {
		t.Fatal(err)
	}
	svc := contextTestService(t, gitlab.NewFake(), review.DefaultContextBudget())
	mr := &gitlab.MergeRequest{DiffRefs: gitlab.DiffRefs{HeadSHA: "head"}}
	deleted := parsedFile(t, "gone.go", ctxDiff)
	deleted.Deleted = true
	files := []*review.FileDiff{
		parsedFile(t, "bin.dat", ctxDiff),    // binary content
		deleted,                              // deleted file
		parsedFile(t, "missing.go", ctxDiff), // not on disk
	}

	if got := svc.buildFileContexts(t.Context(), "10", mr, files, wt); len(got) != 0 {
		t.Errorf("want no contexts, got %+v", got)
	}
}

func TestBuildFileContextsBudgetExhaustion(t *testing.T) {
	wt := t.TempDir()
	// big.go has many changed lines (wins ordering) and fits; small.go then
	// exceeds the remaining budget and is dropped.
	if err := os.WriteFile(filepath.Join(wt, "big.go"), []byte(genLines(30)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "small.go"), []byte(genLines(30)), 0o600); err != nil {
		t.Fatal(err)
	}
	bigDiff := "@@ -1,2 +1,4 @@\n line 1\n+line 2\n+line 3\n line 4\n"
	budget := review.ContextBudget{IncludeFullFiles: true, MaxFileLines: 500, HunkWindowLines: 2, MaxTotalBytes: 150}
	svc := contextTestService(t, gitlab.NewFake(), budget)
	mr := &gitlab.MergeRequest{DiffRefs: gitlab.DiffRefs{HeadSHA: "head"}}
	files := []*review.FileDiff{parsedFile(t, "small.go", ctxDiff), parsedFile(t, "big.go", bigDiff)}

	got := svc.buildFileContexts(t.Context(), "10", mr, files, wt)
	if len(got) != 1 || got[0].Path != "big.go" {
		t.Fatalf("want only big.go (most-changed wins budget), got %+v", got)
	}
}

func TestBuildCommitsNewestKeptOldestFirst(t *testing.T) {
	fake := gitlab.NewFake()
	fake.Commits["10/5"] = []gitlab.Commit{
		{ShortID: "c2", Title: "second", Message: "second\n\nlong body here"},
		{ShortID: "c1", Title: "first", Message: "first"},
	}
	svc := contextTestService(t, fake, review.DefaultContextBudget())

	got := svc.buildCommits(t.Context(), "10", 5)
	if len(got) != 2 || got[0].ShortSHA != "c1" || got[1].ShortSHA != "c2" {
		t.Fatalf("commits must be oldest first: %+v", got)
	}
	if got[1].Message != "long body here" {
		t.Errorf("message must strip the title line: %q", got[1].Message)
	}
	if got[0].Message != "" {
		t.Errorf("title-only commit must have empty message: %q", got[0].Message)
	}
}

func TestBuildDiscussionNotesFiltersAndFlags(t *testing.T) {
	line := 7
	discussions := []gitlab.Discussion{
		{ID: "d1", Notes: []gitlab.Note{
			{Body: "system note", System: true, Author: gitlab.User{Username: "x"}},
			{Body: "human comment", Author: gitlab.User{Username: "alice"}, Resolved: true,
				Position: &gitlab.Position{NewPath: "main.go", NewLine: &line}},
			{Body: "bot comment", Author: gitlab.User{Username: "me"}},
		}},
	}
	svc := contextTestService(t, gitlab.NewFake(), review.DefaultContextBudget())
	got := svc.buildDiscussionNotes(discussions)
	if len(got) != 2 {
		t.Fatalf("system notes must be filtered, got %+v", got)
	}
	if !got[0].Resolved || got[0].FilePath != "main.go" || got[0].Line != 7 {
		t.Errorf("inline note mapping wrong: %+v", got[0])
	}
	if !got[1].OwnBot {
		t.Errorf("own reviewer note must be flagged: %+v", got[1])
	}
}

func TestBuildDiscussionNotesBudgetDropsOldest(t *testing.T) {
	var discussions []gitlab.Discussion
	for i := 0; i < 50; i++ {
		discussions = append(discussions, gitlab.Discussion{Notes: []gitlab.Note{
			{Body: strings.Repeat("x", 200), Author: gitlab.User{Username: "alice"}},
		}})
	}
	budget := review.DefaultContextBudget()
	budget.MaxDiscussionBytes = 1000
	svc := contextTestService(t, gitlab.NewFake(), budget)
	got := svc.buildDiscussionNotes(discussions)
	if len(got) == 0 || len(got) >= 50 {
		t.Fatalf("budget must trim oldest notes, got %d", len(got))
	}
}

func TestBuildFileContextsDisabled(t *testing.T) {
	svc := contextTestService(t, gitlab.NewFake(), review.ContextBudget{IncludeFullFiles: false})
	mr := &gitlab.MergeRequest{DiffRefs: gitlab.DiffRefs{HeadSHA: "head"}}
	if got := svc.buildFileContexts(t.Context(), "10", mr, []*review.FileDiff{parsedFile(t, "a.go", ctxDiff)}, ""); got != nil {
		t.Errorf("disabled context should return nil, got %+v", got)
	}
}
