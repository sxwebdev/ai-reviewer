// Package llm defines the provider-agnostic LLM client interface and the
// Claude CLI implementation. The review engine depends on this package; this
// package depends on nothing higher-level, so providers stay swappable.
package llm

// ReviewResponse is the strict JSON contract the model must return for a review.
type ReviewResponse struct {
	Summary               string        `json:"summary"`
	RiskLevel             string        `json:"risk_level"`             // low|medium|high|critical
	OverallRecommendation string        `json:"overall_recommendation"` // approve|comment|request_changes
	Findings              []Finding     `json:"findings"`
	MissingTests          []MissingTest `json:"missing_tests"`
	Questions             []Question    `json:"questions"`

	// Populated by the client, not the model.
	Raw     string  `json:"-"`
	CostUSD float64 `json:"-"`
}

// Finding is a single review comment candidate as emitted by the model. The
// model supplies only file/line intent; Go owns GitLab position generation.
type Finding struct {
	Severity           string   `json:"severity"` // blocking|high|medium|low|nit
	Category           string   `json:"category"` // correctness|security|tests|performance|architecture|maintainability|style|question|observability
	FilePath           string   `json:"file_path"`
	LineKind           string   `json:"line_kind"` // new|old|context|file
	Line               int      `json:"line"`
	LineEnd            int      `json:"line_end"`
	Title              string   `json:"title"`
	Body               string   `json:"body"`
	Suggestion         string   `json:"suggestion"`
	Confidence         float64  `json:"confidence"`
	Evidence           []string `json:"evidence"`
	Blocking           bool     `json:"blocking"`
	RequiresHumanCheck bool     `json:"requires_human_check"`

	// PassName tags which pipeline pass produced the finding. Set client-side
	// by the engine's merge stage; never sent to or from the model.
	PassName string `json:"-"`
}

// MissingTest describes an untested behaviour.
type MissingTest struct {
	Behavior      string  `json:"behavior"`
	SuggestedTest string  `json:"suggested_test"`
	FilePath      string  `json:"file_path"`
	Confidence    float64 `json:"confidence"`
}

// Question is an open question the reviewer wants answered.
type Question struct {
	Question     string `json:"question"`
	WhyItMatters string `json:"why_it_matters"`
	FilePath     string `json:"file_path"`
	Line         int    `json:"line"`
}

// CompletenessCriterion is one acceptance criterion extracted from the MR's
// stated intent, with the model's verdict on whether the diff fulfils it.
type CompletenessCriterion struct {
	Criterion string `json:"criterion"`
	Status    string `json:"status"`   // done|partial|missing|unclear
	Evidence  string `json:"evidence"` // code references / quotes backing the verdict
}

// CompletenessResponse is the strict JSON contract for the completeness pass.
type CompletenessResponse struct {
	Criteria []CompletenessCriterion `json:"criteria"`
	Note     string                  `json:"note"` // e.g. "description has no actionable criteria"
}

// CompletenessJSONSchema coerces the completeness pass output.
const CompletenessJSONSchema = `{
  "type": "object",
  "required": ["criteria"],
  "properties": {
    "criteria": {
      "type": "array",
      "items": {
        "type": "object",
        "required": ["criterion", "status", "evidence"],
        "properties": {
          "criterion": {"type": "string"},
          "status": {"type": "string", "enum": ["done", "partial", "missing", "unclear"]},
          "evidence": {"type": "string"}
        }
      }
    },
    "note": {"type": "string"}
  }
}`

// FindingVerdict is the skeptic pass's judgement of one draft finding. The
// skeptic can only judge findings by index — it cannot rewrite them.
type FindingVerdict struct {
	Index       int     `json:"index"`        // 1-based index from the numbered list
	Verdict     string  `json:"verdict"`      // confirmed|refuted|uncertain
	Reason      string  `json:"reason"`       // for refuted: quoted-code evidence
	Confidence  float64 `json:"confidence"`   // 0.0-1.0
	DuplicateOf int     `json:"duplicate_of"` // 1-based index of the finding this duplicates; 0/-1 when none
}

// VerdictResponse is the strict JSON contract for the skeptic pass.
type VerdictResponse struct {
	Verdicts []FindingVerdict `json:"verdicts"`
}

// VerdictJSONSchema coerces the skeptic pass output.
const VerdictJSONSchema = `{
  "type": "object",
  "required": ["verdicts"],
  "properties": {
    "verdicts": {
      "type": "array",
      "items": {
        "type": "object",
        "required": ["index", "verdict", "reason", "confidence"],
        "properties": {
          "index": {"type": "integer"},
          "verdict": {"type": "string", "enum": ["confirmed", "refuted", "uncertain"]},
          "reason": {"type": "string"},
          "confidence": {"type": "number"},
          "duplicate_of": {"type": "integer"}
        }
      }
    }
  }
}`

// ReviewJSONSchema is passed to `claude --json-schema` to coerce strict output.
// Kept intentionally permissive (types only) so model creativity in content is
// preserved while structure is enforced.
const ReviewJSONSchema = `{
  "type": "object",
  "required": ["summary", "risk_level", "overall_recommendation", "findings"],
  "properties": {
    "summary": {"type": "string"},
    "risk_level": {"type": "string", "enum": ["low", "medium", "high", "critical"]},
    "overall_recommendation": {"type": "string", "enum": ["approve", "comment", "request_changes"]},
    "findings": {
      "type": "array",
      "items": {
        "type": "object",
        "required": ["severity", "category", "file_path", "line_kind", "line", "title", "body", "confidence"],
        "properties": {
          "severity": {"type": "string", "enum": ["blocking", "high", "medium", "low", "nit"]},
          "category": {"type": "string"},
          "file_path": {"type": "string"},
          "line_kind": {"type": "string", "enum": ["new", "old", "context", "file"]},
          "line": {"type": "integer"},
          "line_end": {"type": "integer"},
          "title": {"type": "string"},
          "body": {"type": "string"},
          "suggestion": {"type": "string"},
          "confidence": {"type": "number"},
          "evidence": {"type": "array", "items": {"type": "string"}},
          "blocking": {"type": "boolean"},
          "requires_human_check": {"type": "boolean"}
        }
      }
    },
    "missing_tests": {"type": "array", "items": {"type": "object"}},
    "questions": {"type": "array", "items": {"type": "object"}}
  }
}`
