package review

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/sxwebdev/ai-reviewer/internal/llm"
)

// highFindings builds n distinct above-threshold findings on the in-diff file, so
// nothing but the cap can drop them.
func highFindings(n int) []llm.Finding {
	fs := make([]llm.Finding, 0, n)
	for i := range n {
		fs = append(fs, llm.Finding{
			Severity: "high", Category: "correctness", FilePath: "main.go", LineKind: "new", Line: 2,
			// Distinct titles: an identical title would be collapsed by the
			// fingerprint dedupe and the test would measure that instead of the cap.
			Title: fmt.Sprintf("issue %d", i), Body: "b", Confidence: 0.9,
		})
	}
	return fs
}

// TestValidatorRecordsFindingsCutByTheCap pins the cap as a suppression stage
// like any other. The cut used to be a plain `out = out[:MaxComments]`, which is
// the one drop that belongs to no stage at all: the findings passed every gate,
// were merely ranked too low, and vanished without a counter — so the log read
// "raw_findings: 30, validated: 12, suppressed: """ and an operator could not
// tell a review that found twelve things from one that discarded eighteen.
func TestValidatorRecordsFindingsCutByTheCap(t *testing.T) {
	const raw, limit = 5, 2
	v := NewValidator(ValidatorConfig{SeverityThreshold: "low", MaxComments: limit})
	kept, suppressed := v.Validate(&llm.ReviewResponse{Findings: highFindings(raw)},
		testFiles(t), testRefs, 1, 5, nil)

	if len(kept) != limit {
		t.Fatalf("kept = %d, want the cap of %d", len(kept), limit)
	}
	if len(suppressed) != raw-limit {
		t.Fatalf("suppressed = %d, want the %d findings the cap cut: %+v", len(suppressed), raw-limit, suppressed)
	}
	keptTitles := map[string]bool{}
	for _, f := range kept {
		keptTitles[f.Title] = true
	}
	for _, s := range suppressed {
		if s.Stage != SuppressMaxComments {
			t.Errorf("stage = %q, want %q", s.Stage, SuppressMaxComments)
		}
		if s.Reason == "" {
			t.Errorf("suppressed finding %q carries no reason", s.Title)
		}
		if keptTitles[s.Title] {
			t.Errorf("%q is reported both published and suppressed", s.Title)
		}
	}
}

// TestValidatorNeverReportsAFingerprintBothWays pins the one-outcome-per-
// fingerprint rule: a concern is published or suppressed, never both.
//
// The model routinely emits the same concern twice — once malformed or below
// threshold, once usable. Recording the weak copy while publishing the strong one
// makes the "also considered" list unreadable (an entry could mean "you did not
// see this" or "an earlier draft of something you did see") and makes
// ai_review_findings_suppressed_total count findings that were in fact delivered,
// which is not what its help text claims.
func TestValidatorNeverReportsAFingerprintBothWays(t *testing.T) {
	// Same file + category + title, so the two copies share a fingerprint; only
	// the usable one may survive, and it must survive silently.
	usable := llm.Finding{Severity: "high", Category: "correctness", FilePath: "main.go",
		LineKind: "new", Line: 2, Title: "same concern", Body: "a real body", Confidence: 0.9}
	malformed := usable
	malformed.Body = "   "
	weak := usable
	weak.Severity = "nit"

	cases := map[string][]llm.Finding{
		"malformed copy first":     {malformed, usable},
		"malformed copy second":    {usable, malformed},
		"below-threshold first":    {weak, usable},
		"below-threshold second":   {usable, weak},
		"duplicate of a kept copy": {usable, usable},
	}
	for name, findings := range cases {
		t.Run(name, func(t *testing.T) {
			v := NewValidator(ValidatorConfig{SeverityThreshold: "medium", MaxComments: 10})
			kept, suppressed := v.Validate(&llm.ReviewResponse{Findings: findings},
				testFiles(t), testRefs, 1, 5, nil)

			if len(kept) != 1 || kept[0].Title != "same concern" {
				t.Fatalf("kept = %+v, want the one usable copy", kept)
			}
			if len(suppressed) != 0 {
				t.Errorf("suppressed = %+v, want none: the concern was published, so nothing was lost",
					suppressed)
			}
		})
	}
}

