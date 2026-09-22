package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/tkcrm/mx/logger"

	"github.com/sxwebdev/ai-reviewer/internal/git"
	"github.com/sxwebdev/ai-reviewer/internal/gitlab"
	"github.com/sxwebdev/ai-reviewer/internal/review"
)

const mainGoContent = "package main\n\nfunc added() {}\n// tail\n"

func sampleFiles(t *testing.T) []*review.FileDiff {
	t.Helper()
	files := parseDiffs([]gitlab.MergeRequestDiff{
		{OldPath: "main.go", NewPath: "main.go", Diff: sampleDiff},
	}, nil, logger.ForTests(t))
	if len(files) != 1 {
		t.Fatalf("fixture diff did not parse: %d files", len(files))
	}
	return files
}

func TestBuildFileContexts(t *testing.T) {
	t.Parallel()

	t.Run("reads the file from GitLab when there is no worktree", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, withConfig(func(c *Config) {
			c.Context = review.ContextBudget{
				IncludeFullFiles: true, MaxFileLines: 500, HunkWindowLines: 60, MaxTotalBytes: 64 << 10,
			}
		}))
		h.fake.RawFiles["main.go@"+testHeadSHA] = []byte(mainGoContent)

		got := h.svc.buildFileContexts(t.Context(), "7", testHeadSHA, sampleFiles(t), "")
		if len(got) != 1 {
			t.Fatalf("file contexts = %d, want 1", len(got))
		}
		if got[0].Path != "main.go" {
			t.Errorf("path = %q, want main.go", got[0].Path)
		}
		if !strings.Contains(got[0].Rendered, "func added()") {
			t.Errorf("the rendered section is not cached or is empty:\n%s", got[0].Rendered)
		}
	})

	t.Run("prefers the worktree", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, withConfig(func(c *Config) {
			c.Context = review.ContextBudget{
				IncludeFullFiles: true, MaxFileLines: 500, HunkWindowLines: 60, MaxTotalBytes: 64 << 10,
			}
		}))
		dir := t.TempDir()
		writeFile(t, dir+"/main.go", "package main\n\nfunc added() {}\n// from the worktree\n")

		got := h.svc.buildFileContexts(t.Context(), "7", testHeadSHA, sampleFiles(t), dir)
		if len(got) != 1 || !strings.Contains(got[0].Rendered, "from the worktree") {
			t.Errorf("the worktree copy must win over the API: %+v", got)
		}
	})

	t.Run("skips a file that blows the budget", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, withConfig(func(c *Config) {
			c.Context = review.ContextBudget{
				IncludeFullFiles: true, MaxFileLines: 500, HunkWindowLines: 60, MaxTotalBytes: 8,
			}
		}))
		h.fake.RawFiles["main.go@"+testHeadSHA] = []byte(mainGoContent)

		if got := h.svc.buildFileContexts(t.Context(), "7", testHeadSHA, sampleFiles(t), ""); len(got) != 0 {
			t.Errorf("file contexts = %d, want none within an 8-byte budget", len(got))
		}
	})

	t.Run("rejects binary and oversized content", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, withConfig(func(c *Config) {
			c.Context = review.ContextBudget{
				IncludeFullFiles: true, MaxFileLines: 500, HunkWindowLines: 60, MaxTotalBytes: 64 << 10,
			}
		}))
		h.fake.RawFiles["main.go@"+testHeadSHA] = []byte("\x00\x01\x02binary")

		if got := h.svc.buildFileContexts(t.Context(), "7", testHeadSHA, sampleFiles(t), ""); len(got) != 0 {
			t.Errorf("binary content must never reach a prompt: %+v", got)
		}
	})

	t.Run("disabled", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		if got := h.svc.buildFileContexts(t.Context(), "7", testHeadSHA, sampleFiles(t), ""); got != nil {
			t.Errorf("file contexts = %v, want none when include_full_files is off", got)
		}
	})

	// A symlink is an ordinary text blob in a git diff (mode 120000), so
	// parseDiffs keeps it: not binary, not generated, not vendored. Reading it
	// through the worktree would put the target's contents into a "## File:"
	// prompt section — an MR author choosing what the reviewer's host reads,
	// with no agent mode and no cooperation from the model required.
	t.Run("refuses a symlink pointing out of the worktree", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, withConfig(func(c *Config) {
			c.Context = review.ContextBudget{
				IncludeFullFiles: true, MaxFileLines: 500, HunkWindowLines: 60, MaxTotalBytes: 64 << 10,
			}
		}))
		outside := t.TempDir()
		writeFile(t, outside+"/host-secret.env", "GITLAB_TOKEN=glpat-verysecrettokenvalue\n")

		dir := t.TempDir()
		if err := os.Symlink(outside+"/host-secret.env", dir+"/main.go"); err != nil {
			t.Fatalf("symlink: %v", err)
		}

		if got := h.svc.buildFileContexts(t.Context(), "7", testHeadSHA, sampleFiles(t), dir); len(got) != 0 {
			t.Errorf("a symlink was followed out of the worktree: %+v", got)
		}
	})

	// The leaf can be a perfectly ordinary regular file and still be outside:
	// an Lstat on it alone sees nothing wrong.
	t.Run("refuses a file reached through a symlinked directory", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, withConfig(func(c *Config) {
			c.Context = review.ContextBudget{
				IncludeFullFiles: true, MaxFileLines: 500, HunkWindowLines: 60, MaxTotalBytes: 64 << 10,
			}
		}))
		outside := t.TempDir()
		if err := os.Mkdir(outside+"/pkg", 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		writeFile(t, outside+"/pkg/main.go", "package main // from outside the worktree\n")

		dir := t.TempDir()
		if err := os.Symlink(outside+"/pkg", dir+"/pkg"); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		files := parseDiffs([]gitlab.MergeRequestDiff{
			{OldPath: "pkg/main.go", NewPath: "pkg/main.go", Diff: sampleDiff},
		}, nil, logger.ForTests(t))

		if got := h.svc.buildFileContexts(t.Context(), "7", testHeadSHA, files, dir); len(got) != 0 {
			t.Errorf("a symlinked ancestor was followed out of the worktree: %+v", got)
		}
	})

	// The containment check must not reject the ordinary case, and on macOS the
	// temp root is itself a symlink (/var -> /private/var), which is exactly the
	// way a naive implementation rejects everything.
	t.Run("a symlink that stays inside the worktree is fine", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, withConfig(func(c *Config) {
			c.Context = review.ContextBudget{
				IncludeFullFiles: true, MaxFileLines: 500, HunkWindowLines: 60, MaxTotalBytes: 64 << 10,
			}
		}))
		dir := t.TempDir()
		writeFile(t, dir+"/real.go", "package main\n\nfunc added() {}\n// linked inside\n")
		if err := os.Symlink(dir+"/real.go", dir+"/main.go"); err != nil {
			t.Fatalf("symlink: %v", err)
		}

		got := h.svc.buildFileContexts(t.Context(), "7", testHeadSHA, sampleFiles(t), dir)
		if len(got) != 1 || !strings.Contains(got[0].Rendered, "linked inside") {
			t.Errorf("an in-worktree symlink must still be readable: %+v", got)
		}
	})

	t.Run("an unreadable file simply contributes nothing", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, withConfig(func(c *Config) {
			c.Context = review.ContextBudget{
				IncludeFullFiles: true, MaxFileLines: 500, HunkWindowLines: 60, MaxTotalBytes: 64 << 10,
			}
		}))
		if got := h.svc.buildFileContexts(t.Context(), "7", testHeadSHA, sampleFiles(t), ""); len(got) != 0 {
			t.Errorf("file contexts = %d, want none when the file cannot be read", len(got))
		}
	})
}

