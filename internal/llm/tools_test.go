package llm

import (
	"slices"
	"testing"
)

func TestExpandToolRules(t *testing.T) {
	for _, tc := range []struct {
		name        string
		rules       []string
		workDir     string
		wantExpand  []string
		wantDropped []string
	}{
		{
			name:       "the shipped default is scoped to the worktree",
			rules:      []string{"Read(" + WorktreePlaceholder + "/**)", "Grep(" + WorktreePlaceholder + "/**)"},
			workDir:    "/work/worktrees/gitlab.example.com/group/repo/abc123",
			wantExpand: []string{"Read(//work/worktrees/gitlab.example.com/group/repo/abc123/**)", "Grep(//work/worktrees/gitlab.example.com/group/repo/abc123/**)"},
		},
		{
			// An operator who writes a bare tool name gets what they asked for;
			// the placeholder is opt-in, not a rewrite of every rule.
			name:       "rules without the placeholder pass through untouched",
			rules:      []string{"Read", "Bash(git log *)"},
			workDir:    "/work/wt",
			wantExpand: []string{"Read", "Bash(git log *)"},
		},
		{
			// The dangerous case: substituting "" would produce Read(//**) — a
			// grant over the entire filesystem, emitted by the code that exists to
			// prevent exactly that.
			name:        "no worktree drops the scoped rules rather than widening them",
			rules:       []string{"Read(" + WorktreePlaceholder + "/**)", "Read"},
			workDir:     "",
			wantExpand:  []string{"Read"},
			wantDropped: []string{"Read(" + WorktreePlaceholder + "/**)"},
		},
		{
			// The subprocess's cwd is not this process's cwd, so a relative path
			// would scope the rule to somewhere unrelated.
			name:        "a relative worktree is refused",
			rules:       []string{"Glob(" + WorktreePlaceholder + "/**)"},
			workDir:     "relative/dir",
			wantDropped: []string{"Glob(" + WorktreePlaceholder + "/**)"},
		},
		{
			name:       "a trailing separator does not double up",
			rules:      []string{"Read(" + WorktreePlaceholder + "/**)"},
			workDir:    "/work/wt/",
			wantExpand: []string{"Read(//work/wt/**)"},
		},
		{
			name:       "every occurrence in one rule is substituted",
			rules:      []string{"Read(" + WorktreePlaceholder + "/**," + WorktreePlaceholder + "/.config)"},
			workDir:    "/w",
			wantExpand: []string{"Read(//w/**,//w/.config)"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, dropped := ExpandToolRules(tc.rules, tc.workDir)
			if !slices.Equal(got, tc.wantExpand) {
				t.Errorf("rules = %q, want %q", got, tc.wantExpand)
			}
			if !slices.Equal(dropped, tc.wantDropped) {
				t.Errorf("dropped = %q, want %q", dropped, tc.wantDropped)
			}
		})
	}
}

// The placeholder must survive as an exact literal: it is spelled out in the
// `default:` tag on config's allowed_tools (a struct tag cannot reference this
// constant), and a rename here would silently stop scoping the rules — they
// would pass through unexpanded and claude would match nothing.
func TestWorktreePlaceholderSpelling(t *testing.T) {
	if WorktreePlaceholder != "${worktree}" {
		t.Errorf("WorktreePlaceholder = %q; config defaults and README document ${worktree}", WorktreePlaceholder)
	}
}
