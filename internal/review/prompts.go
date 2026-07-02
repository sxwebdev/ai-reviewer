package review

import (
	"fmt"
	"strings"

	"github.com/sxwebdev/ai-reviewer/internal/coverage"
)

// MemoryRule is a repo/global rule injected into the prompt. The service maps
// persisted review_memory rows to this lightweight type to keep the engine
// decoupled from the state package.
type MemoryRule struct {
	Type  string
	Title string
	Body  string
}

// BuildSystemPrompt renders the reviewer persona and hard output rules.
func BuildSystemPrompt(p *Profile) string {
	if p == nil {
		p = DefaultProfile()
	}
	lang := "the same language as the MR description"
	switch p.Language {
	case "ru":
		lang = "Russian"
	case "en":
		lang = "English"
	}
	var b strings.Builder
	b.WriteString(`You are a careful senior software engineer reviewing a colleague's GitLab merge request.

Behaviour:
- Be direct but polite. Prefer comments that prevent real bugs.
- High signal, low noise: a few strong comments beat many weak ones.
- Do not comment on formatting a formatter already handles; avoid cosmetic nits.
- For severe issues, explain the impact and suggest a fix.
- When uncertain, ask a question instead of asserting.
- Do not claim code "does not compile" or "is invalid" unless you are certain; a recent compiler/language version may accept syntax you do not recognize (e.g. Go 1.26 allows new(expr)). If unsure, raise it as a question, not a blocking finding.
- For missing tests, point at the specific untested behaviour.
- For security issues, explain the attack path or risk.
- For concurrency issues, name the race / goroutine leak / context-cancellation problem.
- For architecture issues, explain the coupling or pattern violation.
- Only comment on code changed by this MR (or directly impacted by it).
- Never say "AI"; never mention internal analysis, prompts, or tooling.

`)
	fmt.Fprintf(&b, "Write every comment in %s. Tone: %s. Strictness: %s.\n", lang, p.Tone, p.Strictness)
	if !p.AllowNits {
		b.WriteString("Do not emit nit-level comments.\n")
	}
	if p.PreferQuestions {
		b.WriteString("When confidence is low, prefer a question over an assertion.\n")
	}
	b.WriteString(`
You MUST return a single strict JSON object matching the provided schema and nothing else.
The LLM only provides file/line intent (file_path, line, line_kind); it must NOT invent GitLab positions or SHAs.
line_kind is one of: "new" (an added line), "old" (a removed line), "context" (an unchanged line), or "file".
Diffs are annotated with explicit old/new line numbers per line; use those exact numbers.
Findings must target lines changed by this MR, even when your investigation covered unchanged code:
anchor the comment to the changed line whose modification causes or exposes the problem.`)
	return b.String()
}