func TestBuildCommits(t *testing.T) {
	t.Parallel()
	h := newHarness(t, withConfig(func(c *Config) { c.Context.IncludeCommits = true }))
	h.fake.Commits[fakeKey("7", testMRIID)] = []gitlab.Commit{
		// GitLab returns newest first.
		{ShortID: "ccc", Title: "third", Message: "third\n\nbody three"},
		{ShortID: "bbb", Title: "second", Message: "second"},
		{ShortID: "aaa", Title: "first", Message: "first"},
	}

	got := h.svc.buildCommits(t.Context(), "7", testMRIID)
	if len(got) != 3 {
		t.Fatalf("commits = %d, want 3", len(got))
	}
	if got[0].ShortSHA != "aaa" || got[2].ShortSHA != "ccc" {
		t.Errorf("commit order = %v, want oldest first so the prompt reads as a story", got)
	}
	if got[2].Message != "body three" {
		t.Errorf("message = %q, want the title stripped", got[2].Message)
	}

	h.cfg.Context.IncludeCommits = false
	off := newService(Deps{GitLab: h.gl, Log: logger.ForTests(t)}, h.cfg)
	if c := off.buildCommits(t.Context(), "7", testMRIID); c != nil {
		t.Errorf("commits = %v, want none when the section is off", c)
	}
}

