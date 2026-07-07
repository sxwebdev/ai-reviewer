package service

import (
	"strings"
	"testing"

	"github.com/sxwebdev/ai-reviewer/internal/gitlab"
	"github.com/sxwebdev/ai-reviewer/internal/llm"
	"github.com/sxwebdev/ai-reviewer/internal/review"
	"github.com/sxwebdev/ai-reviewer/internal/state"
)

// TestReviewServiceEndToEnd drives the full review path with fakes: fetch MR +
// diffs from a fake GitLab, run a fake LLM, and persist validated findings with
// mapped GitLab positions.
func TestReviewServiceEndToEnd(t *testing.T) {
	ctx := t.Context()
	db := testDB(t)

	const diff = "@@ -1,2 +1,3 @@\n package main\n+import \"fmt\"\n func main() {}\n"

	fake := gitlab.NewFake()
	fake.Projects["10"] = &gitlab.Project{ID: 10, PathWithNamespace: "group/repo"}
	fake.MRs["10/5"] = &gitlab.MergeRequest{
		IID: 5, ProjectID: 10, Title: "Add import", SHA: "headsha",
		Author:   gitlab.User{Username: "alice"},
		DiffRefs: gitlab.DiffRefs{BaseSHA: "base", HeadSHA: "headsha", StartSHA: "start"},
	}
	fake.Diffs["10/5"] = []gitlab.MergeRequestDiff{
		{OldPath: "main.go", NewPath: "main.go", Diff: diff},
	}

	llmResp := &llm.ReviewResponse{
		Summary: "ok", RiskLevel: "medium", OverallRecommendation: "comment",
		Findings: []llm.Finding{
			{Severity: "high", Category: "correctness", FilePath: "main.go", LineKind: "new", Line: 2,
				Title: "Handle fmt errors", Body: "Consider error handling.", Confidence: 0.9},
			{Severity: "high", Category: "correctness", FilePath: "ghost.go", LineKind: "new", Line: 1,
				Title: "not in diff", Body: "dropped", Confidence: 0.9}, // file not in diff → dropped
		},
		CostUSD: 0.02,
	}
	eng := review.NewEngine(llm.NewFake(llmResp), discardLogger())

	svc := NewReviewService(fake, db, eng, ReviewConfig{
		Host: "https://gitlab.test", ReviewerUsername: "me", Model: "sonnet",
		LLMProvider: "claude-cli", Profile: review.DefaultProfile(),
	}, discardLogger())

	reviewID, err := svc.RunReview(ctx, gitlab.MRRef{ProjectID: 10, IID: 5}, ReviewOptions{})
	if err != nil {
		t.Fatal(err)
	}

	rv, err := db.GetReview(ctx, reviewID)
	if err != nil {
		t.Fatal(err)
	}
	if rv.Status != state.ReviewReady || rv.RiskLevel != "medium" || rv.CostUSD != 0.02 {
		t.Errorf("review persisted wrong: %+v", rv)
	}

	findings, _ := db.ListFindingsByReview(ctx, reviewID)
	if len(findings) != 1 {
		t.Fatalf("want 1 finding (ghost.go dropped), got %d", len(findings))
	}
	f := findings[0]
	if f.Title != "Handle fmt errors" || f.Status != state.FindingProposed {
		t.Errorf("finding wrong: %+v", f)
	}
	if f.NewLine == nil || *f.NewLine != 2 {
		t.Errorf("finding position not mapped to new line 2: %+v", f.NewLine)
	}
	if f.GitLabPositionJSON == "" {
		t.Error("finding should carry a serialized GitLab position")
	}

	// The MR was tracked as a side effect.
	if _, err := db.GetMergeRequestByIID(ctx, "https://gitlab.test", 10, 5); err != nil {
		t.Errorf("MR should be tracked: %v", err)
	}

	// Re-review dedupes against the prior finding (same fingerprint).
	reviewID2, err := svc.RunReview(ctx, gitlab.MRRef{ProjectID: 10, IID: 5}, ReviewOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if f2, _ := db.ListFindingsByReview(ctx, reviewID2); len(f2) != 0 {
		t.Errorf("re-review should dedupe existing finding, got %d", len(f2))
	}
}

// A re-review at a new head SHA must carry the previous review (summary,
// findings with dispositions and rejection reasons) into the prompt.
func TestReviewServicePriorReviewContext(t *testing.T) {
	ctx := t.Context()
	db := testDB(t)

	const diff = "@@ -1,2 +1,3 @@\n package main\n+import \"fmt\"\n func main() {}\n"

	fake := gitlab.NewFake()
	fake.Projects["10"] = &gitlab.Project{ID: 10, PathWithNamespace: "group/repo"}
	fake.MRs["10/5"] = &gitlab.MergeRequest{
		IID: 5, ProjectID: 10, Title: "Add import", SHA: "sha-one",
		Author:   gitlab.User{Username: "alice"},
		DiffRefs: gitlab.DiffRefs{BaseSHA: "base", HeadSHA: "sha-one", StartSHA: "start"},
	}
	fake.Diffs["10/5"] = []gitlab.MergeRequestDiff{{OldPath: "main.go", NewPath: "main.go", Diff: diff}}

	llmFake := llm.NewFake(&llm.ReviewResponse{
		Summary: "first pass summary", RiskLevel: "medium", OverallRecommendation: "comment",
		Findings: []llm.Finding{
			{Severity: "high", Category: "correctness", FilePath: "main.go", LineKind: "new", Line: 2,
				Title: "Handle fmt errors", Body: "b", Confidence: 0.9},
		},
	})
	eng := review.NewEngine(llmFake, discardLogger())
	svc := NewReviewService(fake, db, eng, ReviewConfig{
		Host: "https://gitlab.test", ReviewerUsername: "me", Model: "sonnet",
		LLMProvider: "claude-cli", Profile: review.DefaultProfile(),
		Context: review.DefaultContextBudget(),
	}, discardLogger())

	reviewID, err := svc.RunReview(ctx, gitlab.MRRef{ProjectID: 10, IID: 5}, ReviewOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// First review must NOT carry a prior-review section.
	if strings.Contains(llmFake.LastRequest.Prompt, "Previous review of this MR") {
		t.Error("first review must not have a prior-review section")
	}

	// Human rejects the finding with a reason.
	findings, _ := db.ListFindingsByReview(ctx, reviewID)
	if len(findings) != 1 {
		t.Fatalf("want 1 finding, got %d", len(findings))
	}
	if err := db.UpdateFindingStatus(ctx, findings[0].ID, state.FindingRejected, "intentional: errors ignored here"); err != nil {
		t.Fatal(err)
	}

	// New head SHA arrives.
	fake.MRs["10/5"].SHA = "sha-two"
	fake.MRs["10/5"].DiffRefs.HeadSHA = "sha-two"

	if _, err := svc.RunReview(ctx, gitlab.MRRef{ProjectID: 10, IID: 5}, ReviewOptions{}); err != nil {
		t.Fatal(err)
	}
	prompt := llmFake.LastRequest.Prompt
	if !strings.Contains(prompt, "Previous review of this MR (at commit sha-one)") {
		t.Errorf("re-review prompt missing prior-review section:\n%s", prompt)
	}
	if !strings.Contains(prompt, "first pass summary") {
		t.Error("prior summary missing from prompt")
	}
	if !strings.Contains(prompt, "rejected: intentional: errors ignored here") {
		t.Error("rejection reason missing from prompt")
	}
}

// Project-scoped review memory must reach only its own project's prompt; global
// memory (no project id) must reach every project. Guards the loadMemory scope
// filter that backs the "remember context for this project" feature.
func TestReviewServiceMemoryScope(t *testing.T) {
	ctx := t.Context()
	db := testDB(t)

	const diff = "@@ -1,2 +1,3 @@\n package main\n+import \"fmt\"\n func main() {}\n"
	fake := gitlab.NewFake()
	fake.Projects["10"] = &gitlab.Project{ID: 10, PathWithNamespace: "group/repo"}
	fake.MRs["10/5"] = &gitlab.MergeRequest{
		IID: 5, ProjectID: 10, Title: "Add import", SHA: "headsha",
		Author:   gitlab.User{Username: "alice"},
		DiffRefs: gitlab.DiffRefs{BaseSHA: "base", HeadSHA: "headsha", StartSHA: "start"},
	}
	fake.Diffs["10/5"] = []gitlab.MergeRequestDiff{{OldPath: "main.go", NewPath: "main.go", Diff: diff}}

	otherProj, thisProj := int64(99), int64(10)
	for _, m := range []*state.ReviewMemory{
		{ID: "global", Scope: "global", Type: "convention", Title: "GlobalRule", Body: "global-body", Enabled: true},
		{ID: "mine", Scope: "project", ProjectID: &thisProj, Type: "context", Title: "MineRule", Body: "mine-body", Enabled: true},
		{ID: "other", Scope: "project", ProjectID: &otherProj, Type: "context", Title: "OtherRule", Body: "other-body", Enabled: true},
	} {
		if err := db.UpsertReviewMemory(ctx, m); err != nil {
			t.Fatal(err)
		}
	}

	llmFake := llm.NewFake(&llm.ReviewResponse{Summary: "ok", RiskLevel: "low", OverallRecommendation: "comment"})
	eng := review.NewEngine(llmFake, discardLogger())
	svc := NewReviewService(fake, db, eng, ReviewConfig{
		Host: "https://gitlab.test", ReviewerUsername: "me", Model: "sonnet",
		LLMProvider: "claude-cli", Profile: review.DefaultProfile(),
	}, discardLogger())

	if _, err := svc.RunReview(ctx, gitlab.MRRef{ProjectID: 10, IID: 5}, ReviewOptions{}); err != nil {
		t.Fatal(err)
	}
	prompt := llmFake.LastRequest.Prompt
	if !strings.Contains(prompt, "global-body") {
		t.Error("global memory should reach every project's prompt")
	}
	if !strings.Contains(prompt, "mine-body") {
		t.Error("this project's scoped memory should be in the prompt")
	}
	if strings.Contains(prompt, "other-body") {
		t.Error("another project's scoped memory leaked into this project's prompt")
	}
}
