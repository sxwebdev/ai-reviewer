package review

import (
	"fmt"
	"sort"
	"strings"

	"github.com/sxwebdev/ai-reviewer/internal/gitlab"
	"github.com/sxwebdev/ai-reviewer/internal/llm"
	"github.com/sxwebdev/ai-reviewer/internal/security"
)

// maxBodyLen caps a comment body so the model cannot post an essay.
const maxBodyLen = 4000

// ValidatorConfig configures deterministic validation.
type ValidatorConfig struct {
	SeverityThreshold string
	MaxComments       int
}

// Validator turns raw LLM findings into validated, position-mapped, deduped,
// ranked findings. This is where Go — not the model — owns correctness: file
// existence in the diff, line mapping, severity threshold, dedupe, secret
// scrubbing, body length, and the max-comments cap.
type Validator struct {
	cfg ValidatorConfig
}

// NewValidator builds a Validator.
func NewValidator(cfg ValidatorConfig) *Validator {
	if cfg.MaxComments <= 0 {
		cfg.MaxComments = 12
	}
	if cfg.SeverityThreshold == "" {
		cfg.SeverityThreshold = "medium"
	}
	return &Validator{cfg: cfg}
}

// Validate processes resp.Findings against the diff and returns the validated
// set. existing is the set of fingerprints already present (prior reviews or
// existing discussions) to dedupe against; findings whose file is not in the
// changed set are dropped (we do not comment on pre-existing code).
//
// Exactly one outcome per fingerprint: the two returned lists never name the same
// concern (see the "kept wins" pass at the end).
func (v *Validator) Validate(
	resp *llm.ReviewResponse,
	files []*FileDiff,
	refs gitlab.DiffRefs,
	projectID, mrIID int64,
	existing map[string]bool,
) ([]ValidatedFinding, []SuppressedFinding) {
	threshold := SeverityRank(v.cfg.SeverityThreshold)
	seen := map[string]bool{}    // dedup for kept findings
	seenSup := map[string]bool{} // dedup for suppressed findings
	var out []ValidatedFinding

	// Suppressions are collected with their fingerprint rather than appended
	// straight to the result, because whether a drop is real is not yet known while
	// the loop runs: a later copy of the same concern can still be published, and
	// the fingerprint is what recognises it. It stays a local — SuppressedFinding
	// is what gets persisted and read by humans, and a fingerprint there would be
	// routing detail in a display type.
	type candidate struct {
		fp  string
		sup SuppressedFinding
	}
	var dropped []candidate

	// drop records a suppression once per fingerprint, so a repeated finding does
	// not fill the "also considered" list with identical copies.
	drop := func(f llm.Finding, severity, fp, stage, reason string) {
		if seenSup[fp] {
			return
		}
		seenSup[fp] = true
		dropped = append(dropped, candidate{fp: fp, sup: suppressedFrom(f, severity, stage, reason)})
	}

	for _, f := range resp.Findings {
		severity := NormalizeSeverity(f.Severity)
		// Computed before the gates below so every suppression path can dedupe by
		// it. It depends only on model-supplied fields, not on the diff.
		fp := Fingerprint(projectID, mrIID, f.FilePath, f.Category, f.Title)

		if strings.TrimSpace(f.Title) == "" || strings.TrimSpace(f.Body) == "" {
			// Not actionable, and recorded rather than dropped in silence: a run
			// whose findings all arrive malformed looks identical to a clean run
			// otherwise.
			drop(f, severity, fp, SuppressEmpty,
				"the model returned a finding with no title or no body")
			continue
		}
		// File-in-diff is the first gate: we never surface (nor comment on) a
		// finding about code outside the changed set, regardless of severity.
		fd := FindFileDiff(files, f.FilePath)
		if fd == nil {
			// Still dropped — that is the invariant — but no longer invisibly. This
			// is the most common way a paid-for review returns nothing at all, and
			// it names the file the model wanted to talk about.
			drop(f, severity, fp, SuppressNotInDiff,
				fmt.Sprintf("%s is not in this merge request's diff", f.FilePath))
			continue
		}

		// Blocking findings are a floor: they always pass the threshold, so a
		// finding flagged critical/blocking is never dropped by a label mismatch.
		if !f.Blocking && SeverityRank(severity) < threshold {
			// Real but low-severity: keep it as informational context instead of
			// discarding it silently.
			drop(f, severity, fp, SuppressThreshold,
				fmt.Sprintf("severity %s is below the %s threshold", severity, v.cfg.SeverityThreshold))
			continue
		}
		if existing[fp] {
			// Matches a prior review's finding / an existing discussion: kept out
			// of the comment flow (anti-spam) but shown so the reviewer sees it
			// was raised again.
			drop(f, severity, fp, SuppressDuplicate,
				"already raised in a prior review or an existing discussion")
			continue
		}
		if seen[fp] {
			continue // duplicate within this same response — pure noise
		}
		seen[fp] = true

		pos, outcome := MapPosition(fd, refs, LineIntent{
			FilePath: f.FilePath, Line: f.Line, LineKind: f.LineKind,
		})

		vf := ValidatedFinding{
			Source:      f,
			Title:       f.Title,
			Body:        sanitizeBody(f.Body),
			Suggestion:  f.Suggestion,
			Severity:    severity,
			Category:    strings.ToLower(f.Category),
			Confidence:  clamp01(f.Confidence),
			FilePath:    f.FilePath,
			Position:    pos,
			Outcome:     outcome,
			Fingerprint: fp,
			Pass:        f.PassName,
		}
		switch outcome.Kind {
		case MapOverview:
			vf.ValidationError = "no inline anchor: " + outcome.Reason
		case MapSnapped:
			// Position is real but relocated to the nearest changed line — mark
			// it so the reviewer knows the anchor is approximate.
			vf.ValidationError = "approximate location: " + outcome.Reason
		}
		out = append(out, vf)
	}

	// "Kept wins": a fingerprint that reached the validated set is not also reported
	// as suppressed. The model routinely emits one concern twice — once malformed or
	// below threshold, once usable — and recording the weak copy alongside the
	// published one gave the "also considered" list two incompatible meanings ("you
	// did not see this" vs "an earlier draft of something you did see") and made
	// ai_review_findings_suppressed_total count findings that were in fact
	// delivered. Both orders matter, which is why this is a pass over the collected
	// drops rather than a check inside the loop: the usable copy may arrive after
	// the weak one, and the empty/threshold gates run before the dedupe gate.
	//
	// Conditional on something surviving, so a concern whose every copy was dropped
	// is still recorded — that is the "validated: 0" case the tally exists for.
	var suppressed []SuppressedFinding
	for _, d := range dropped {
		if seen[d.fp] {
			continue
		}
		suppressed = append(suppressed, d.sup)
	}

	rankFindings(out)
	out, capped := capFindings(out, v.cfg.MaxComments,
		fmt.Sprintf("ranked outside the %d findings this validation pass carries forward", v.cfg.MaxComments))
	// Appended after the pruning pass: a cap-cut finding was kept by the loop, so
	// `seen` holds its fingerprint and pruning would erase the very drop that
	// explains the review. The cap is also the last word on those fingerprints —
	// nothing above can have recorded them, since a fingerprint recorded as
	// suppressed never entered `out`.
	suppressed = append(suppressed, capped...)
	return out, suppressed
}

