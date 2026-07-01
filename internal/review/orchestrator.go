package review

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/sxwebdev/ai-reviewer/internal/gitlab"
	"github.com/sxwebdev/ai-reviewer/internal/llm"
)

// ReviewInput is everything the engine needs to review one MR at a head sha.
// The service layer assembles it from GitLab + state; the engine performs no
// I/O of its own beyond the LLM call.
type ReviewInput struct {
	ProjectPath      string
	ProjectID        int64
	MRIID            int64
	Title            string
	Description      string
	AuthorUsername   string
	ReviewerUsername string
	SourceBranch     string
	TargetBranch     string

	Files    []*FileDiff       // parsed diffs
	RawDiffs map[string]string // path -> raw unified diff (for the prompt)
	Refs     gitlab.DiffRefs

	Memory  []MemoryRule
	Profile *Profile

	ExistingFingerprints map[string]bool
	PipelineStatus       string
	ExistingDiscussions  int

	// Agent mode.
	WorkDir      string
	AgentMode    bool
	AllowedTools []string
	Model        string

	// SelfReflect enables the second pruning pass.
	SelfReflect bool
}

// Result is the engine's output: a summary plus validated findings.
type Result struct {
	Summary        string
	RiskLevel      string
	Recommendation string
	Findings       []ValidatedFinding
	MissingTests   []llm.MissingTest
	Questions      []llm.Question
	Raw            string
	CostUSD        float64
}

// Engine runs the LLM review pipeline.
type Engine struct {
	client llm.Client
	log    *slog.Logger
}

// NewEngine builds an Engine.
func NewEngine(client llm.Client, log *slog.Logger) *Engine {
	return &Engine{client: client, log: log}
}

// Review runs the pipeline: build context/prompts → LLM review → optional
// self-reflection → deterministic validation/mapping/dedupe → ranked result.
func (e *Engine) Review(ctx context.Context, in ReviewInput) (*Result, error) {
	if in.Profile == nil {
		in.Profile = DefaultProfile()
	}

	resp, err := e.client.Review(ctx, llm.Request{
		System:       BuildSystemPrompt(in.Profile),
		Prompt:       BuildUserPrompt(in),
		Model:        in.Model,
		WorkDir:      in.WorkDir,
		AgentMode:    in.AgentMode,
		AllowedTools: in.AllowedTools,
		JSONSchema:   llm.ReviewJSONSchema,
	})
	if err != nil {
		return nil, fmt.Errorf("llm review: %w", err)
	}

	if in.SelfReflect {
		if reflected, err := e.selfReflect(ctx, in, resp); err != nil {
			e.log.Warn("self-reflection failed; keeping original findings", "err", err)
		} else {
			resp = reflected
		}
	}

	v := NewValidator(ValidatorConfig{
		SeverityThreshold: in.Profile.SeverityThreshold,
		MaxComments:       in.Profile.MaxComments,
	})
	findings := v.Validate(resp, in.Files, in.Refs, in.ProjectID, in.MRIID, in.ExistingFingerprints)

	// Deterministically refute "does not compile" claims: if a worktree is
	// available, drop any finding asserting a build failure whose package
	// actually compiles (guards against the model missing recent Go features).
	if in.WorkDir != "" {
		findings = e.verifyBuildClaims(ctx, in.WorkDir, findings)
	}

	e.log.Info("review complete",
		"raw_findings", len(resp.Findings), "validated", len(findings),
		"risk", resp.RiskLevel, "cost_usd", resp.CostUSD)

	return &Result{
		Summary:        resp.Summary,
		RiskLevel:      resp.RiskLevel,
		Recommendation: resp.OverallRecommendation,
		Findings:       findings,
		MissingTests:   resp.MissingTests,
		Questions:      resp.Questions,
		Raw:            resp.Raw,
		CostUSD:        resp.CostUSD,
	}, nil
}

// selfReflect asks the model to prune false positives, duplicates, and weak or
// unanchored findings, returning a revised ReviewResponse.
func (e *Engine) selfReflect(ctx context.Context, in ReviewInput, resp *llm.ReviewResponse) (*llm.ReviewResponse, error) {
	prompt := fmt.Sprintf(`Here is your draft review as JSON:

%s

Revise it: remove false positives, duplicates, weak or low-confidence findings,
and anything not tied to the changed lines. Prefer fewer, higher-signal comments.
Convert uncertain assertions into questions. Do not repeat existing discussions.
Return the SAME strict JSON schema with the revised findings and re-scored confidence.`, resp.Raw)

	var revised llm.ReviewResponse
	err := e.client.CompleteJSON(ctx, llm.Request{
		System:     BuildSystemPrompt(in.Profile),
		Prompt:     prompt,
		Model:      in.Model,
		JSONSchema: llm.ReviewJSONSchema,
	}, &revised)
	if err != nil {
		return nil, err
	}
	revised.CostUSD = resp.CostUSD + revised.CostUSD
	return &revised, nil
}
