package review

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sxwebdev/ai-reviewer/internal/llm"
)

// Verify modes for PipelineConfig.VerifyMode.
const (
	VerifyOff     = "off"
	VerifyReflect = "reflect" // second self-pruning pass on the model's own JSON
	VerifySkeptic = "skeptic" // agent-mode refute/confirm pass against the worktree
)

// candidateMultiplier relaxes the validator cap (multiplier × MaxComments) so
// later verification stages choose the final list from verified survivors.
const candidateMultiplier = 2

// Completeness modes for PipelineConfig.Completeness. The distinction matters
// at the engine: "on" always runs the audit, "auto" additionally requires the
// MR to carry enough stated intent (hasIntentText).
const (
	CompletenessOff  = "off"
	CompletenessAuto = "auto"
	CompletenessOn   = "on"
)

// PipelineConfig controls the multi-pass review pipeline. The zero value is
// the cheap single-pass mode with verification off (previous behaviour).
type PipelineConfig struct {
	Passes            []string // pass names; empty => ["general"]
	MaxParallel       int      // concurrent LLM passes (default 2)
	VerifyMode        string   // off | reflect | skeptic (default off)
	VerifyMaxFindings int      // cap on findings sent to the skeptic (default 24)
	Verifiers         []string // deterministic verifier names (default ["go_build"])
	Completeness      string   // acceptance-criteria audit: off | auto | on (default off)
}

func (pc PipelineConfig) withDefaults() PipelineConfig {
	if len(pc.Passes) == 0 {
		pc.Passes = []string{PassGeneral}
	}
	if pc.MaxParallel <= 0 {
		pc.MaxParallel = 2
	}
	if pc.VerifyMode == "" {
		pc.VerifyMode = VerifyOff
	}
	if pc.VerifyMaxFindings <= 0 {
		pc.VerifyMaxFindings = 24
	}
	if pc.Verifiers == nil {
		pc.Verifiers = []string{"go_build"}
	}
	if pc.Completeness == "" {
		pc.Completeness = CompletenessOff
	}
	return pc
}

// PassReport is per-pass accounting persisted with the review for provenance
// and cost visibility.
type PassReport struct {
	Name        string  `json:"name"`
	CostUSD     float64 `json:"cost_usd"`
	DurationMS  int64   `json:"duration_ms"`
	RawFindings int     `json:"raw_findings"`
	Err         string  `json:"error,omitempty"`
}

// passOutcome pairs a pass spec with its LLM response (or error).
type passOutcome struct {
	spec PassSpec
	resp *llm.ReviewResponse
	err  error
}

// runPasses executes the review passes with bounded concurrency. Every pass
// runs to completion regardless of other passes' failures; per-pass errors are
// captured in the outcome and report.
func (e *Engine) runPasses(ctx context.Context, in ReviewInput, specs []PassSpec, maxParallel int) ([]passOutcome, []PassReport) {
	outcomes := make([]passOutcome, len(specs))
	reports := make([]PassReport, len(specs))
	sem := make(chan struct{}, maxParallel)
	var wg sync.WaitGroup

	// The user prompt (potentially hundreds of KB of rendered context) and the
	// base system prompt are identical for every pass — build them once; only
	// the specialist suffix differs per pass.
	userPrompt := BuildUserPrompt(in)
	baseSystem := BuildSystemPrompt(in.Profile)

	for i, spec := range specs {
		wg.Add(1)
		go func(i int, spec PassSpec) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			start := time.Now()
			resp, err := e.client.Review(ctx, llm.Request{
				System:       baseSystem + spec.SystemSuffix,
				Prompt:       userPrompt,
				Model:        in.Model,
				WorkDir:      in.WorkDir,
				AgentMode:    in.AgentMode,
				AllowedTools: in.AllowedTools,
				Skills:       in.Skills,
				JSONSchema:   llm.ReviewJSONSchema,
			})
			outcomes[i] = passOutcome{spec: spec, resp: resp, err: err}
			rep := PassReport{Name: spec.Name, DurationMS: time.Since(start).Milliseconds()}
			if err != nil {
				rep.Err = err.Error()
				e.log.Warn("review pass failed", "pass", spec.Name, "err", err)
			} else {
				rep.CostUSD = resp.CostUSD
				rep.RawFindings = len(resp.Findings)
			}
			reports[i] = rep
		}(i, spec)
	}
	wg.Wait()
	return outcomes, reports
}

