package review

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/sxwebdev/ai-reviewer/internal/llm"
)

// skepticBatchSize bounds how many findings go into one skeptic LLM call.
const skepticBatchSize = 10

const skepticSystemPrompt = `You are a skeptical staff engineer auditing another reviewer's draft findings
before they are posted to a GitLab merge request. You are running inside a full
read-only checkout of the repository at the MR head commit.

For each numbered finding, actively try to REFUTE it by reading the actual
repository code (Read, Grep, Glob, git). Rules:
- "refuted" requires concrete quoted-code evidence in "reason" showing the
  finding is wrong. Do not refute on style disagreement or plausibility.
- "confirmed" means you checked the code and the issue is real.
- If you cannot conclusively confirm or refute, return "uncertain".
- If a finding duplicates another finding in the list, set "duplicate_of" to
  the other finding's index (keep the better one un-flagged).
- Never invent new findings; judge only the listed ones, by index.
You MUST return a single strict JSON object matching the provided schema.`

// skepticStage runs the skeptic verification over the first VerifyMaxFindings
// findings (they are already severity-ranked by the validator) in batches.
// Batches are independent (verdict indices are batch-local), so they run
// concurrently under the pipeline's MaxParallel bound. Findings beyond the cap
// — or any batch whose LLM call fails — are kept and marked unverified. The
// skeptic can only drop or demote, never add. Returns the surviving findings
// and the stage's total LLM cost.
func (e *Engine) skepticStage(ctx context.Context, in ReviewInput, pc PipelineConfig, findings []ValidatedFinding) ([]ValidatedFinding, float64) {
	limit := min(pc.VerifyMaxFindings, len(findings))
	verified := findings[:limit]
	rest := findings[limit:]
	for i := range rest {
		rest[i].Verification = VerificationUnverified
	}

	nBatches := (len(verified) + skepticBatchSize - 1) / skepticBatchSize
	results := make([][]ValidatedFinding, nBatches)
	costs := make([]float64, nBatches)
	sem := make(chan struct{}, max(pc.MaxParallel, 1))
	var wg sync.WaitGroup
	for b := range nBatches {
		wg.Add(1)
		go func(b int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			start := b * skepticBatchSize
			end := min(start+skepticBatchSize, len(verified))
			batch := verified[start:end] // disjoint sub-slices — no cross-goroutine writes
			verdicts, cost, err := e.skepticVerify(ctx, in, batch)
			costs[b] = cost
			if err != nil {
				e.log.Warn("skeptic pass failed; keeping findings unverified", "err", err)
				for i := range batch {
					batch[i].Verification = VerificationUnverified
				}
				results[b] = batch
				return
			}
			results[b] = applyVerdicts(batch, verdicts, e.log)
		}(b)
	}
	wg.Wait()

	var out []ValidatedFinding
	var cost float64
	for b := range results {
		out = append(out, results[b]...)
		cost += costs[b]
	}
	return append(out, rest...), cost
}

// skepticVerify runs one skeptic LLM call over a batch of findings and returns
// the verdicts (indices are 1-based within the batch) plus the call's cost.
func (e *Engine) skepticVerify(ctx context.Context, in ReviewInput, batch []ValidatedFinding) ([]llm.FindingVerdict, float64, error) {
	var resp llm.VerdictResponse
	cost, err := e.client.CompleteJSON(ctx, llm.Request{
		System:       skepticSystemPrompt,
		Prompt:       buildSkepticPrompt(in, batch),
		Model:        in.Model,
		WorkDir:      in.WorkDir,
		AgentMode:    true,
		AllowedTools: in.AllowedTools,
		JSONSchema:   llm.VerdictJSONSchema,
	}, &resp)
	if err != nil {
		return nil, cost, err
	}
	return resp.Verdicts, cost, nil
}

// buildSkepticPrompt renders the numbered findings with their diff excerpts.
func buildSkepticPrompt(in ReviewInput, batch []ValidatedFinding) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# MR under review: %s (%s → %s)\n\n", in.Title, in.SourceBranch, in.TargetBranch)
	b.WriteString("Audit the following draft review findings. Return a verdict for every index.\n")
	for i, f := range batch {
		fmt.Fprintf(&b, "\n## Finding %d\n", i+1)
		fmt.Fprintf(&b, "- severity: %s, category: %s, confidence: %.2f\n", f.Severity, f.Category, f.Confidence)
		fmt.Fprintf(&b, "- location: %s:%d (%s)\n", f.FilePath, f.Source.Line, f.Source.LineKind)
		fmt.Fprintf(&b, "- title: %s\n", f.Title)
		fmt.Fprintf(&b, "- body: %s\n", f.Body)
		if excerpt := annotatedHunkFor(in.Files, f); excerpt != "" {
			b.WriteString("- diff excerpt:\n```\n")
			b.WriteString(excerpt)
			b.WriteString("```\n")
		}
	}
	b.WriteString("\nReturn the strict JSON verdicts object now.")
	return b.String()
}

