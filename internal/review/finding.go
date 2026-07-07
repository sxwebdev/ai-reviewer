package review

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"

	"github.com/sxwebdev/ai-reviewer/internal/gitlab"
	"github.com/sxwebdev/ai-reviewer/internal/llm"
)

// severityRank orders severities; higher is more severe. "critical" is included
// because models frequently emit it (the risk_level enum uses it) even though
// the finding severity enum does not.
var severityRank = map[string]int{
	"nit":      1,
	"low":      2,
	"medium":   3,
	"high":     4,
	"blocking": 5,
	"critical": 6,
}

// SeverityRank returns the rank of a severity (0 for unknown).
func SeverityRank(sev string) int { return severityRank[strings.ToLower(strings.TrimSpace(sev))] }

// NormalizeSeverity maps an empty/unknown severity label to "medium" so a
// finding is never silently dropped by the threshold because the model used a
// slightly-off label; known labels are returned lowercased.
func NormalizeSeverity(sev string) string {
	s := strings.ToLower(strings.TrimSpace(sev))
	if _, ok := severityRank[s]; ok {
		return s
	}
	return "medium"
}

// Verification states for ValidatedFinding.Verification.
const (
	VerificationConfirmed  = "confirmed"  // skeptic confirmed with evidence
	VerificationUncertain  = "uncertain"  // skeptic could not confirm nor refute
	VerificationUnverified = "unverified" // verification skipped or failed
)

// ValidatedFinding is an LLM finding after deterministic validation and GitLab
// position mapping. Position is nil for overview-only findings.
type ValidatedFinding struct {
	Source          llm.Finding
	Title           string
	Body            string
	Suggestion      string
	Severity        string
	Category        string
	Confidence      float64
	FilePath        string
	Position        *gitlab.Position
	Outcome         MapOutcome
	Fingerprint     string
	ValidationError string

	// Pass names the pipeline pass that produced the finding; Verification is
	// the skeptic outcome ("" when no verification ran).
	Pass         string
	Verification string
}

// IsOverview reports whether the finding has no inline anchor.
func (f ValidatedFinding) IsOverview() bool { return f.Position == nil }

// Suppression stages: where in the pipeline a finding was dropped. Surfaced
// read-only in the UI so a real-but-filtered concern is not silently lost.
const (
	SuppressThreshold = "threshold" // below the severity threshold
	SuppressDuplicate = "duplicate" // fingerprint of a prior finding / already raised
	SuppressSkeptic   = "skeptic"   // skeptic refuted / marked a duplicate (non-blocking)
	SuppressVerifier  = "verifier"  // a deterministic verifier refuted it (e.g. clean build)
)

// SuppressedFinding is a finding the pipeline dropped, retained with the stage
// and reason it was dropped so the UI can show it as informational context. It
// never anchors to a diff line and is never publishable.
type SuppressedFinding struct {
	Title    string `json:"title"`
	Body     string `json:"body"`
	Severity string `json:"severity"`
	Category string `json:"category"`
	FilePath string `json:"file_path"`
	Pass     string `json:"pass"`   // provenance pass that produced it
	Stage    string `json:"stage"`  // one of the Suppress* constants
	Reason   string `json:"reason"` // human-readable detail
}

// suppressedFromValidated builds a SuppressedFinding from a finding dropped after
// validation (skeptic/verifier stages). Its body is already scrubbed and capped.
func suppressedFromValidated(f ValidatedFinding, stage, reason string) SuppressedFinding {
	return SuppressedFinding{
		Title:    f.Title,
		Body:     f.Body,
		Severity: f.Severity,
		Category: f.Category,
		FilePath: f.FilePath,
		Pass:     f.Pass,
		Stage:    stage,
		Reason:   reason,
	}
}

// Fingerprint produces a stable, line-insensitive identity for a finding so
// that the same issue dedupes across head shas and against existing
// discussions. Intentionally excludes the line number (lines shift between
// revisions) and normalizes the title.
func Fingerprint(projectID, mrIID int64, filePath, category, title string) string {
	h := sha256.New()
	h.Write([]byte(strconv.FormatInt(projectID, 10)))
	h.Write([]byte{0})
	h.Write([]byte(strconv.FormatInt(mrIID, 10)))
	h.Write([]byte{0})
	h.Write([]byte(strings.ToLower(strings.TrimSpace(filePath))))
	h.Write([]byte{0})
	h.Write([]byte(strings.ToLower(strings.TrimSpace(category))))
	h.Write([]byte{0})
	h.Write([]byte(normalizeTitle(title)))
	return hex.EncodeToString(h.Sum(nil))
}

// normalizeTitle lowercases, trims, and collapses internal whitespace.
func normalizeTitle(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}