// mergeResponses merges successful pass responses into one ReviewResponse:
// the primary pass supplies summary/risk/recommendation, findings are
// concatenated with cross-pass dedupe (relaxed key: file + normalized title,
// category-insensitive — two passes reporting the same bug under different
// categories collapse to the more severe instance), and costs are summed.
// Pure; callers guarantee at least one successful outcome.
func mergeResponses(outcomes []passOutcome) *llm.ReviewResponse {
	merged := &llm.ReviewResponse{}
	primaryFound := false

	for _, o := range outcomes {
		if o.err != nil || o.resp == nil {
			continue
		}
		if o.spec.Primary && !primaryFound {
			merged.Summary = o.resp.Summary
			merged.RiskLevel = o.resp.RiskLevel
			merged.OverallRecommendation = o.resp.OverallRecommendation
			primaryFound = true
		}
		merged.CostUSD += o.resp.CostUSD
		for _, f := range o.resp.Findings {
			f.PassName = o.spec.Name
			merged.Findings = append(merged.Findings, f)
		}
		merged.MissingTests = append(merged.MissingTests, o.resp.MissingTests...)
		merged.Questions = append(merged.Questions, o.resp.Questions...)
	}
	if !primaryFound {
		synthesizeSummary(merged, outcomes)
	}
	merged.Findings = dedupeAcrossPasses(merged.Findings)

	if raw, err := json.Marshal(merged); err == nil {
		merged.Raw = string(raw)
	}
	return merged
}

// dedupeAcrossPasses collapses findings that share file + normalized title,
// keeping the more severe (then more confident) instance. Order of survivors
// follows first appearance.
func dedupeAcrossPasses(findings []llm.Finding) []llm.Finding {
	type slot struct{ idx int }
	byKey := map[string]slot{}
	var out []llm.Finding
	for _, f := range findings {
		key := strings.ToLower(strings.TrimSpace(f.FilePath)) + "\x00" + normalizeTitle(f.Title)
		if s, ok := byKey[key]; ok {
			cur := out[s.idx]
			if SeverityRank(NormalizeSeverity(f.Severity)) > SeverityRank(NormalizeSeverity(cur.Severity)) ||
				(SeverityRank(NormalizeSeverity(f.Severity)) == SeverityRank(NormalizeSeverity(cur.Severity)) && f.Confidence > cur.Confidence) {
				out[s.idx] = f
			}
			continue
		}
		byKey[key] = slot{idx: len(out)}
		out = append(out, f)
	}
	return out
}

// synthesizeSummary fills summary/risk/recommendation from the merged findings
// when the primary pass failed: risk = max severity seen, recommendation =
// comment when anything was found.
func synthesizeSummary(merged *llm.ReviewResponse, outcomes []passOutcome) {
	var names []string
	maxRank := 0
	for _, o := range outcomes {
		if o.err != nil || o.resp == nil {
			continue
		}
		names = append(names, o.spec.Name)
		for _, f := range o.resp.Findings {
			if r := SeverityRank(NormalizeSeverity(f.Severity)); r > maxRank {
				maxRank = r
			}
		}
	}
	sort.Strings(names)
	merged.Summary = "Automated multi-pass review (primary pass unavailable); findings collected from: " + strings.Join(names, ", ") + "."
	switch {
	case maxRank >= SeverityRank("blocking"):
		merged.RiskLevel = "critical"
		merged.OverallRecommendation = "request_changes"
	case maxRank >= SeverityRank("high"):
		merged.RiskLevel = "high"
		merged.OverallRecommendation = "request_changes"
	case maxRank >= SeverityRank("medium"):
		merged.RiskLevel = "medium"
		merged.OverallRecommendation = "comment"
	default:
		merged.RiskLevel = "low"
		merged.OverallRecommendation = "comment"
	}
}

// assembleResult builds the engine Result from the merged response, the final
// findings, and the per-pass reports.
func assembleResult(merged *llm.ReviewResponse, findings []ValidatedFinding, reports []PassReport) *Result {
	return &Result{
		Summary:        merged.Summary,
		RiskLevel:      merged.RiskLevel,
		Recommendation: merged.OverallRecommendation,
		Findings:       findings,
		MissingTests:   merged.MissingTests,
		Questions:      merged.Questions,
		Raw:            merged.Raw,
		CostUSD:        merged.CostUSD,
		PassReports:    reports,
	}
}