func TestBuildCommitsCapsTheList(t *testing.T) {
	t.Parallel()
	h := newHarness(t, withConfig(func(c *Config) { c.Context.IncludeCommits = true }))
	var commits []gitlab.Commit
	for i := range maxCommitCount + 10 {
		commits = append(commits, gitlab.Commit{ShortID: string(rune('a' + i%26)), Title: "c"})
	}
	h.fake.Commits[fakeKey("7", testMRIID)] = commits

	if got := h.svc.buildCommits(t.Context(), "7", testMRIID); len(got) != maxCommitCount {
		t.Errorf("commits = %d, want the list capped at %d", len(got), maxCommitCount)
	}
}

func TestBuildDiscussionNotes(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	line := 3
	got := h.svc.buildDiscussionNotes([]gitlab.Discussion{{
		ID: "d1",
		Notes: []gitlab.Note{
			{ID: 1, Author: gitlab.User{Username: "dev"}, Body: "please fix"},
			{ID: 2, Author: gitlab.User{Username: "dev"}, Body: "added 3 commits", System: true},
			{ID: 3, Author: gitlab.User{Username: "dev"}, Body: "   "},
			{ID: 4, Author: gitlab.User{Username: "ai-reviewer"}, Body: "our finding", Resolved: true,
				Position: &gitlab.Position{NewPath: "main.go", NewLine: &line}},
			{ID: 5, Author: gitlab.User{Username: "dev"}, Body: "old line",
				Position: &gitlab.Position{OldPath: "main.go", NewPath: "main.go", OldLine: &line}},
		},
	}})

	if len(got) != 3 {
		t.Fatalf("notes = %d, want 3 (system and blank notes are dropped)", len(got))
	}
	ours := got[1]
	if !ours.OwnBot {
		t.Error("our own note must be marked so a re-review can see what it said last time")
	}
	if !ours.Resolved || ours.FilePath != "main.go" || ours.Line != 3 {
		t.Errorf("inline note = %+v, want file main.go line 3 resolved", ours)
	}
	if got[2].Line != 3 {
		t.Errorf("removed-line note = %+v, want the old line number", got[2])
	}
}

func TestBuildDiscussionNotesKeepsTheNewestWithinBudget(t *testing.T) {
	t.Parallel()
	h := newHarness(t, withConfig(func(c *Config) { c.Context.MaxDiscussionBytes = 80 }))
	var notes []gitlab.Note
	for i := range 10 {
		notes = append(notes, gitlab.Note{
			ID: int64(i), Author: gitlab.User{Username: "dev"}, Body: strings.Repeat("x", 40),
		})
	}
	got := h.svc.buildDiscussionNotes([]gitlab.Discussion{{ID: "d", Notes: notes}})
	if len(got) == 0 || len(got) == 10 {
		t.Fatalf("notes = %d, want the oldest dropped but some kept", len(got))
	}
}

