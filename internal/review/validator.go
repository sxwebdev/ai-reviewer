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
func (v *Validator) Validate(
	resp *llm.ReviewResponse,
	files []*FileDiff,
	refs gitlab.DiffRefs,
	projectID, mrIID int64,
	existing map[string]bool,
) ([]ValidatedFinding, []SuppressedFinding) {
	threshold := SeverityRank(v.cfg.SeverityThreshold)
	seen := map[string]bool{}    // dedup for kept findings
	seenSup := map[string]bool{} // dedup for suppressed findings (independent set)
	var out []ValidatedFinding
	var suppressed []SuppressedFinding

	for _, f := range resp.Findings {
		if strings.TrimSpace(f.Title) == "" || strings.TrimSpace(f.Body) == "" {
			continue // empty findings are not actionable
		}
		severity := NormalizeSeverity(f.Severity)
		// File-in-diff is the first gate: we never surface (nor comment on) a
		// finding about code outside the changed set, regardless of severity.
		fd := FindFileDiff(files, f.FilePath)
		if fd == nil {
			continue // file not in the changed set — do not comment on pre-existing code
		}
		fp := Fingerprint(projectID, mrIID, f.FilePath, f.Category, f.Title)

		// Blocking findings are a floor: they always pass the threshold, so a
		// finding flagged critical/blocking is never dropped by a label mismatch.
		if !f.Blocking && SeverityRank(severity) < threshold {
			// Real but low-severity: keep it as informational context instead of
			// discarding it silently. seenSup dedupes so a repeated finding does
			// not fill the "also considered" list with identical copies.
			if !seenSup[fp] {
				seenSup[fp] = true
				suppressed = append(suppressed, suppressedFrom(f, severity, SuppressThreshold,
					fmt.Sprintf("severity %s is below the %s threshold", severity, v.cfg.SeverityThreshold)))
			}
			continue
		}
		if existing[fp] {
			// Matches a prior review's finding / an existing discussion: kept out
			// of the comment flow (anti-spam) but shown so the reviewer sees it
			// was raised again.
			if !seenSup[fp] {
				seenSup[fp] = true
				suppressed = append(suppressed, suppressedFrom(f, severity, SuppressDuplicate,
					"already raised in a prior review or an existing discussion"))
			}
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

	rankFindings(out)
	if len(out) > v.cfg.MaxComments {
		out = out[:v.cfg.MaxComments]
	}
	return out, suppressed
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
