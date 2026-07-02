package review

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/sxwebdev/ai-reviewer/internal/llm"
)

func TestEngineReviewPipeline(t *testing.T) {
	files := []*FileDiff{fileDiff(t, "main.go", "main.go", mapDiff)}
	resp := &llm.ReviewResponse{
		Summary:               "Looks mostly good.",
		RiskLevel:             "medium",
		OverallRecommendation: "comment",
		Findings: []llm.Finding{
			{Severity: "high", Category: "correctness", FilePath: "main.go", LineKind: "new", Line: 2,
				Title: "Guard the import", Body: "Explain the issue.", Confidence: 0.9},
			{Severity: "nit", Category: "style", FilePath: "main.go", LineKind: "new", Line: 3,
				Title: "cosmetic", Body: "meh", Confidence: 0.3}, // dropped by threshold
		},
		CostUSD: 0.01,
	}
	fake := llm.NewFake(resp)
	eng := NewEngine(fake, slog.New(slog.NewTextHandler(io.Discard, nil)))

	in := ReviewInput{
		ProjectPath: "group/repo", ProjectID: 1, MRIID: 5,
		Title: "Add imports", AuthorUsername: "bob", ReviewerUsername: "me",
		SourceBranch: "feat", TargetBranch: "main",
		Files:   files,
		Refs:    testRefs,
		Memory:  []MemoryRule{{Type: "repo_rule", Title: "Context", Body: "Pass ctx to DB."}},
		Profile: DefaultProfile(),
	}
	res, err := eng.Review(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}

	if res.RiskLevel != "medium" || res.Recommendation != "comment" {
		t.Errorf("summary fields wrong: %+v", res)
	}
	if len(res.Findings) != 1 {
		t.Fatalf("want 1 validated finding (nit dropped), got %d", len(res.Findings))
	}
	if res.Findings[0].Position == nil || res.Findings[0].Position.NewLine == nil || *res.Findings[0].Position.NewLine != 2 {
		t.Errorf("position not mapped: %+v", res.Findings[0].Position)
	}
	if res.CostUSD != 0.01 {
		t.Errorf("cost not propagated: %v", res.CostUSD)
	}

	// The prompt must carry MR metadata and the memory rule.
	if !strings.Contains(fake.LastRequest.Prompt, "group/repo") {
		t.Error("prompt missing project path")
	}
	if !strings.Contains(fake.LastRequest.Prompt, "Pass ctx to DB") {
		t.Error("prompt missing review memory rule")
	}
	if fake.LastRequest.JSONSchema == "" {
		t.Error("review should request strict JSON schema")
	}
}