func TestBuildDiscussionNotesDisabled(t *testing.T) {
	t.Parallel()
	h := newHarness(t, withConfig(func(c *Config) { c.Context.IncludeDiscussions = false }))
	if got := h.svc.buildDiscussionNotes([]gitlab.Discussion{{Notes: []gitlab.Note{{Body: "hi"}}}}); got != nil {
		t.Errorf("notes = %v, want none when the section is off", got)
	}
}

func TestBuildPriorReview(t *testing.T) {
	h := newHarness(t, withDB)

	if got := h.svc.buildPriorReview(t.Context(), testProject(), testMRIID, testHeadSHA); got != nil {
		t.Errorf("prior review = %+v, want nil on a first review", got)
	}

	id := h.seedReview(t, "older00", StatusSucceeded)
	if _, err := h.pool.Exec(t.Context(),
		`UPDATE mr_reviews SET summary = 'the previous verdict' WHERE id = $1`, id); err != nil {
		t.Fatalf("set summary: %v", err)
	}

	got := h.svc.buildPriorReview(t.Context(), testProject(), testMRIID, testHeadSHA)
	if got == nil {
		t.Fatal("prior review = nil, want the previous review on a re-review")
	}
	if got.HeadSHA != "older00" || got.Summary != "the previous verdict" {
		t.Errorf("prior review = %+v, want the older head and its summary", got)
	}
	if got.Interdiff != "" {
		t.Error("without a mirror there is no interdiff to include")
	}

	// Reviewing the same SHA the prior review covered is not a re-review.
	if same := h.svc.buildPriorReview(t.Context(), testProject(), testMRIID, "older00"); same != nil {
		t.Errorf("prior review = %+v, want nil when the head has not moved", same)
	}

	h.svc.cfg.Context.IncludePriorReview = false
	if off := h.svc.buildPriorReview(t.Context(), testProject(), testMRIID, testHeadSHA); off != nil {
		t.Errorf("prior review = %+v, want none when the section is off", off)
	}
}

// TestBuildPriorReviewCarriesTheFindings is §11's mr_findings continuity. The
// discussions half was wired and this half was not, so the prompt block "Prior
// findings and their dispositions" and its "do not re-raise a published finding"
// rule referred to an empty list on every re-review — the model re-derived
// findings that the deterministic fingerprint gate then discarded.
func TestBuildPriorReviewCarriesTheFindings(t *testing.T) {
	h := newHarness(t, withDB)

	prior := h.seedReview(t, "older00", StatusSucceeded)
	published := h.seedFinding(t, prior, "fp-published")
	h.seedFinding(t, prior, "fp-pending")
	if err := h.st.Finding().MarkPublished(t.Context(), 4242, published); err != nil {
		t.Fatalf("MarkPublished: %v", err)
	}
	if _, err := h.pool.Exec(t.Context(),
		`UPDATE mr_findings SET position_json = '{"new_path":"main.go","new_line":3}' WHERE id = $1`,
		published); err != nil {
		t.Fatalf("set position: %v", err)
	}

	got := h.svc.buildPriorReview(t.Context(), testProject(), testMRIID, testHeadSHA)
	if got == nil {
		t.Fatal("prior review = nil")
	}
	if len(got.Findings) != 2 {
		t.Fatalf("prior findings = %+v, want both of the previous review's findings", got.Findings)
	}
	// Whether a finding is on the merge request is the whole disposition
	// vocabulary now; the old proposed/approved/rejected set described a local
	// approval UI that no longer exists.
	byStatus := map[string]review.PriorFinding{}
	for _, f := range got.Findings {
		byStatus[f.Status] = f
	}
	pub, ok := byStatus[review.PriorPublished]
	if !ok {
		t.Fatalf("no published finding in %+v", got.Findings)
	}
	if _, ok := byStatus[review.PriorPending]; !ok {
		t.Fatalf("no pending finding in %+v", got.Findings)
	}
	if pub.FilePath != "main.go" || pub.Severity != "high" || pub.Title != "leaks a connection" {
		t.Errorf("published finding = %+v, want the stored file, severity and title", pub)
	}
	// The line is the one Go computed and stored, never recomputed against a
	// newer diff.
	if pub.Line != 3 {
		t.Errorf("line = %d, want the stored position's new_line 3", pub.Line)
	}
}

