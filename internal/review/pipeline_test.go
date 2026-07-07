package review

import (
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sxwebdev/ai-reviewer/internal/llm"
)

func pipelineEngine(fake *llm.FakeClient) *Engine {
	return NewEngine(fake, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func testInput(t *testing.T, pc PipelineConfig) ReviewInput {
	t.Helper()
	return ReviewInput{
		ProjectPath: "group/repo", ProjectID: 1, MRIID: 5,
		Title: "Add imports", AuthorUsername: "bob", ReviewerUsername: "me",
		Files:    []*FileDiff{fileDiff(t, "main.go", "main.go", mapDiff)},
		Refs:     testRefs,
		Profile:  DefaultProfile(),
		Pipeline: pc,
	}
}

func passFinding(sev, category, title string, conf float64) llm.Finding {
	return llm.Finding{
		Severity: sev, Category: category, FilePath: "main.go", LineKind: "new", Line: 2,
		Title: title, Body: "body of " + title, Confidence: conf,
	}
}

func TestPipelineCheapModeIsSinglePass(t *testing.T) {
	fake := llm.NewFake(&llm.ReviewResponse{
		Summary: "ok", RiskLevel: "low", OverallRecommendation: "comment",
		Findings: []llm.Finding{passFinding("high", "correctness", "bug", 0.9)},
		CostUSD:  0.01,
	})
	res, err := pipelineEngine(fake).Review(t.Context(), testInput(t, PipelineConfig{}))
	if err != nil {
		t.Fatal(err)
	}
	if fake.Calls != 1 {
		t.Errorf("zero-value pipeline must make exactly 1 LLM call, got %d", fake.Calls)
	}
	if len(res.Findings) != 1 || res.Summary != "ok" || res.CostUSD != 0.01 {
		t.Errorf("cheap-mode result wrong: %+v", res)
	}
	if len(res.PassReports) != 1 || res.PassReports[0].Name != PassGeneral {
		t.Errorf("pass report wrong: %+v", res.PassReports)
	}
}

// passByPrompt routes fake responses by the specialist suffix in the system prompt.
func passByPrompt(t *testing.T, responses map[string]*llm.ReviewResponse, errs map[string]error) func(llm.Request) (*llm.ReviewResponse, error) {
	t.Helper()
	return func(req llm.Request) (*llm.ReviewResponse, error) {
		name := PassGeneral
		for _, n := range []string{PassCorrectness, PassConcurrency, PassSecurity, PassContracts} {
			if strings.Contains(req.System, "Specialist pass: "+specialistLabel(n)) {
				name = n
				break
			}
		}
		if err := errs[name]; err != nil {
			return nil, err
		}
		resp, ok := responses[name]
		if !ok {
			t.Errorf("unexpected pass %q", name)
			return &llm.ReviewResponse{}, nil
		}
		return resp, nil
	}
}

func specialistLabel(name string) string {
	switch name {
	case PassCorrectness:
		return "correctness only"
	case PassConcurrency:
		return "concurrency only"
	case PassSecurity:
		return "security only"
	case PassContracts:
		return "cross-file contracts only"
	}
	return name
}

func TestPipelineFanOutMergesAndDedupes(t *testing.T) {
	fake := &llm.FakeClient{}
	fake.ReviewFn = passByPrompt(t, map[string]*llm.ReviewResponse{
		PassGeneral: {
			Summary: "primary summary", RiskLevel: "medium", OverallRecommendation: "comment",
			Findings: []llm.Finding{passFinding("medium", "maintainability", "Shared Bug", 0.6)},
			CostUSD:  0.01,
		},
		PassCorrectness: {
			Summary: "ignored", RiskLevel: "high", OverallRecommendation: "request_changes",
			// Same file+title, different category and higher severity — must
			// dedupe across passes and keep this more severe instance.
			Findings: []llm.Finding{
				passFinding("high", "correctness", "Shared Bug", 0.9),
				passFinding("high", "correctness", "Unique Bug", 0.8),
			},
			CostUSD: 0.02,
		},
	}, nil)

	res, err := pipelineEngine(fake).Review(t.Context(),
		testInput(t, PipelineConfig{Passes: []string{PassGeneral, PassCorrectness}}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Summary != "primary summary" || res.RiskLevel != "medium" {
		t.Errorf("primary pass must seed summary/risk: %+v", res)
	}
	if res.CostUSD != 0.03 {
		t.Errorf("cost must sum across passes: %v", res.CostUSD)
	}
	if len(res.Findings) != 2 {
		t.Fatalf("want 2 findings after cross-pass dedupe, got %d: %+v", len(res.Findings), res.Findings)
	}
	byTitle := map[string]ValidatedFinding{}
	for _, f := range res.Findings {
		byTitle[f.Title] = f
	}
	shared := byTitle["Shared Bug"]
	if shared.Severity != "high" || shared.Pass != PassCorrectness {
		t.Errorf("dedupe must keep the more severe instance with provenance: %+v", shared)
	}
	if byTitle["Unique Bug"].Pass != PassCorrectness {
		t.Errorf("provenance missing: %+v", byTitle["Unique Bug"])
	}
}

func TestPipelinePassFailureDegrades(t *testing.T) {
	fake := &llm.FakeClient{}
	fake.ReviewFn = passByPrompt(t, map[string]*llm.ReviewResponse{
		PassGeneral: {
			Summary: "ok", RiskLevel: "low", OverallRecommendation: "comment",
			Findings: []llm.Finding{passFinding("high", "correctness", "bug", 0.9)},
		},
	}, map[string]error{PassCorrectness: errors.New("boom")})

	res, err := pipelineEngine(fake).Review(t.Context(),
		testInput(t, PipelineConfig{Passes: []string{PassGeneral, PassCorrectness}}))
	if err != nil {
		t.Fatalf("one failed pass must not fail the review: %v", err)
	}
	if len(res.Findings) != 1 {
		t.Errorf("surviving pass findings must remain: %+v", res.Findings)
	}
	var failed *PassReport
	for i := range res.PassReports {
		if res.PassReports[i].Name == PassCorrectness {
			failed = &res.PassReports[i]
		}
	}
	if failed == nil || failed.Err == "" {
		t.Errorf("failed pass must be recorded in reports: %+v", res.PassReports)
	}
}

func TestPipelineAllPassesFailErrors(t *testing.T) {
	fake := &llm.FakeClient{Err: errors.New("llm down")}
	_, err := pipelineEngine(fake).Review(t.Context(),
		testInput(t, PipelineConfig{Passes: []string{PassGeneral, PassCorrectness}}))
	if err == nil || !strings.Contains(err.Error(), "all 2 review passes failed") {
		t.Fatalf("want all-passes-failed error, got %v", err)
	}
}

func TestPipelinePrimaryPassFailureSynthesizesSummary(t *testing.T) {
	fake := &llm.FakeClient{}
	fake.ReviewFn = passByPrompt(t, map[string]*llm.ReviewResponse{
		PassCorrectness: {
			Summary: "specialist", RiskLevel: "high", OverallRecommendation: "request_changes",
			Findings: []llm.Finding{passFinding("high", "correctness", "bug", 0.9)},
		},
	}, map[string]error{PassGeneral: errors.New("boom")})

	res, err := pipelineEngine(fake).Review(t.Context(),
		testInput(t, PipelineConfig{Passes: []string{PassGeneral, PassCorrectness}}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Summary, "primary pass unavailable") {
		t.Errorf("summary must be synthesized: %q", res.Summary)
	}
	if res.RiskLevel != "high" || res.Recommendation != "request_changes" {
		t.Errorf("synthesized risk must follow max severity: %+v", res)
	}
}

func TestPipelineMaxParallel(t *testing.T) {
	var inFlight, peak atomic.Int64
	fake := &llm.FakeClient{}
	fake.ReviewFn = func(req llm.Request) (*llm.ReviewResponse, error) {
		cur := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			p := peak.Load()
			if cur <= p || peak.CompareAndSwap(p, cur) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		return &llm.ReviewResponse{Summary: "ok", RiskLevel: "low", OverallRecommendation: "comment"}, nil
	}
	_, err := pipelineEngine(fake).Review(t.Context(), testInput(t, PipelineConfig{
		Passes:      []string{PassGeneral, PassCorrectness, PassConcurrency, PassSecurity, PassContracts},
		MaxParallel: 2,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got := peak.Load(); got > 2 {
		t.Errorf("max parallel exceeded: peak %d", got)
	}
	if fake.Calls != 5 {
		t.Errorf("want 5 pass calls, got %d", fake.Calls)
	}
}

func TestResolvePassesUnknownSkippedAndPrimaryEnsured(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	specs := ResolvePasses([]string{"nonsense", PassSecurity}, log)
	if len(specs) != 1 || specs[0].Name != PassSecurity {
		t.Fatalf("unknown pass must be skipped: %+v", specs)
	}
	if !specs[0].Primary {
		t.Error("first pass must become primary when general is absent")
	}
	if specs := ResolvePasses([]string{"nope"}, log); len(specs) != 1 || specs[0].Name != PassGeneral {
		t.Errorf("all-unknown must fall back to general: %+v", specs)
	}
}

// TestPipelineConcurrentPromptReads guards against data races between passes
// reading the shared ReviewInput (run with -race).
func TestPipelineConcurrentPromptReads(t *testing.T) {
	fake := &llm.FakeClient{}
	var mu sync.Mutex
	prompts := map[string]bool{}
	fake.ReviewFn = func(req llm.Request) (*llm.ReviewResponse, error) {
		mu.Lock()
		prompts[req.System] = true
		mu.Unlock()
		return &llm.ReviewResponse{Summary: "ok", RiskLevel: "low", OverallRecommendation: "comment"}, nil
	}
	_, err := pipelineEngine(fake).Review(t.Context(), testInput(t, PipelineConfig{
		Passes:      []string{PassGeneral, PassCorrectness, PassSecurity},
		MaxParallel: 3,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(prompts) != 3 {
		t.Errorf("want 3 distinct pass system prompts, got %d", len(prompts))
	}
}
