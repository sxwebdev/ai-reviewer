package review

import (
	"context"
	"fmt"
	"strings"

	"github.com/sxwebdev/ai-reviewer/internal/llm"
)

// CompletenessReport is the acceptance-criteria audit of an MR: what the
// author said the change does vs. what the diff actually contains. It is a
// report, never findings — it does not anchor to changed lines.
type CompletenessReport struct {
	Criteria []llm.CompletenessCriterion `json:"criteria"`
	Note     string                      `json:"note,omitempty"`
}

const completenessSystemPrompt = `You are auditing a GitLab merge request for TASK COMPLETENESS, not code quality.

From the MR title, description, and commit messages, extract the concrete
acceptance criteria the author stated or clearly implied — the specific
behaviours, fixes, or artifacts this MR claims to deliver. Then, for each
criterion, verify against the diff (and the repository, when tools are
available) whether it is actually delivered:
- "done"    — the diff clearly implements it
- "partial" — started but visibly incomplete (e.g. handled in one code path of two)
- "missing" — claimed but not present in the diff
- "unclear" — cannot be judged from the available context
Cite concrete evidence (files, functions, quoted fragments) for every verdict.

Rules:
- Do NOT invent criteria beyond what the author stated; an empty criteria
  array is the correct answer for an MR whose description carries no
  actionable claims.
- Do not comment on code quality, style, or bugs — other reviewers do that.
- Never mention AI, prompts, or internal tooling.
You MUST return a single strict JSON object matching the provided schema.`

// hasIntentText reports whether the MR carries enough stated intent
// (description or commit messages beyond bare titles) to audit against. A
// bare title yields nothing useful, so the call is skipped.
func hasIntentText(in ReviewInput) bool {
	if len(strings.TrimSpace(in.Description)) >= 20 {
		return true
	}
	for _, c := range in.Commits {
		if strings.TrimSpace(c.Message) != "" {
			return true
		}
	}
	return len(in.Commits) > 1 // several commit titles still sketch a task list
}

// checkCompleteness runs the completeness audit as one LLM call, returning
// the report and the call's cost. Agent-mode tools are passed through so
// criteria can be verified against the repo.
func (e *Engine) checkCompleteness(ctx context.Context, in ReviewInput) (*CompletenessReport, float64, error) {
	var resp llm.CompletenessResponse
	cost, err := e.client.CompleteJSON(ctx, llm.Request{
		System:       completenessSystemPrompt,
		Prompt:       buildCompletenessPrompt(in),
		Model:        in.Model,
		WorkDir:      in.WorkDir,
		AgentMode:    in.AgentMode,
		AllowedTools: in.AllowedTools,
		JSONSchema:   llm.CompletenessJSONSchema,
	}, &resp)
	if err != nil {
		return nil, cost, err
	}
	return &CompletenessReport{Criteria: resp.Criteria, Note: resp.Note}, cost, nil
}

// buildCompletenessPrompt renders the stated intent plus the annotated diffs.
// Deliberately excludes discussions/memory/risk — the audit is intent vs diff.
func buildCompletenessPrompt(in ReviewInput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Merge Request: %s\n", in.Title)
	fmt.Fprintf(&b, "- Branch: %s → %s\n", in.SourceBranch, in.TargetBranch)
	if strings.TrimSpace(in.Description) != "" {
		fmt.Fprintf(&b, "\n## Description (the author's stated intent)\n%s\n", strings.TrimSpace(in.Description))
	}
	if len(in.Commits) > 0 {
		b.WriteString("\n## Commit messages\n")
		for _, c := range in.Commits {
			fmt.Fprintf(&b, "- %s %s", c.ShortSHA, c.Title)
			if c.Message != "" {
				fmt.Fprintf(&b, "\n  %s", strings.ReplaceAll(c.Message, "\n", "\n  "))
			}
			b.WriteByte('\n')
		}
	}
	b.WriteString("\n## Changed files (diffs annotated with old/new line numbers)\n")
	for _, f := range in.Files {
		path := f.Path()
		fmt.Fprintf(&b, "\n### %s%s\n", path, fileDiffLabel(f))
		if rendered := RenderAnnotatedDiff(f); rendered != "" {
			b.WriteString("```\n")
			b.WriteString(rendered)
			b.WriteString("```\n")
		}
	}
	b.WriteString("\nExtract the acceptance criteria and return the strict JSON completeness object now.")
	return b.String()
}