func TestBuildRiskReport(t *testing.T) {
	t.Parallel()
	h := newHarness(t, withConfig(func(c *Config) {
		c.Risk = RiskSettings{Enabled: true, SensitiveGlobs: []string{"**/*.sql"}}
	}))

	files := parseDiffs([]gitlab.MergeRequestDiff{
		{NewPath: "main.go", Diff: sampleDiff},
		{NewPath: "main_test.go", Diff: sampleDiff},
		{NewPath: "db/schema.sql", Diff: sampleDiff},
	}, nil, logger.ForTests(t))

	got := h.svc.buildRiskReport(t.Context(), testProject(), files)
	if got == nil {
		t.Fatal("risk report = nil, want a report when risk scoring is on")
	}
	if got.Level == "" {
		t.Error("the report must carry a level")
	}

	h.svc.cfg.Risk.Enabled = false
	if off := h.svc.buildRiskReport(t.Context(), testProject(), files); off != nil {
		t.Errorf("risk report = %+v, want none when risk scoring is off", off)
	}
}

func TestBuildCoverageReport(t *testing.T) {
	t.Parallel()
	files := sampleFiles(t)

	t.Run("off by default", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		if got := h.svc.buildCoverageReport(t.Context(), t.TempDir(), files); got != nil {
			t.Errorf("coverage = %+v, want none — measuring executes repository code", got)
		}
	})

	t.Run("needs a worktree", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, withConfig(func(c *Config) { c.Coverage.Enabled = true }))
		if got := h.svc.buildCoverageReport(t.Context(), "", files); got != nil {
			t.Errorf("coverage = %+v, want none without a worktree", got)
		}
	})

	t.Run("no providers configured", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, withConfig(func(c *Config) { c.Coverage.Enabled = true }))
		if got := h.svc.buildCoverageReport(t.Context(), t.TempDir(), files); got != nil {
			t.Errorf("coverage = %+v, want none with no providers", got)
		}
	})

	t.Run("nothing added", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, withConfig(func(c *Config) {
			c.Coverage.Enabled = true
			c.Coverage.Providers = []string{"go"}
		}))
		deleted := parseDiffs([]gitlab.MergeRequestDiff{
			{OldPath: "main.go", DeletedFile: true, Diff: "@@ -1,2 +0,0 @@\n-package main\n-\n"},
		}, nil, logger.ForTests(t))
		if got := h.svc.buildCoverageReport(t.Context(), t.TempDir(), deleted); got != nil {
			t.Errorf("coverage = %+v, want none when nothing was added", got)
		}
	})
}

