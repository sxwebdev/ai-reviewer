package review

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/sxwebdev/ai-reviewer/internal/coverage"
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

	Files []*FileDiff // parsed diffs
	Refs  gitlab.DiffRefs

	// FileContexts is budgeted full/windowed content of changed files so the
	// model sees the code surrounding the hunks.
	FileContexts []FileContext

	// Commits are the MR's commit messages (intent context).
	Commits []CommitInfo

	// Discussions is existing discussion content so the model does not repeat
	// settled topics (complements the ExistingDiscussions count).
	Discussions []DiscussionNote

	// PriorReview is the previous review of this MR (nil on first review) so a
	// re-review focuses on the interdiff and honours prior dispositions.
	PriorReview *PriorReview

	// RelatedFiles are FTS-suggested investigation leads (agent mode).
	RelatedFiles []RelatedFile

	// Risk is the deterministic risk assessment (nil when disabled); it points
	// the passes at hot spots and is persisted alongside the review.
	Risk *RiskReport

	// Coverage is the measured changed-line test coverage (nil when the
	// opt-in measurement did not run); facts for the passes and the skeptic.
	Coverage *coverage.Report

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

	// Pipeline configures the multi-pass pipeline; the zero value is the
	// cheap single-pass mode with verification off.
	Pipeline PipelineConfig
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
	PassReports    []PassReport
	Completeness   *CompletenessReport
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

// Review runs the multi-stage pipeline:
//
//  1. FAN-OUT: N specialist LLM passes with bounded concurrency
//  2. MERGE: concatenate + cross-pass dedupe + provenance (pure Go)
//  3. VALIDATE: deterministic validation/mapping/dedupe at a relaxed cap
//  4. VERIFY-LLM: skeptic refute/confirm pass (or reflect fallback)
//  5. VERIFY-DET: deterministic checks against the worktree
//  6. FINALIZE: rank and cap at MaxComments
//
// The zero-value Pipeline reproduces the original single-pass behaviour.
func (e *Engine) Review(ctx context.Context, in ReviewInput) (*Result, error) {
	if in.Profile == nil {
		in.Profile = DefaultProfile()
	}
	// The engine boundary owns the MaxComments floor (a zero-value field must
	// mean "default", never "drop everything"); a local keeps the caller's
	// shared Profile unmutated.
	maxComments := in.Profile.MaxComments
	if maxComments <= 0 {
		maxComments = DefaultProfile().MaxComments
	}
	pc := in.Pipeline.withDefaults()
	specs := ResolvePasses(pc.Passes, e.log)

	// Completeness audit runs concurrently with the review passes; failure is
	// never fatal and does not count against MaxParallel (one extra call, like
	// the skeptic).
	var completeness *CompletenessReport
	var complErr error
	var complCost float64
	var complDur time.Duration
	var complWG sync.WaitGroup
	complCtx, complCancel := context.WithCancel(ctx)
	// An explicit "on" always runs; "auto" additionally requires enough stated
	// intent to audit against (which depends on the commits section being
	// enabled — auto quietly skipping there is fine, an explicit "on" is not).
	complRan := pc.Completeness == CompletenessOn ||
		(pc.Completeness == CompletenessAuto && hasIntentText(in))
	if complRan {
		complWG.Go(func() {
			start := time.Now()
			r, cost, err := e.checkCompleteness(complCtx, in)
			complCost, complDur, complErr = cost, time.Since(start), err
			if err != nil {
				e.log.Warn("completeness audit failed", "err", err)
			} else {
				completeness = r
			}
		})
	}
	// The audit's claude subprocess must never outlive Review: on ANY exit
	// (including the all-passes-failed early return) cancel it and wait, so
	// the caller can safely remove the worktree it is reading. Defers run
	// LIFO: Wait is registered first so cancel fires before we block on it.
	defer complWG.Wait()
	defer complCancel()

	// Stage 1: fan-out.
	outcomes, reports := e.runPasses(ctx, in, specs, pc.MaxParallel)
	if err := allPassesFailed(outcomes); err != nil {
		return nil, fmt.Errorf("all %d review passes failed: %w", len(specs), err)
	}

	// Stage 2: merge (pure).
	merged := mergeResponses(outcomes)

	if pc.VerifyMode == VerifyReflect {
		start := time.Now()
		reflected, reflectCost, err := e.selfReflect(ctx, in, merged)
		if err != nil {
			e.log.Warn("self-reflection failed; keeping original findings", "err", err)
		} else {
			merged = applyReflect(merged, reflected, e.log)
		}
		// Add the reflect call's cost AFTER applyReflect: the reflected
		// response carries the pre-reflect total, so adding earlier would lose
		// the reflect spend on the success path.
		merged.CostUSD += reflectCost
		rep := PassReport{Name: "reflect", CostUSD: reflectCost, DurationMS: time.Since(start).Milliseconds()}
		if err != nil {
			rep.Err = err.Error()
		}
		reports = append(reports, rep)
	}

	// Stage 3: validate at a relaxed cap so later verification stages choose
	// the final MaxComments from verified survivors, not a pre-starved list.
	v := NewValidator(ValidatorConfig{
		SeverityThreshold: in.Profile.SeverityThreshold,
		MaxComments:       maxComments * candidateMultiplier,
	})
	findings := v.Validate(merged, in.Files, in.Refs, in.ProjectID, in.MRIID, in.ExistingFingerprints)

	// Stage 4: skeptic verification (agent mode only).
	if pc.VerifyMode == VerifySkeptic && in.WorkDir != "" && len(findings) > 0 {
		start := time.Now()
		var skepticCost float64
		findings, skepticCost = e.skepticStage(ctx, in, pc, findings)
		merged.CostUSD += skepticCost
		reports = append(reports, PassReport{
			Name: "skeptic", CostUSD: skepticCost, DurationMS: time.Since(start).Milliseconds(),
		})
	}

	// Stage 5: deterministic verification against the worktree (e.g. go build
	// refuting "does not compile" claims, go vet corroboration).
	if in.WorkDir != "" {
		findings = runVerifiers(ctx, in.WorkDir, BuiltinVerifiers(pc.Verifiers, e.log), findings, e.log)
	}

	// Stage 6: finalize.
	rankFindings(findings)
	if len(findings) > maxComments {
		findings = findings[:maxComments]
	}

	complWG.Wait()
	if complRan {
		merged.CostUSD += complCost
		rep := PassReport{Name: "completeness", CostUSD: complCost, DurationMS: complDur.Milliseconds()}
		if complErr != nil {
			rep.Err = complErr.Error()
		}
		reports = append(reports, rep)
	}

	e.log.Info("review complete",
		"passes", len(specs), "raw_findings", len(merged.Findings), "validated", len(findings),
		"risk", merged.RiskLevel, "cost_usd", merged.CostUSD)

	res := assembleResult(merged, findings, reports)
	res.Completeness = completeness
	return res, nil
}