// annotatedHunkFor renders the annotated hunk containing the finding's target
// line (empty when the file/hunk cannot be located).
func annotatedHunkFor(files []*FileDiff, f ValidatedFinding) string {
	fd := FindFileDiff(files, f.FilePath)
	if fd == nil {
		return ""
	}
	for _, h := range fd.Hunks {
		for _, l := range h.Lines {
			if (f.Source.LineKind == "old" && l.OldLine == f.Source.Line && l.OldLine != 0) ||
				(f.Source.LineKind != "old" && l.NewLine == f.Source.Line && l.NewLine != 0) {
				return RenderAnnotatedDiff(&FileDiff{Hunks: []Hunk{h}})
			}
		}
	}
	// Fall back to the first hunk so the skeptic sees at least some diff.
	if len(fd.Hunks) > 0 {
		return RenderAnnotatedDiff(&FileDiff{Hunks: fd.Hunks[:1]})
	}
	return ""
}

// applyVerdicts applies skeptic verdicts to a batch (pure):
//   - refuted   → dropped; blocking/critical findings are demoted to uncertain
//     instead of dropped (a hallucinating skeptic must not silently
//     suppress a real blocker)
//   - uncertain → kept, flagged for human check, confidence capped at 0.5
//   - confirmed → kept, confidence raised to the verdict's confidence
//   - duplicate_of → dropped, but only when the target itself survives and the
//     finding is not blocking/critical (blockers are demoted, never dropped);
//     mutual duplicates (A→B, B→A) keep the smaller index
//   - no verdict for an index → kept, marked unverified
func applyVerdicts(batch []ValidatedFinding, verdicts []llm.FindingVerdict, log *slog.Logger) []ValidatedFinding {
	byIndex := map[int]llm.FindingVerdict{}
	for _, v := range verdicts {
		if v.Index >= 1 && v.Index <= len(batch) {
			byIndex[v.Index] = v
		}
	}

	// Phase 1: who survives the refute verdicts (ignoring duplicates), so a
	// duplicate_of pointing at a finding the skeptic itself removed is never
	// honored — otherwise both copies of a real bug vanish.
	survivesRefute := func(idx int) bool { // 1-based
		v, ok := byIndex[idx]
		if !ok {
			return true
		}
		if v.Verdict != "refuted" {
			return true
		}
		return SeverityRank(NormalizeSeverity(batch[idx-1].Severity)) >= SeverityRank("blocking")
	}
	// dupTarget returns the valid duplicate target of idx, or 0.
	dupTarget := func(idx int) int {
		v, ok := byIndex[idx]
		if !ok {
			return 0
		}
		if d := v.DuplicateOf; d >= 1 && d <= len(batch) && d != idx {
			return d
		}
		return 0
	}

	demote := func(f *ValidatedFinding, note string) {
		f.Verification = VerificationUncertain
		f.Confidence = min(f.Confidence, 0.5)
		f.Source.RequiresHumanCheck = true
		f.ValidationError = appendNote(f.ValidationError, note)
	}

	var out []ValidatedFinding
	for i, f := range batch {
		v, ok := byIndex[i+1]
		if !ok {
			f.Verification = VerificationUnverified
			out = append(out, f)
			continue
		}
		// Duplicate handling first, with guards: target must survive its own
		// refutation, mutual pairs keep the smaller index, and blockers are
		// demoted instead of dropped.
		if d := dupTarget(i + 1); d != 0 && survivesRefute(d) {
			mutual := dupTarget(d) == i+1
			if !mutual || i+1 > d {
				if SeverityRank(NormalizeSeverity(f.Severity)) >= SeverityRank("blocking") {
					demote(&f, fmt.Sprintf("skeptic marked as duplicate of finding %d", d))
					out = append(out, f)
					continue
				}
				log.Info("skeptic dropped duplicate finding", "title", f.Title, "duplicate_of", d)
				continue
			}
		}
		switch v.Verdict {
		case "refuted":
			if SeverityRank(NormalizeSeverity(f.Severity)) >= SeverityRank("blocking") {
				demote(&f, "skeptic disputed: "+v.Reason)
				out = append(out, f)
				continue
			}
			log.Info("skeptic refuted finding", "title", f.Title, "reason", v.Reason)
		case "uncertain":
			demote(&f, "unverified: "+v.Reason)
			out = append(out, f)
		case "confirmed":
			f.Verification = VerificationConfirmed
			f.Confidence = max(f.Confidence, clamp01(v.Confidence))
			out = append(out, f)
		default:
			f.Verification = VerificationUnverified
			out = append(out, f)
		}
	}
	return out
}

func appendNote(existing, note string) string {
	if existing == "" {
		return note
	}
	return existing + "; " + note
}