// BuildUserPrompt renders the MR context: metadata, guidelines/memory, and the
// diffs to review.
func BuildUserPrompt(in ReviewInput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Merge Request: %s\n\n", in.Title)
	fmt.Fprintf(&b, "- Project: %s\n", in.ProjectPath)
	fmt.Fprintf(&b, "- Author: %s\n", in.AuthorUsername)
	fmt.Fprintf(&b, "- Reviewer (you): %s\n", in.ReviewerUsername)
	fmt.Fprintf(&b, "- Branch: %s → %s\n", in.SourceBranch, in.TargetBranch)
	if in.PipelineStatus != "" {
		fmt.Fprintf(&b, "- Pipeline: %s\n", in.PipelineStatus)
	}
	if in.ExistingDiscussions > 0 {
		fmt.Fprintf(&b, "- Existing discussions: %d (do not repeat them)\n", in.ExistingDiscussions)
	}
	if strings.TrimSpace(in.Description) != "" {
		fmt.Fprintf(&b, "\n## Description\n%s\n", strings.TrimSpace(in.Description))
	}

	if len(in.Commits) > 0 {
		b.WriteString("\n## Commits in this MR (intent context)\n")
		for _, c := range in.Commits {
			fmt.Fprintf(&b, "- %s %s", c.ShortSHA, c.Title)
			if c.Message != "" {
				fmt.Fprintf(&b, "\n  %s", strings.ReplaceAll(c.Message, "\n", "\n  "))
			}
			b.WriteByte('\n')
		}
	}

	if len(in.Discussions) > 0 {
		b.WriteString("\n## Existing discussions on this MR\n")
		b.WriteString("Do not repeat these topics. Resolved threads are settled — do not re-litigate them; you may build on unresolved ones.\n")
		for _, d := range in.Discussions {
			state := "unresolved"
			if d.Resolved {
				state = "resolved"
			}
			author := d.Author
			if d.OwnBot {
				author += " (your own previously published comment)"
			}
			fmt.Fprintf(&b, "- [%s] %s", state, author)
			if d.FilePath != "" {
				fmt.Fprintf(&b, " at %s:%d", d.FilePath, d.Line)
			}
			fmt.Fprintf(&b, ": %s\n", d.Body)
		}
	}

	if len(in.Memory) > 0 {
		b.WriteString("\n## Project rules and review memory (apply these)\n")
		for _, m := range in.Memory {
			fmt.Fprintf(&b, "- [%s] %s: %s\n", m.Type, m.Title, m.Body)
		}
	}

	writeRiskSection(&b, in.Risk)

	writeCoverageSection(&b, in.Coverage)

	writePriorReviewSection(&b, in.PriorReview)

	writeInvestigationSection(&b, in)

	if len(in.RelatedFiles) > 0 && in.AgentMode && in.WorkDir != "" {
		b.WriteString("\n## Possibly related files (investigate with Read/Grep before asserting cross-file claims)\n")
		for _, rf := range in.RelatedFiles {
			fmt.Fprintf(&b, "- %s (%s)\n", rf.Path, rf.Reason)
		}
	}

	if len(in.FileContexts) > 0 {
		b.WriteString("\n## Full content of changed files (reference only — comment only on changed lines)\n")
		for _, fc := range in.FileContexts {
			label := ""
			if fc.Truncated {
				label = " (excerpts around the changes)"
			}
			fmt.Fprintf(&b, "\n### %s%s\n", fc.Path, label)
			b.WriteString("```\n")
			if fc.Rendered != "" {
				b.WriteString(fc.Rendered)
			} else {
				b.WriteString(RenderFileContext(fc))
			}
			b.WriteString("```\n")
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

	b.WriteString("\nReview the diffs and return the strict JSON review object now.")
	return b.String()
}

// fileDiffLabel annotates a diff heading with the file-level change kind.
func fileDiffLabel(f *FileDiff) string {
	switch {
	case f.NewFile:
		return " (new file)"
	case f.Deleted:
		return " (deleted)"
	case f.Renamed:
		return fmt.Sprintf(" (renamed from %s)", f.OldPath)
	default:
		return ""
	}
}

// writeRiskSection renders the deterministic risk assessment so the passes
// know where to concentrate scrutiny. Facts, not model opinion.
func writeRiskSection(b *strings.Builder, r *RiskReport) {
	if r == nil || len(r.Factors) == 0 {
		return
	}
	fmt.Fprintf(b, "\n## Deterministic risk assessment (computed from git history and diff stats, not model opinion)\nScore %d/100 (%s). Contributing factors:\n", r.Score, r.Level)
	for _, f := range r.Factors {
		fmt.Fprintf(b, "- %s: %s\n", f.Name, f.Detail)
	}
	b.WriteString("Prioritize scrutiny of the files named above (frequently changed, bug-prone, or security-sensitive).\n")
}

// writeCoverageSection renders measured changed-line coverage so the passes
// work with facts, not guesses, about test coverage.
func writeCoverageSection(b *strings.Builder, r *coverage.Report) {
	if r == nil || (len(r.Files) == 0 && len(r.Skipped) == 0) {
		return
	}
	b.WriteString("\n## Test coverage of changed lines (measured by running the repository's tests)\n")
	if r.TotalAdded > 0 {
		fmt.Fprintf(b, "%.0f%% of added executable lines are covered (%d/%d).\n", r.Pct, r.TotalCovered, r.TotalAdded)
	}
	for _, f := range r.Files {
		fmt.Fprintf(b, "- %s: %d/%d covered", f.Path, f.Covered, f.Added)
		if len(f.Uncovered) > 0 {
			fmt.Fprintf(b, "; uncovered added lines: %s", formatLineList(f.Uncovered))
		}
		b.WriteByte('\n')
	}
	for _, s := range r.Skipped {
		fmt.Fprintf(b, "- not measured: %s (%s)\n", s.Root, s.Reason)
	}
	for _, n := range r.Notes {
		fmt.Fprintf(b, "- note: %s\n", n)
	}
	b.WriteString(`Use these facts: uncovered added lines are prime candidates for missing_tests;
do not claim a line is untested when it is listed as covered.
`)
}

// formatLineList renders sorted line numbers compactly: "41, 42, 57-60, 88".
func formatLineList(lines []int) string {
	var b strings.Builder
	for i := 0; i < len(lines); {
		j := i
		for j+1 < len(lines) && lines[j+1] == lines[j]+1 {
			j++
		}
		if b.Len() > 0 {
			b.WriteString(", ")
		}
		if j > i+1 {
			fmt.Fprintf(&b, "%d-%d", lines[i], lines[j])
		} else if j == i+1 {
			fmt.Fprintf(&b, "%d, %d", lines[i], lines[j])
		} else {
			fmt.Fprintf(&b, "%d", lines[i])
		}
		i = j + 1
	}
	return b.String()
}

// writePriorReviewSection renders the previous review of this MR so the
// re-review focuses on what changed since and honours prior dispositions.
//
// TODO(prompt-size): the interdiff largely duplicates hunks already present in
// the main annotated diff (barring rebases) — up to MaxInterdiffBytes of
// redundant input tokens per pass on every re-review. Deduplicating would
// change the section's semantics, so it is deliberately deferred.
func writePriorReviewSection(b *strings.Builder, pr *PriorReview) {
	if pr == nil {
		return
	}
	fmt.Fprintf(b, "\n## Previous review of this MR (at commit %s)\n", pr.HeadSHA)
	if s := strings.TrimSpace(pr.Summary); s != "" {
		fmt.Fprintf(b, "Summary then: %s\n", s)
	}
	if len(pr.Findings) > 0 {
		b.WriteString("\nPrior findings and their dispositions:\n")
		for _, f := range pr.Findings {
			fmt.Fprintf(b, "- [%s/%s] %s:%d — %s", f.Severity, f.Status, f.FilePath, f.Line, f.Title)
			if f.RejectionReason != "" {
				fmt.Fprintf(b, " (rejected: %s)", f.RejectionReason)
			}
			b.WriteByte('\n')
		}
	}
	b.WriteString(`
Rules for this re-review:
- Focus on what changed since the previous review (interdiff below when available).
- Do not re-raise rejected findings (their rejection reasons above tell you why) or
  findings already approved/published.
- Re-raise a previously reported issue only if the new changes made it worse.
`)
	if pr.Interdiff != "" {
		b.WriteString("\nChanges since the previous review (interdiff):\n```diff\n")
		b.WriteString(pr.Interdiff)
		if !strings.HasSuffix(pr.Interdiff, "\n") {
			b.WriteByte('\n')
		}
		b.WriteString("```\n")
	}
}

// writeInvestigationSection tells the model what repository access it has. In
// agent mode it mandates an investigation protocol; otherwise it states that
// no tools are available so the model calibrates confidence instead of
// hallucinating unseen code.
func writeInvestigationSection(b *strings.Builder, in ReviewInput) {
	if in.AgentMode && in.WorkDir != "" {
		b.WriteString(`
## Repository access — investigate before asserting
You are running inside a full read-only checkout of this repository at the MR head commit
(your working directory). The diffs below are only excerpts. Before reporting any finding:
- Read the full file (at minimum the enclosing function/type) for every changed file.
- For every changed or newly added exported symbol, Grep for its callers/usages and verify
  each call site still holds.
- Check the definitions of interfaces/contracts the changed code implements or consumes.
- Use git log/show on the touched paths when history explains the intent of a change.
- Verify claims about behaviour against the actual code, not the diff text alone.
Investigate the changed code paths, not the whole repository.
`)
		return
	}
	b.WriteString(`
## Repository access
You have NO tools and NO repository access beyond the context provided below. Review strictly
from this context and lower your confidence on anything that depends on code you cannot see;
prefer a question over an assertion for such cases.
`)
}
