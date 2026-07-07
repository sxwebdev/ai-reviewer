package review

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/sxwebdev/ai-reviewer/internal/llm"
)

func completenessInput(t *testing.T) ReviewInput {
	t.Helper()
	in := testInput(t, PipelineConfig{Completeness: CompletenessAuto})
	in.Description = "Implements refund processing:\n- validate amounts\n- write audit log"
	in.Commits = []CommitInfo{{ShortSHA: "c1", Title: "feat: refunds", Message: "adds ProcessRefund"}}
	return in
}

func TestCompletenessAuditViaEngine(t *testing.T) {
	fake := llm.NewFake(&llm.ReviewResponse{
		Summary: "ok", RiskLevel: "low", OverallRecommendation: "comment",
	})
	fake.JSONFn = func(req llm.Request) (string, error) {
		if req.JSONSchema != llm.CompletenessJSONSchema {
			return "", errors.New("unexpected CompleteJSON schema")
		}
		if !strings.Contains(req.Prompt, "validate amounts") || !strings.Contains(req.Prompt, "adds ProcessRefund") {
			t.Errorf("completeness prompt missing intent text:\n%s", req.Prompt)
		}
		if strings.Contains(req.Prompt, "Project rules") {
			t.Error("completeness prompt must not carry memory rules")
		}
		v, _ := json.Marshal(llm.CompletenessResponse{Criteria: []llm.CompletenessCriterion{
			{Criterion: "validate amounts", Status: "done", Evidence: "validateAmount in pay.go"},
			{Criterion: "write audit log", Status: "missing", Evidence: "no audit writes in diff"},
		}})
		return string(v), nil
	}

	fake.Response.CostUSD = 0.02
	fake.JSONCost = 0.04

	res, err := pipelineEngine(fake).Review(t.Context(), completenessInput(t))
	if err != nil {
		t.Fatal(err)
	}
	if res.Completeness == nil || len(res.Completeness.Criteria) != 2 {
		t.Fatalf("completeness report missing: %+v", res.Completeness)
	}
	if res.Completeness.Criteria[1].Status != "missing" {
		t.Errorf("criterion status wrong: %+v", res.Completeness.Criteria[1])
	}
	// Audit spend must be accounted and visible in the pass table.
	if res.CostUSD != 0.06 {
		t.Errorf("CostUSD must include the completeness call: got %v, want 0.06", res.CostUSD)
	}
	found := false
	for _, r := range res.PassReports {
		if r.Name == "completeness" && r.CostUSD == 0.04 {
			found = true
		}
	}
	if !found {
		t.Errorf("completeness must appear in pass reports: %+v", res.PassReports)
	}
}

func TestCompletenessFailureIsNonFatal(t *testing.T) {
	fake := llm.NewFake(&llm.ReviewResponse{Summary: "ok", RiskLevel: "low", OverallRecommendation: "comment"})
	fake.JSONFn = func(req llm.Request) (string, error) { return "", errors.New("audit down") }

	res, err := pipelineEngine(fake).Review(t.Context(), completenessInput(t))
	if err != nil {
		t.Fatalf("completeness failure must not fail the review: %v", err)
	}
	if res.Completeness != nil {
		t.Errorf("failed audit must yield nil report: %+v", res.Completeness)
	}
}

func TestCompletenessSkippedWithoutIntent(t *testing.T) {
	fake := llm.NewFake(&llm.ReviewResponse{Summary: "ok", RiskLevel: "low", OverallRecommendation: "comment"})
	in := testInput(t, PipelineConfig{Completeness: CompletenessAuto}) // bare title, no description/commits
	if _, err := pipelineEngine(fake).Review(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	if fake.Calls != 1 {
		t.Errorf("no intent text → audit must be skipped, got %d calls", fake.Calls)
	}
}

func TestCompletenessExplicitOnRunsWithoutIntent(t *testing.T) {
	// Explicit "on" must run even without intent text — hasIntentText depends
	// on the commits section, which an unrelated config flag can empty.
	fake := llm.NewFake(&llm.ReviewResponse{Summary: "ok", RiskLevel: "low", OverallRecommendation: "comment"})
	fake.JSON = `{"criteria":[]}`
	in := testInput(t, PipelineConfig{Completeness: CompletenessOn}) // bare title, no description/commits
	if _, err := pipelineEngine(fake).Review(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	if fake.Calls != 2 {
		t.Errorf("explicit on must run the audit despite missing intent, got %d calls", fake.Calls)
	}
}

func TestHasIntentText(t *testing.T) {
	if hasIntentText(ReviewInput{Title: "fix"}) {
		t.Error("bare title must not count as intent")
	}
	if !hasIntentText(ReviewInput{Description: "This MR implements the refund flow end to end."}) {
		t.Error("real description must count")
	}
	if !hasIntentText(ReviewInput{Commits: []CommitInfo{{Title: "a", Message: "does X because Y"}}}) {
		t.Error("commit body must count")
	}
	if !hasIntentText(ReviewInput{Commits: []CommitInfo{{Title: "a"}, {Title: "b"}}}) {
		t.Error("multiple commit titles must count")
	}
}