// applyReflect merges a self-reflection result back into the review response
// while preserving the invariants reflect's raw JSON round-trip loses (pure):
//   - Raw: the reflected response is re-marshaled so raw_report_json never
//     persists empty (CompleteJSON does not populate Raw);
//   - PassName provenance: restored by file+title matching against the
//     pre-reflect findings (json:"-" fields do not survive the round-trip);
//   - blocking protection: blocking/critical findings the reflection removed
//     are restored demoted (confidence ≤ 0.5, requires human check) — the
//     model must not silently suppress a blocker, mirroring the skeptic rule;
//   - drop-only enforcement: verification may only remove or demote, never
//     add — post findings whose file+title key was not in the pre-reflect set
//     (hallucinated additions, reworded titles) are dropped in Go, not trusted
//     to the prompt instruction.
func applyReflect(pre, post *llm.ReviewResponse, log *slog.Logger) *llm.ReviewResponse {
	key := func(f llm.Finding) string {
		return strings.ToLower(strings.TrimSpace(f.FilePath)) + "\x00" + normalizeTitle(f.Title)
	}
	preKeys := map[string]bool{}
	prePass := map[string]string{}
	for _, f := range pre.Findings {
		k := key(f)
		preKeys[k] = true
		if _, ok := prePass[k]; !ok {
			prePass[k] = f.PassName
		}
	}

	postKeys := map[string]bool{}
	kept := post.Findings[:0:0]
	for _, f := range post.Findings {
		k := key(f)
		if !preKeys[k] {
			log.Warn("self-reflection added a new finding; dropping (verification may only remove or demote)",
				"file", f.FilePath, "title", f.Title)
			continue
		}
		if f.PassName == "" {
			f.PassName = prePass[k]
		}
		postKeys[k] = true
		kept = append(kept, f)
	}
	post.Findings = kept

	for _, f := range pre.Findings {
		if SeverityRank(NormalizeSeverity(f.Severity)) < SeverityRank("blocking") || postKeys[key(f)] {
			continue
		}
		log.Warn("self-reflection removed a blocking finding; restoring demoted for human check",
			"file", f.FilePath, "title", f.Title)
		f.Confidence = min(f.Confidence, 0.5)
		f.RequiresHumanCheck = true
		post.Findings = append(post.Findings, f)
	}

	if raw, err := json.Marshal(post); err == nil {
		post.Raw = string(raw)
	}
	return post
}

// allPassesFailed returns the first pass error when no pass succeeded, nil
// otherwise.
func allPassesFailed(outcomes []passOutcome) error {
	var firstErr error
	for _, o := range outcomes {
		if o.err == nil && o.resp != nil {
			return nil
		}
		if firstErr == nil && o.err != nil {
			firstErr = o.err
		}
	}
	if firstErr == nil {
		firstErr = fmt.Errorf("no pass produced a response")
	}
	return firstErr
}

// selfReflect asks the model to prune false positives, duplicates, and weak or
// unanchored findings, returning a revised ReviewResponse and the call's cost
// (returned even on failure — the tokens were spent either way).
func (e *Engine) selfReflect(ctx context.Context, in ReviewInput, resp *llm.ReviewResponse) (*llm.ReviewResponse, float64, error) {
	prompt := fmt.Sprintf(`Here is your draft review as JSON:

%s

Revise it: remove false positives, duplicates, weak or low-confidence findings,
and anything not tied to the changed lines. Prefer fewer, higher-signal comments.
Convert uncertain assertions into questions. Do not repeat existing discussions.
Never ADD new findings — you may only remove findings or downgrade their severity
and confidence. Return the SAME strict JSON schema with the revised findings.`, resp.Raw)

	var revised llm.ReviewResponse
	cost, err := e.client.CompleteJSON(ctx, llm.Request{
		System:     BuildSystemPrompt(in.Profile),
		Prompt:     prompt,
		Model:      in.Model,
		JSONSchema: llm.ReviewJSONSchema,
	}, &revised)
	if err != nil {
		return nil, cost, err
	}
	revised.CostUSD = resp.CostUSD
	return &revised, cost, nil
}