// capFindings truncates a *ranked* list to limit and returns everything it cut as
// suppressed findings. Both cap sites go through it — the validator's relaxed
// candidate cap and the engine's final MaxComments cut — because the cap is the
// one drop that has no gate of its own: the findings passed every check and were
// merely ranked too low, so a plain `fs = fs[:limit]` left them counted under no
// suppression stage and the log line read "raw_findings: 30, validated: 12,
// suppressed: """. That is precisely the "found something and threw it away" case
// SuppressedCounts exists to make answerable without a database query.
//
// The caller must rank first: the cut keeps the head of the slice, so the order
// is what decides which findings survive. A non-positive limit means "no cap"
// rather than "drop everything" — the engine and NewValidator both apply their
// own floor, and a zero here must never silently discard a whole review.
func capFindings(fs []ValidatedFinding, limit int, reason string) ([]ValidatedFinding, []SuppressedFinding) {
	if limit <= 0 || len(fs) <= limit {
		return fs, nil
	}
	dropped := make([]SuppressedFinding, 0, len(fs)-limit)
	for _, f := range fs[limit:] {
		dropped = append(dropped, suppressedFromValidated(f, SuppressMaxComments, reason))
	}
	return fs[:limit], dropped
}

// suppressedFrom builds a SuppressedFinding from a raw LLM finding dropped before
// position mapping. The body is scrubbed and length-capped exactly like a kept
// finding, so the "no secrets in output" invariant holds for suppressed items too.
func suppressedFrom(f llm.Finding, severity, stage, reason string) SuppressedFinding {
	return SuppressedFinding{
		Title:    strings.TrimSpace(f.Title),
		Body:     sanitizeBody(f.Body),
		Severity: severity,
		Category: strings.ToLower(strings.TrimSpace(f.Category)),
		FilePath: f.FilePath,
		Pass:     f.PassName,
		Stage:    stage,
		Reason:   reason,
	}
}

// clamp01 bounds a model-supplied confidence to [0,1] — Go owns validation of
// model numbers; the JSON schema bounds are advisory, not trusted.
func clamp01(v float64) float64 {
	return min(max(v, 0), 1)
}

// rankFindings sorts by severity (desc), then verification state
// (confirmed > unverified/none > uncertain), then confidence (desc).
func rankFindings(fs []ValidatedFinding) {
	sort.SliceStable(fs, func(i, j int) bool {
		ri, rj := SeverityRank(fs[i].Severity), SeverityRank(fs[j].Severity)
		if ri != rj {
			return ri > rj
		}
		vi, vj := verificationRank(fs[i].Verification), verificationRank(fs[j].Verification)
		if vi != vj {
			return vi > vj
		}
		return fs[i].Confidence > fs[j].Confidence
	})
}

// maxSuppressed bounds the informational "also considered" list stored per review.
const maxSuppressed = 20

// rankSuppressed orders suppressed findings most-severe first for display.
func rankSuppressed(fs []SuppressedFinding) []SuppressedFinding {
	sort.SliceStable(fs, func(i, j int) bool {
		return SeverityRank(fs[i].Severity) > SeverityRank(fs[j].Severity)
	})
	return fs
}

func verificationRank(v string) int {
	switch v {
	case VerificationConfirmed:
		return 2
	case VerificationUncertain:
		return 0
	default: // "" or unverified
		return 1
	}
}

// sanitizeBody masks secrets and caps the length of a comment body.
func sanitizeBody(body string) string {
	return security.Truncate(security.Mask(strings.TrimSpace(body)), maxBodyLen)
}
