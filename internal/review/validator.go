package review

import (
	"sort"
	"strings"
	"unicode/utf8"

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
) []ValidatedFinding {
	threshold := SeverityRank(v.cfg.SeverityThreshold)
	seen := map[string]bool{}
	var out []ValidatedFinding

	for _, f := range resp.Findings {
		if strings.TrimSpace(f.Title) == "" || strings.TrimSpace(f.Body) == "" {
			continue // empty findings are not actionable
		}
		severity := NormalizeSeverity(f.Severity)
		// Blocking findings are a floor: they always pass the threshold, so a
		// finding flagged critical/blocking is never dropped by a label mismatch.
		if !f.Blocking && SeverityRank(severity) < threshold {
			continue // below severity threshold
		}
		fd := FindFileDiff(files, f.FilePath)
		if fd == nil {
			continue // file not in the changed set — do not comment on pre-existing code
		}

		fp := Fingerprint(projectID, mrIID, f.FilePath, f.Category, f.Title)
		if existing[fp] || seen[fp] {
			continue // duplicate of a prior finding / existing discussion / within this response
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
			Confidence:  f.Confidence,
			FilePath:    f.FilePath,
			Position:    pos,
			Outcome:     outcome,
			Fingerprint: fp,
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
	return out
}

// rankFindings sorts by severity (desc) then confidence (desc).
func rankFindings(fs []ValidatedFinding) {
	sort.SliceStable(fs, func(i, j int) bool {
		ri, rj := SeverityRank(fs[i].Severity), SeverityRank(fs[j].Severity)
		if ri != rj {
			return ri > rj
		}
		return fs[i].Confidence > fs[j].Confidence
	})
}

// sanitizeBody masks secrets and caps the length of a comment body, truncating
// on a UTF-8 rune boundary so the result is always valid UTF-8.
func sanitizeBody(body string) string {
	body = security.Mask(strings.TrimSpace(body))
	if len(body) > maxBodyLen {
		cut := maxBodyLen
		for cut > 0 && !utf8.RuneStart(body[cut]) {
			cut--
		}
		body = body[:cut] + "…"
	}
	return body
}