// TestValidatorStillRecordsADropWhenNoCopySurvives is the other half of the rule:
// pruning must be conditional on something actually being published, or the fix
// would re-open the hole it closes — "raw_findings: 2, validated: 0" with an empty
// suppression breakdown.
func TestValidatorStillRecordsADropWhenNoCopySurvives(t *testing.T) {
	weak := llm.Finding{Severity: "nit", Category: "style", FilePath: "main.go",
		LineKind: "new", Line: 2, Title: "same concern", Body: "b", Confidence: 0.4}
	v := NewValidator(ValidatorConfig{SeverityThreshold: "medium", MaxComments: 10})
	kept, suppressed := v.Validate(&llm.ReviewResponse{Findings: []llm.Finding{weak, weak}},
		testFiles(t), testRefs, 1, 5, nil)

	if len(kept) != 0 {
		t.Fatalf("kept = %+v, want nothing above the threshold", kept)
	}
	if len(suppressed) != 1 || suppressed[0].Stage != SuppressThreshold {
		t.Fatalf("suppressed = %+v, want exactly one threshold drop", suppressed)
	}
}

// TestEngineCountsEveryFindingTheCapDropped is the end-to-end half: both cap
// sites (the validator's relaxed candidate cap and the engine's final
// MaxComments cut) have to land in Result.SuppressedCounts, and the tally has to
// survive the maxSuppressed cut on the stored list — the counters must say what
// was really dropped, not what fits in the "also considered" list.
func TestEngineCountsEveryFindingTheCapDropped(t *testing.T) {
	// Enough raw findings that the drops outnumber maxSuppressed, which is what
	// separates "counted before the cap" from "counted off the capped slice".
	const raw = maxSuppressed * 2
	profile := DefaultProfile()
	profile.MaxComments = 2

	eng := NewEngine(llm.NewFake(&llm.ReviewResponse{
		Summary: "s", RiskLevel: "medium", OverallRecommendation: "comment",
		Findings: highFindings(raw),
	}), discardLog())
	res, err := eng.Review(t.Context(), ReviewInput{
		ProjectID: 1, MRIID: 5, Files: testFiles(t), Refs: testRefs, Profile: profile,
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(res.Findings) != profile.MaxComments {
		t.Fatalf("findings = %d, want the cap of %d", len(res.Findings), profile.MaxComments)
	}
	wantDropped := raw - profile.MaxComments
	if got := res.SuppressedCounts[SuppressMaxComments]; got != wantDropped {
		t.Errorf("%s count = %d, want %d — every finding the cap cut, at both cap sites",
			SuppressMaxComments, got, wantDropped)
	}
	if len(res.Suppressed) > maxSuppressed {
		t.Errorf("stored suppressed = %d, want it still bounded at %d", len(res.Suppressed), maxSuppressed)
	}
	// The breakdown in the "review complete" log line is the reason the tally
	// exists; a stage missing from it is invisible where an operator looks first.
	fragment := fmt.Sprintf("%s=%d", SuppressMaxComments, wantDropped)
	if got := formatSuppressedCounts(res.SuppressedCounts); !strings.Contains(got, fragment) {
		t.Errorf("log breakdown = %q, want it to contain %q", got, fragment)
	}
	// Conservation: every raw finding is either published or counted under some
	// stage. Nothing may leave the pipeline unaccounted for. Exact here because
	// highFindings gives every finding its own fingerprint — repeated copies of one
	// concern collapse by design (one outcome per fingerprint), so conservation is a
	// statement about distinct concerns, not about raw JSON objects.
	accounted := len(res.Findings)
	for _, n := range res.SuppressedCounts {
		accounted += n
	}
	if accounted != raw {
		t.Errorf("accounted for %d of %d raw findings; counts = %v", accounted, raw, res.SuppressedCounts)
	}
}

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

// TestValidatorSuppressedDedupAndFileGate covers the two halves of the file gate
// that are easy to confuse.
//
// A finding about a file outside the diff is never *published* — that invariant is
// what "findings only on changed lines" means, and it is checked by `kept`. But it
// is now recorded as suppressed rather than discarded in silence: a review that
// cost real money and returned nothing has to be able to say why, and "the model
// wanted to comment on a file this MR does not touch" is the most common answer.
// Before this, `raw_findings: 1, validated: 0` appeared in the log with no
// explanation anywhere outside the database.
func TestValidatorSuppressedDedupAndFileGate(t *testing.T) {
	low := func(file string) llm.Finding {
		return llm.Finding{Severity: "low", Category: "style", FilePath: file, LineKind: "new", Line: 2,
			Title: "same low nit", Body: "b", Confidence: 0.5}
	}
	resp := &llm.ReviewResponse{Findings: []llm.Finding{
		low("main.go"),  // below threshold, in diff → one suppressed
		low("main.go"),  // exact repeat → must NOT add a second suppressed entry
		low("other.go"), // file not in diff → suppressed, and never publishable
	}}
	v := NewValidator(ValidatorConfig{SeverityThreshold: "medium", MaxComments: 10})
	kept, suppressed := v.Validate(resp, testFiles(t), testRefs, 1, 5, nil)

	if len(kept) != 0 {
		t.Fatalf("nothing should be kept, got %d", len(kept))
	}
	byStage := map[string]SuppressedFinding{}
	for _, s := range suppressed {
		if _, dup := byStage[s.Stage]; dup {
			t.Errorf("stage %q recorded twice; the repeated finding was not deduped", s.Stage)
		}
		byStage[s.Stage] = s
	}
	if len(suppressed) != 2 {
		t.Fatalf("want 2 suppressed (one threshold, one not-in-diff), got %d: %+v", len(suppressed), suppressed)
	}
	if got := byStage[SuppressThreshold]; got.FilePath != "main.go" {
		t.Errorf("threshold suppression = %+v, want the in-diff file", got)
	}
	if got := byStage[SuppressNotInDiff]; got.FilePath != "other.go" {
		t.Errorf("not-in-diff suppression = %+v, want other.go", got)
	}
}

// TestValidatorRecordsEmptyFindings pins the other formerly-silent drop. A model
// that returns findings with no body produces the same "validated: 0" as a model
// that found nothing, and the two need different reactions from an operator.
func TestValidatorRecordsEmptyFindings(t *testing.T) {
	resp := &llm.ReviewResponse{Findings: []llm.Finding{
		{Severity: "high", Category: "correctness", FilePath: "main.go", LineKind: "new", Line: 2,
			Title: "no body at all", Body: "   "},
		{Severity: "high", Category: "correctness", FilePath: "main.go", LineKind: "new", Line: 2,
			Title: "", Body: "a body with no title"},
	}}
	v := NewValidator(ValidatorConfig{SeverityThreshold: "medium", MaxComments: 10})
	kept, suppressed := v.Validate(resp, testFiles(t), testRefs, 1, 5, nil)

	if len(kept) != 0 {
		t.Fatalf("an unactionable finding must never be published, got %d", len(kept))
	}
	if len(suppressed) != 2 {
		t.Fatalf("want both malformed findings recorded, got %d: %+v", len(suppressed), suppressed)
	}
	for _, s := range suppressed {
		if s.Stage != SuppressEmpty {
			t.Errorf("stage = %q, want %q", s.Stage, SuppressEmpty)
		}
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

// TestSuppressedCountsAreNotTruncated is the reason the tally is taken before the
// cap rather than derived from the stored slice.
//
// Suppressed is ranked and cut to maxSuppressed so a noisy run cannot bloat the
// persisted review. Counting the cut slice would report at most that many drops
// and quietly understate exactly the run an operator is trying to understand — a
// review where the model produced dozens of findings and none survived.
func TestSuppressedCountsAreNotTruncated(t *testing.T) {
	n := maxSuppressed + 7
	fs := make([]SuppressedFinding, 0, n+1)
	for range n {
		fs = append(fs, SuppressedFinding{Stage: SuppressNotInDiff, Severity: "low"})
	}
	fs = append(fs, SuppressedFinding{Stage: SuppressThreshold, Severity: "low"})

	counts := countSuppressed(fs)
	if got := counts[SuppressNotInDiff]; got != n {
		t.Errorf("not_in_diff count = %d, want %d (the full tally, not the capped list)", got, n)
	}
	if got := counts[SuppressThreshold]; got != 1 {
		t.Errorf("threshold count = %d, want 1", got)
	}

	// And the stored slice is still bounded, which is the other half of the deal.
	capped := rankSuppressed(fs)
	if len(capped) > maxSuppressed {
		capped = capped[:maxSuppressed]
	}
	if len(capped) != maxSuppressed {
		t.Errorf("stored suppressed = %d, want it capped at %d", len(capped), maxSuppressed)
	}
}

func TestFormatSuppressedCounts(t *testing.T) {
	if got := formatSuppressedCounts(nil); got != "" {
		t.Errorf("empty tally rendered %q, want an empty string so a clean run stays quiet", got)
	}
	// Sorted by stage so two log lines can be compared by eye.
	got := formatSuppressedCounts(map[string]int{
		SuppressThreshold: 1, SuppressNotInDiff: 2, SuppressEmpty: 3,
	})
	if want := "empty=3 not_in_diff=2 threshold=1"; got != want {
		t.Errorf("rendered %q, want %q", got, want)
	}
}