func TestPrepareWorktree(t *testing.T) {
	t.Parallel()

	t.Run("no cache means diff-only review", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, withConfig(func(c *Config) { c.AgentMode = true }))
		dir, agent, cleanup, err := h.svc.prepareWorktree(t.Context(), testProject(), testHeadSHA)
		defer cleanup()
		if err != nil {
			t.Fatalf("prepareWorktree: %v", err)
		}
		if dir != "" || agent {
			t.Errorf("prepareWorktree = (%q, %v), want diff-only without a git cache", dir, agent)
		}
	})

	t.Run("agent mode off", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.svc.cache = git.NewCache(t.TempDir(), logger.ForTests(t))
		dir, agent, cleanup, err := h.svc.prepareWorktree(t.Context(), testProject(), testHeadSHA)
		defer cleanup()
		if err != nil {
			t.Fatalf("prepareWorktree: %v", err)
		}
		if dir != "" || agent {
			t.Errorf("prepareWorktree = (%q, %v), want diff-only with agent mode off", dir, agent)
		}
	})

	t.Run("an unreachable repository degrades instead of failing the review", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, withConfig(func(c *Config) { c.AgentMode = true }))
		h.svc.cache = git.NewCache(t.TempDir(), logger.ForTests(t))
		proj := testProject()
		proj.HTTPURLToRepo = "file:///definitely/not/a/repository.git"

		dir, agent, cleanup, err := h.svc.prepareWorktree(t.Context(), proj, testHeadSHA)
		defer cleanup()
		if err != nil {
			t.Fatalf("an unreachable repository must degrade, not fail the review: %v", err)
		}
		if dir != "" || agent {
			t.Errorf("prepareWorktree = (%q, %v), want a diff-only fallback", dir, agent)
		}
	})

	// The failure this pins cost real money on the first live run: Ctrl-C killed
	// `git fetch` through the process group, the mirror error was read as ordinary
	// infrastructure trouble, and the review degraded to diff-only and went on to
	// start LLM passes while the process was already shutting down.
	t.Run("shutdown aborts the review instead of degrading it", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, withConfig(func(c *Config) { c.AgentMode = true }))
		h.svc.cache = git.NewCache(t.TempDir(), logger.ForTests(t))
		proj := testProject()
		proj.HTTPURLToRepo = "file:///definitely/not/a/repository.git"

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		dir, agent, cleanup, err := h.svc.prepareWorktree(ctx, proj, testHeadSHA)
		defer cleanup()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("prepareWorktree err = %v, want a wrapped context.Canceled", err)
		}
		if dir != "" || agent {
			t.Errorf("prepareWorktree = (%q, %v), want nothing on the abort path", dir, agent)
		}
	})

	// The other half of that discrimination, and the expensive one to get wrong.
	// A wrapped context.Canceled from here is what makes recordFailure discard the
	// attempt row, so returning one for an expired *deadline* means the §6.5 ladder
	// never counts a head SHA that cannot be reviewed inside jobs.ReviewTimeout —
	// it is re-enqueued at full price on every scan, forever.
	t.Run("a job timeout is the merge request's fault and must not read as shutdown", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, withConfig(func(c *Config) { c.AgentMode = true }))
		h.svc.cache = git.NewCache(t.TempDir(), logger.ForTests(t))
		proj := testProject()
		proj.HTTPURLToRepo = "file:///definitely/not/a/repository.git"

		// Already past its deadline, exactly as River's job context is when the
		// 30-minute budget runs out inside worktree preparation.
		ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
		defer cancel()

		dir, agent, cleanup, err := h.svc.prepareWorktree(ctx, proj, testHeadSHA)
		defer cleanup()
		if err != nil {
			t.Fatalf("prepareWorktree err = %v, want a degraded diff-only review so the failure stays countable", err)
		}
		if errors.Is(err, context.Canceled) {
			t.Error("a deadline must never surface as context.Canceled: recordFailure would discard the attempt row")
		}
		if dir != "" || agent {
			t.Errorf("prepareWorktree = (%q, %v), want a diff-only fallback", dir, agent)
		}
	})
}

// signallingLocker is a git.Locker that hands out n successful locks and then
// fails with err. It is the only seam that reaches prepareWorktree's abort paths
// with a HEALTHY context — the case that actually happened in production, where
// the terminal killed our git child directly and the child's error is the sole
// evidence that a shutdown is under way.
type signallingLocker struct {
	mu        sync.Mutex
	remaining int
	err       error
}

func (l *signallingLocker) Lock(context.Context, string) (func(), error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.remaining > 0 {
		l.remaining--
		return func() {}, nil
	}
	return nil, l.err
}

