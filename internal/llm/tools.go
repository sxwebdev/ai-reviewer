package llm

import (
	"path/filepath"
	"strings"
)

// WorktreePlaceholder is substituted in every allowed_tools rule with the
// worktree claude is invoked in, in the absolute form claude's permission
// matcher expects.
//
// It exists because the scope is only known per review — the worktree path
// carries the head SHA — while allowed_tools is static configuration. Writing
// `Read(${worktree}/**)` in config.yaml is what turns the worktree from a
// working directory into a boundary: verified against claude 2.1.222, a Read of
// a file outside the rule's path is refused with a permission_denials entry,
// while a Read inside it succeeds.
const WorktreePlaceholder = "${worktree}"

// ExpandToolRules resolves WorktreePlaceholder in each rule against workDir and
// returns the usable rules plus the ones that had to be dropped.
//
// A rule is dropped rather than passed through unexpanded when there is no
// usable worktree: `Read(${worktree}/**)` with an empty workDir would otherwise
// expand to `Read(//**)` — a grant over the whole filesystem, produced by the
// very code meant to constrain it. Dropping fails closed instead: claude's
// dontAsk mode denies any tool that no allow rule covers (verified against
// 2.1.222, including with no --allowedTools flag at all).
func ExpandToolRules(rules []string, workDir string) (expanded, dropped []string) {
	scope := toolPathScope(workDir)
	for _, r := range rules {
		if !strings.Contains(r, WorktreePlaceholder) {
			expanded = append(expanded, r)
			continue
		}
		if scope == "" {
			dropped = append(dropped, r)
			continue
		}
		expanded = append(expanded, strings.ReplaceAll(r, WorktreePlaceholder, scope))
	}
	return expanded, dropped
}

// toolPathScope renders workDir as a claude permission-rule path, or "" when it
// cannot be one. claude spells an absolute path with a doubled leading slash
// (`Read(//work/x/**)`); a relative path would be resolved against a working
// directory this process does not share with the subprocess, so it is refused.
func toolPathScope(workDir string) string {
	workDir = strings.TrimSpace(workDir)
	if workDir == "" || !filepath.IsAbs(workDir) {
		return ""
	}
	return "/" + filepath.ToSlash(filepath.Clean(workDir))
}
