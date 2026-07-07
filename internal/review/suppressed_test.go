package review

import (
	"context"
	"strings"
	"testing"

	"github.com/sxwebdev/ai-reviewer/internal/llm"
)

// TestValidatorCapturesThresholdAndDuplicateDrops proves that below-threshold and
// prior-duplicate findings are retained as SuppressedFinding (with the right
// stage) rather than silently discarded, and that a dropped body is still
// secret-scrubbed.
func TestValidatorCapturesThresholdAndDuplicateDrops(t *testing.T) {
	resp := &llm.ReviewResponse{Findings: []llm.Finding{
		// Surrounding whitespace proves the body ran through sanitizeBody (which
		// trims + scrubs), i.e. suppressed bodies get the same treatment as kept.
		{Severity: "low", Category: "style", FilePath: "main.go", LineKind: "new", Line: 2,
			Title: "low nit", Body: "  needs trimming  ", Confidence: 0.5},
		{Severity: "high", Category: "correctness", FilePath: "main.go", LineKind: "new", Line: 3,
			Title: "kept blocker", Body: "serious", Confidence: 0.9},
	}}
	v := NewValidator(ValidatorConfig{SeverityThreshold: "medium", MaxComments: 10})
	kept, suppressed := v.Validate(resp, testFiles(t), testRefs, 1, 5, nil)

	if len(kept) != 1 || kept[0].Title != "kept blocker" {
		t.Fatalf("kept set wrong: %+v", kept)
	}
	if len(suppressed) != 1 {
		t.Fatalf("want 1 suppressed (below-threshold), got %d", len(suppressed))
	}
	s := suppressed[0]
	if s.Stage != SuppressThreshold {
		t.Errorf("stage = %q, want %q", s.Stage, SuppressThreshold)
	}
	if s.Title != "low nit" || s.Severity != "low" {
		t.Errorf("suppressed finding fields wrong: %+v", s)
	}
	if s.Body != sanitizeBody("  needs trimming  ") || s.Body != "needs trimming" {
		t.Errorf("suppressed body not sanitized like a kept finding: %q", s.Body)
	}

	// Same finding, now pre-seeded as an existing fingerprint → suppressed as duplicate.
	mk := llm.Finding{Severity: "high", Category: "correctness", FilePath: "main.go",
		LineKind: "new", Line: 2, Title: "dup issue", Body: "b", Confidence: 0.8}
	fp := Fingerprint(1, 5, mk.FilePath, mk.Category, mk.Title)
	kept2, sup2 := v.Validate(&llm.ReviewResponse{Findings: []llm.Finding{mk}}, testFiles(t), testRefs, 1, 5,
		map[string]bool{fp: true})
	if len(kept2) != 0 {
		t.Fatalf("existing-fingerprint finding must not be kept, got %d", len(kept2))
	}
	if len(sup2) != 1 || sup2[0].Stage != SuppressDuplicate {
		t.Fatalf("want 1 duplicate-suppressed, got %+v", sup2)
	}
}

// TestValidatorSuppressedDedupAndFileGate proves suppressed capture dedupes a
// repeated finding and never surfaces a finding about a file outside the diff.
func TestValidatorSuppressedDedupAndFileGate(t *testing.T) {
	low := func(file string) llm.Finding {
		return llm.Finding{Severity: "low", Category: "style", FilePath: file, LineKind: "new", Line: 2,
			Title: "same low nit", Body: "b", Confidence: 0.5}
	}
	resp := &llm.ReviewResponse{Findings: []llm.Finding{
		low("main.go"),  // below threshold, in diff → one suppressed
		low("main.go"),  // exact repeat → must NOT add a second suppressed entry
		low("other.go"), // below threshold but file not in diff → must be dropped, not surfaced
	}}
	v := NewValidator(ValidatorConfig{SeverityThreshold: "medium", MaxComments: 10})
	kept, suppressed := v.Validate(resp, testFiles(t), testRefs, 1, 5, nil)

	if len(kept) != 0 {
		t.Fatalf("nothing should be kept, got %d", len(kept))
	}
	if len(suppressed) != 1 {
		t.Fatalf("want exactly 1 suppressed (deduped, file-gated), got %d: %+v", len(suppressed), suppressed)
	}
	if suppressed[0].FilePath != "main.go" || suppressed[0].Stage != SuppressThreshold {
		t.Errorf("wrong suppressed item: %+v", suppressed[0])
	}
}

// TestSkepticSuppressesRefutedNonBlocking proves a refuted non-blocking finding
// moves to the suppressed set, while a refuted blocker is demoted-and-kept (never
// suppressed).
func TestSkepticSuppressesRefutedNonBlocking(t *testing.T) {
	batch := []ValidatedFinding{
		vf("high", "refuted high", 0.9),
		vf("blocking", "refuted blocker", 0.9),
	}
	verdicts := []llm.FindingVerdict{
		{Index: 1, Verdict: "refuted", Reason: "code guards this"},
		{Index: 2, Verdict: "refuted", Reason: "also fine"},
	}
	kept, suppressed := applyVerdicts(batch, verdicts, discardLog())

	keptTitles := map[string]bool{}
	for _, f := range kept {
		keptTitles[f.Title] = true
	}
	if keptTitles["refuted high"] {
		t.Error("refuted non-blocking must not be kept")
	}
	if !keptTitles["refuted blocker"] {
		t.Error("refuted blocker must be demoted-and-kept, not suppressed")
	}
	if len(suppressed) != 1 || suppressed[0].Title != "refuted high" || suppressed[0].Stage != SuppressSkeptic {
		t.Fatalf("want the refuted non-blocking in suppressed, got %+v", suppressed)
	}
	if !strings.Contains(suppressed[0].Reason, "code guards this") {
		t.Errorf("suppressed reason should carry the skeptic reason: %q", suppressed[0].Reason)
	}
}

// dropVerifier is a stub Verifier that always refutes, to exercise the
// runVerifiers suppression path without a toolchain.
type dropVerifier struct{}

func (dropVerifier) Name() string                    { return "stub" }
func (dropVerifier) Applies(f ValidatedFinding) bool { return true }
func (dropVerifier) Verify(context.Context, string, ValidatedFinding) VerifierResult {
	return VerifierResult{Verdict: VerdictDrop, Note: "not real"}
}

func TestRunVerifiersCapturesDrops(t *testing.T) {
	findings := []ValidatedFinding{
		{Title: "refuted by tool", Body: "x", Severity: "high", FilePath: "a.go"},
	}
	kept, suppressed := runVerifiers(context.Background(), "wd", []Verifier{dropVerifier{}}, findings, discardLog())
	if len(kept) != 0 {
		t.Fatalf("dropped finding must not be kept, got %d", len(kept))
	}
	if len(suppressed) != 1 || suppressed[0].Stage != SuppressVerifier {
		t.Fatalf("want 1 verifier-suppressed, got %+v", suppressed)
	}
	if !strings.Contains(suppressed[0].Reason, "not real") {
		t.Errorf("reason should carry the verifier note: %q", suppressed[0].Reason)
	}
}