// TestPrepareWorktreeAbortErrorCarriesTheCause pins both halves of the abort
// error at once, because each is useless without the other.
//
// The wrapped context.Canceled is load-bearing: recordFailure discriminates on it
// to discard the attempt row instead of counting it against the merge request.
// The cause text is what makes the resulting log line answerable — an abort that
// says only "interrupted: context canceled" names neither the signal nor the
// operation, which is precisely how a misclassified SIGKILL from the OOM killer
// survived a live run looking exactly like a clean shutdown.
func TestPrepareWorktreeAbortErrorCarriesTheCause(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	t.Parallel()

	// A git child killed by SIGINT, as os/exec reports it, wrapped the way
	// Cache.run wraps it. The context stays healthy throughout: only
	// git.Signalled can see that this is a shutdown.
	sigint := func(t *testing.T) error {
		return fmt.Errorf("fetch mirror: %w", signalledChild(t, syscall.SIGINT))
	}

	t.Run("mirror fetch", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, withConfig(func(c *Config) { c.AgentMode = true }))
		h.svc.cache = git.NewCache(t.TempDir(), logger.ForTests(t),
			git.WithLocker(&signallingLocker{err: sigint(t)}))
		proj := testProject()
		// Never reached: the locker fails before any git runs.
		proj.HTTPURLToRepo = "file:///definitely/not/a/repository.git"

		_, _, cleanup, err := h.svc.prepareWorktree(t.Context(), proj, testHeadSHA)
		defer cleanup()
		assertAbortError(t, err, "mirror fetch interrupted")
	})

	t.Run("worktree checkout", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t, withConfig(func(c *Config) { c.AgentMode = true }))
		proj := testProject()
		cache := git.NewCache(t.TempDir(), logger.ForTests(t),
			// One lock for EnsureMirror, then the failure — so the abort happens at
			// AddWorktree, the half no test used to reach at all.
			git.WithLocker(&signallingLocker{remaining: 1, err: sigint(t)}))
		h.svc.cache = cache

		// A real mirror with a real origin, placed where the cache expects it, so
		// EnsureMirror takes its fetch path and succeeds.
		src := initGitRepo(t)
		proj.HTTPURLToRepo = "file://" + src
		gitCmd(t, "", "clone", "--mirror", "--quiet", src, cache.BareDir(h.svc.cfg.Host, proj.PathWithNamespace))

		_, _, cleanup, err := h.svc.prepareWorktree(t.Context(), proj, testHeadSHA)
		defer cleanup()
		assertAbortError(t, err, "worktree checkout interrupted")
	})
}

// assertAbortError checks the two facts an abort error must carry together.
func assertAbortError(t *testing.T, err error, wantOp string) {
	t.Helper()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("prepareWorktree err = %v, want a wrapped context.Canceled — recordFailure discriminates on it", err)
	}
	if !strings.Contains(err.Error(), wantOp) {
		t.Errorf("err = %q, want it to name the operation (%q)", err, wantOp)
	}
	if !strings.Contains(err.Error(), "signal: interrupt") {
		t.Errorf("err = %q, want the git failure readable in the message; an abort naming no signal is unanswerable in the log", err)
	}
}

// initGitRepo creates a one-commit repository and returns its path.
func initGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitCmd(t, dir, "init", "-q", "-b", "main")
	gitCmd(t, dir, "config", "user.email", "t@example.com")
	gitCmd(t, dir, "config", "user.name", "Tester")
	writeFile(t, dir+"/main.go", mainGoContent)
	gitCmd(t, dir, "add", ".")
	gitCmd(t, dir, "commit", "-q", "-m", "init")
	return dir
}

func gitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// signalledChild starts a child, kills it with sig and returns the error os/exec
// reports — the shape a git failure has when the signal reached the child
// directly rather than through our own context.
func signalledChild(t *testing.T, sig syscall.Signal) error {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	// SIGQUIT's default disposition is a core dump; keep any core file out of the
	// package directory.
	cmd.Dir = t.TempDir()
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	if err := cmd.Process.Signal(sig); err != nil {
		t.Fatalf("signal sleep with %v: %v", sig, err)
	}
	err := cmd.Wait()
	if err == nil {
		t.Fatalf("a child killed by %v must report an error", sig)
	}
	return err
}

