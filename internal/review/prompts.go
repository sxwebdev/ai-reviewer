package review

import (
	"fmt"
	"strings"
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
line_kind is one of: "new" (an added line), "old" (a removed line), "context" (an unchanged line), or "file".`)
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

	if len(in.Memory) > 0 {
		b.WriteString("\n## Project rules and review memory (apply these)\n")
		for _, m := range in.Memory {
			fmt.Fprintf(&b, "- [%s] %s: %s\n", m.Type, m.Title, m.Body)
		}
	}

	b.WriteString("\n## Changed files (unified diffs)\n")
	for _, f := range in.Files {
		path := f.NewPath
		if path == "" {
			path = f.OldPath
		}
		fmt.Fprintf(&b, "\n### %s\n", path)
		if raw := in.RawDiffs[path]; raw != "" {
			b.WriteString("```diff\n")
			b.WriteString(raw)
			if !strings.HasSuffix(raw, "\n") {
				b.WriteByte('\n')
			}
			b.WriteString("```\n")
		}
	}

	b.WriteString("\nReview the diffs and return the strict JSON review object now.")
	return b.String()
}
