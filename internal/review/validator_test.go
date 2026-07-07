package review

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/sxwebdev/ai-reviewer/internal/llm"
	"github.com/sxwebdev/ai-reviewer/internal/security"
)

func testFiles(t *testing.T) []*FileDiff {
	return []*FileDiff{fileDiff(t, "main.go", "main.go", mapDiff)}
}

func TestValidatorMapsAddedLineAndRanks(t *testing.T) {
	resp := &llm.ReviewResponse{Findings: []llm.Finding{
		{Severity: "low", Category: "style", FilePath: "main.go", LineKind: "new", Line: 2,
			Title: "low one", Body: "b", Confidence: 0.5},
		{Severity: "blocking", Category: "correctness", FilePath: "main.go", LineKind: "new", Line: 3,
			Title: "blocker", Body: "serious", Confidence: 0.9},
	}}
	v := NewValidator(ValidatorConfig{SeverityThreshold: "medium", MaxComments: 10})
	got, _ := v.Validate(resp, testFiles(t), testRefs, 1, 5, nil)

	if len(got) != 1 {
		t.Fatalf("want 1 finding (low dropped by threshold), got %d", len(got))
	}
	if got[0].Severity != "blocking" {
		t.Errorf("expected blocking to survive, got %q", got[0].Severity)
	}
	if got[0].Position == nil || got[0].Position.NewLine == nil || *got[0].Position.NewLine != 3 {
		t.Errorf("position not mapped for added line: %+v", got[0].Position)
	}
}

func TestValidatorDropsFileNotInDiff(t *testing.T) {
	resp := &llm.ReviewResponse{Findings: []llm.Finding{
		{Severity: "high", Category: "correctness", FilePath: "other.go", LineKind: "new", Line: 1,
			Title: "elsewhere", Body: "x", Confidence: 0.9},
	}}
	v := NewValidator(ValidatorConfig{SeverityThreshold: "low"})
	got, _ := v.Validate(resp, testFiles(t), testRefs, 1, 5, nil)
	if len(got) != 0 {
		t.Errorf("finding on file not in diff must be dropped, got %d", len(got))
	}
}

func TestValidatorDedupesWithinAndAgainstExisting(t *testing.T) {
	mk := func() llm.Finding {
		return llm.Finding{Severity: "high", Category: "correctness", FilePath: "main.go",
			LineKind: "new", Line: 2, Title: "Same Issue", Body: "b", Confidence: 0.8}
	}
	resp := &llm.ReviewResponse{Findings: []llm.Finding{mk(), mk()}} // duplicate within response
	v := NewValidator(ValidatorConfig{SeverityThreshold: "low"})

	got, _ := v.Validate(resp, testFiles(t), testRefs, 1, 5, nil)
	if len(got) != 1 {
		t.Fatalf("intra-response dedupe failed: got %d", len(got))
	}

	// Now pre-seed the fingerprint as existing → should drop entirely.
	existing := map[string]bool{got[0].Fingerprint: true}
	got2, _ := v.Validate(&llm.ReviewResponse{Findings: []llm.Finding{mk()}}, testFiles(t), testRefs, 1, 5, existing)
	if len(got2) != 0 {
		t.Errorf("existing-fingerprint dedupe failed: got %d", len(got2))
	}
}

func TestValidatorMaxComments(t *testing.T) {
	var findings []llm.Finding
	for i := range 5 {
		findings = append(findings, llm.Finding{
			Severity: "high", Category: "correctness", FilePath: "main.go", LineKind: "new", Line: 2,
			Title: "issue " + string(rune('A'+i)), Body: "b", Confidence: 0.8,
		})
	}
	v := NewValidator(ValidatorConfig{SeverityThreshold: "low", MaxComments: 2})
	got, _ := v.Validate(&llm.ReviewResponse{Findings: findings}, testFiles(t), testRefs, 1, 5, nil)
	if len(got) != 2 {
		t.Errorf("max_comments cap failed: got %d, want 2", len(got))
	}
}

func TestValidatorScrubsSecretsInBody(t *testing.T) {
	security.RegisterSecret("glpat-scrubbedinbody12345")
	resp := &llm.ReviewResponse{Findings: []llm.Finding{
		{Severity: "high", Category: "security", FilePath: "main.go", LineKind: "new", Line: 2,
			Title: "leak", Body: "token glpat-scrubbedinbody12345 here", Confidence: 0.9},
	}}
	v := NewValidator(ValidatorConfig{SeverityThreshold: "low"})
	got, _ := v.Validate(resp, testFiles(t), testRefs, 1, 5, nil)
	if len(got) != 1 {
		t.Fatal("want 1 finding")
	}
	if strings.Contains(got[0].Body, "scrubbedinbody") {
		t.Errorf("secret leaked in body: %q", got[0].Body)
	}
}

func TestValidatorOverviewWhenLineFar(t *testing.T) {
	resp := &llm.ReviewResponse{Findings: []llm.Finding{
		{Severity: "high", Category: "correctness", FilePath: "main.go", LineKind: "new", Line: 999,
			Title: "far", Body: "b", Confidence: 0.9},
	}}
	v := NewValidator(ValidatorConfig{SeverityThreshold: "low"})
	got, _ := v.Validate(resp, testFiles(t), testRefs, 1, 5, nil)
	if len(got) != 1 {
		t.Fatal("want 1 finding (kept as overview, not dropped)")
	}
	// Line 999 is beyond the snap ceiling → overview with a validation note.
	if got[0].Outcome.Kind != MapOverview || got[0].Position != nil {
		t.Errorf("expected overview, got kind=%v pos=%v", got[0].Outcome.Kind, got[0].Position)
	}
	if got[0].ValidationError == "" {
		t.Error("overview finding should carry a validation note")
	}
}

func TestValidatorKeepsCriticalAndBlocking(t *testing.T) {
	resp := &llm.ReviewResponse{Findings: []llm.Finding{
		// "critical" is not in the base enum but must not be dropped.
		{Severity: "critical", Category: "security", FilePath: "main.go", LineKind: "new", Line: 2,
			Title: "crit", Body: "x", Confidence: 0.9},
		// A blocking finding with an odd severity label must survive as a floor.
		{Severity: "cosmetic", Category: "style", FilePath: "main.go", LineKind: "new", Line: 3,
			Title: "blk", Body: "y", Confidence: 0.9, Blocking: true},
	}}
	v := NewValidator(ValidatorConfig{SeverityThreshold: "high"}) // high threshold
	got, _ := v.Validate(resp, testFiles(t), testRefs, 1, 5, nil)
	if len(got) != 2 {
		t.Fatalf("critical + blocking must survive a high threshold, got %d", len(got))
	}
}

func TestSanitizeBodyRuneSafe(t *testing.T) {
	// Build a body longer than maxBodyLen whose byte at the cut is mid-rune.
	body := strings.Repeat("a", maxBodyLen-1) + "€€€"
	got := sanitizeBody(body)
	if !utf8.ValidString(got) {
		t.Errorf("truncated body is not valid UTF-8")
	}
}