func TestInterruptedByShutdown(t *testing.T) {
	t.Parallel()

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()

	// A deadline that has already passed: this is River cancelling the review job
	// at jobs.ReviewTimeout, not an operator stopping the service.
	expired, cancelExpired := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancelExpired()

	tests := []struct {
		name string
		ctx  context.Context
		err  error
		want bool
	}{
		{name: "healthy", ctx: t.Context(), err: errors.New("remote: repository not found"), want: false},
		{name: "cancelled context", ctx: cancelled, err: errors.New("fetch mirror: 128"), want: true},
		// A git child killed by the SIGINT the terminal sent to the whole process
		// group, as os/exec reports it. This is the case a cancelled context misses:
		// the child dies before our own cancellation arrives.
		{
			name: "signalled child",
			ctx:  t.Context(),
			err:  fmt.Errorf("fetch mirror: %w", signalledChild(t, syscall.SIGINT)),
			want: true,
		},
		{
			name: "SIGTERM child",
			ctx:  t.Context(),
			err:  fmt.Errorf("fetch mirror: %w", signalledChild(t, syscall.SIGTERM)),
			want: true,
		},
		// The two cases that must stay countable. An expired deadline is the MR's
		// own fault, and the OOM killer's SIGKILL on a fetch of a huge mirror is
		// infrastructure trouble — reading either as shutdown discards the attempt
		// row and silently re-reviews the same head SHA forever.
		{name: "expired deadline", ctx: expired, err: errors.New("fetch mirror: 128"), want: false},
		{
			name: "OOM-killed child",
			ctx:  t.Context(),
			err:  fmt.Errorf("fetch mirror: %w", signalledChild(t, syscall.SIGKILL)),
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := interruptedByShutdown(tt.ctx, tt.err); got != tt.want {
				t.Errorf("interruptedByShutdown = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestChangedLineCount(t *testing.T) {
	t.Parallel()
	if got := changedLineCount(sampleFiles(t)[0]); got != 1 {
		t.Errorf("changedLineCount = %d, want 1 added line", got)
	}
}

func TestNewValidatesTheDependenciesNothingCanWorkWithout(t *testing.T) {
	t.Parallel()
	if _, err := New(Deps{}, Config{}); err == nil {
		t.Error("New without a GitLab client must fail")
	}
	if _, err := New(Deps{GitLab: gitlab.NewFake()}, Config{}); err == nil {
		t.Error("New without a store must fail")
	}
}

func TestNewAppliesDefaults(t *testing.T) {
	t.Parallel()
	svc := newService(Deps{GitLab: gitlab.NewFake()}, Config{})
	if svc.cfg.Profile == nil {
		t.Error("a nil profile must default rather than crash the first review")
	}
	if svc.cfg.StalePublishAfter != defaultStalePublishAfter {
		t.Errorf("StalePublishAfter = %v, want %v", svc.cfg.StalePublishAfter, defaultStalePublishAfter)
	}
	if svc.log == nil {
		t.Error("a nil logger must default")
	}
	if svc.now().IsZero() {
		t.Error("now() must fall back to the wall clock")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestBuildRiskReportWithoutAMirrorDropsTheHistoryFactors(t *testing.T) {
	t.Parallel()
	h := newHarness(t, withConfig(func(c *Config) {
		c.Risk = RiskSettings{Enabled: true, HistoryCommits: 100}
	}))
	// A cache that has never mirrored this project: git log fails, and the
	// report must degrade to history-free scoring rather than fail the review.
	h.svc.cache = git.NewCache(t.TempDir(), logger.ForTests(t))

	got := h.svc.buildRiskReport(t.Context(), testProject(), sampleFiles(t))
	if got == nil {
		t.Fatal("risk report = nil, want history-free scoring")
	}
}
