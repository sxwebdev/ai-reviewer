package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sxwebdev/ai-reviewer/internal/gitlab"
	"github.com/sxwebdev/ai-reviewer/internal/review"
	"github.com/sxwebdev/ai-reviewer/internal/security"
	"github.com/sxwebdev/ai-reviewer/internal/toolchain"
)

// Bounds on the prompt context that are properties of the prompt rather than of
// the operator's budget: a single pathological file, an MR with a thousand
// commits, a novel-length discussion note.
const (
	maxContextFileBytes = 1 << 20
	maxCommitCount      = 30
	maxCommitMsgLen     = 400
	maxDiscussionLen    = 300
)

// prepareWorktree checks out a read-only worktree of the repository at headSHA
// so the model can inspect code the diff does not show.
//
// Every failure degrades to a diff-only review rather than failing the run: an
// unreachable mirror is an infrastructure problem, not a reason to skip the MR.
// The returned cleanup is always non-nil.
func (s *Service) prepareWorktree(ctx context.Context, proj *gitlab.Project, headSHA string) (dir string, agent bool, cleanup func()) {
	noop := func() {}
	if s.cache == nil || !s.cfg.AgentMode || proj.HTTPURLToRepo == "" || headSHA == "" {
		return "", false, noop
	}
	if _, err := s.cache.EnsureMirror(ctx, proj.HTTPURLToRepo, s.cfg.Host, proj.PathWithNamespace, s.cfg.Token); err != nil {
		s.log.Warnw("agent mode: mirror failed, falling back to diff-only review", "err", err)
		return "", false, noop
	}
	wt, done, err := s.cache.AddWorktree(ctx, s.cfg.Host, proj.PathWithNamespace, headSHA)
	if err != nil {
		s.log.Warnw("agent mode: worktree failed, falling back to diff-only review", "err", err)
		return "", false, noop
	}
	return wt, true, done
}

// buildFileContexts assembles full or hunk-windowed content of the changed
// files within the configured byte budget, most-edited file first so the
// heaviest change wins the shared budget. Content comes from the worktree when
// there is one and from the raw-file API otherwise. Every failure is
// best-effort: a file that cannot be read simply contributes no context.
func (s *Service) buildFileContexts(ctx context.Context, pk, headSHA string, files []*review.FileDiff, workDir string) []review.FileContext {
	budget := s.cfg.Context
	if !budget.IncludeFullFiles {
		return nil
	}

	ordered := make([]*review.FileDiff, len(files))
	copy(ordered, files)
	sort.SliceStable(ordered, func(i, j int) bool {
		return changedLineCount(ordered[i]) > changedLineCount(ordered[j])
	})

	remaining := budget.MaxTotalBytes
	var out []review.FileContext
	for _, f := range ordered {
		if f.Deleted || remaining <= 0 {
			continue
		}
		path := f.Path()
		content, ok := s.readChangedFile(ctx, pk, path, headSHA, workDir)
		if !ok {
			continue
		}
		fc := review.BuildFileContext(f, string(content), budget.MaxFileLines, budget.HunkWindowLines)
		if fc == nil {
			continue
		}
		rendered := review.RenderFileContext(*fc)
		if len(rendered) > remaining && !fc.Truncated {
			// Whole file over budget: retry as windows around the hunks.
			if fc = review.BuildFileContext(f, string(content), 0, budget.HunkWindowLines); fc == nil {
				continue
			}
			rendered = review.RenderFileContext(*fc)
		}
		if len(rendered) > remaining {
			s.log.Debugw("file context skipped: over budget", "path", path, "size", len(rendered))
			continue
		}
		// Cache the rendering so the prompt builder reuses it instead of
		// re-rendering the section once per pass.
		fc.Rendered = rendered
		remaining -= len(rendered)
		out = append(out, *fc)
	}

	// Restore diff order so the prompt layout is stable across runs.
	pos := make(map[string]int, len(files))
	for i, f := range files {
		pos[f.Path()] = i
	}
	sort.SliceStable(out, func(i, j int) bool { return pos[out[i].Path] < pos[out[j].Path] })
	return out
}

// readChangedFile loads a changed file at the review head, preferring the local
// worktree. Binary and oversized content is rejected before it can reach a
// prompt.
func (s *Service) readChangedFile(ctx context.Context, pk, path, headSHA, workDir string) ([]byte, bool) {
	var (
		content []byte
		err     error
	)
	if workDir != "" {
		content, err = readWithinWorktree(workDir, path)
	} else {
		content, err = s.gl.GetRawFile(ctx, pk, path, headSHA)
	}
	if err != nil {
		s.log.Debugw("read changed file failed", "path", path, "err", err)
		return nil, false
	}
	if len(content) > maxContextFileBytes || toolchain.LooksBinary(content) {
		return nil, false
	}
	return content, true
}

// readWithinWorktree reads one changed file out of the checked-out worktree,
// refusing to leave it.
//
// A symlink is an ordinary text blob in a git diff (mode 120000): not binary,
// not generated, not vendored, so parseDiffs keeps it and its target's contents
// would become a "## File:" section of a prompt built from an MR that anybody
// with push access to a watched repository controls. Committing
// `app.env -> /etc/ai-reviewer/config.yaml` is enough; no agent mode and no
// cooperation from the model are involved, because this is plain Go I/O that
// runs on every review.
//
// Containment is checked on the FULLY RESOLVED path rather than by rejecting
// symlinks outright, because a symlink inside a repository is ordinary and the
// property that matters is where the bytes come from, not how the path spells
// it. Resolving also covers the two variants a leaf-only check misses: a
// symlinked ancestor directory, and a diff path containing "..". The regular-file
// check then runs on the resolved entry, which keeps devices, FIFOs (a read that
// never returns) and directories out.
//
// The worktree root is resolved too. On macOS the temp root is itself a symlink
// (/var → /private/var), so comparing against an unresolved root would reject
// every legitimate read — the failure mode that makes people delete the check.
func readWithinWorktree(workDir, path string) ([]byte, error) {
	root, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		return nil, fmt.Errorf("resolve worktree: %w", err)
	}

	resolved, err := filepath.EvalSymlinks(filepath.Join(root, path))
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("%s resolves outside the worktree", path)
	}

	info, err := os.Lstat(resolved)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file (%s)", path, info.Mode().Type())
	}
	return os.ReadFile(resolved)
}

// buildCommits maps the MR's commits to prompt context — oldest first, capped,
// with message bodies truncated. It is the cheapest signal of intent an MR
// description often lacks. Best-effort: an error yields no section.
func (s *Service) buildCommits(ctx context.Context, pk string, iid int64) []review.CommitInfo {
	if !s.cfg.Context.IncludeCommits {
		return nil
	}
	commits, err := s.gl.ListMRCommits(ctx, pk, iid)
	if err != nil {
		s.log.Debugw("list MR commits failed", "err", err)
		return nil
	}
	if len(commits) > maxCommitCount {
		commits = commits[:maxCommitCount] // the API returns newest first
	}
	out := make([]review.CommitInfo, 0, len(commits))
	for i := len(commits) - 1; i >= 0; i-- { // reverse: the prompt reads oldest → newest
		c := commits[i]
		msg := strings.TrimSpace(strings.TrimPrefix(c.Message, c.Title))
		out = append(out, review.CommitInfo{
			ShortSHA: c.ShortID,
			Title:    c.Title,
			Message:  security.Truncate(msg, maxCommitMsgLen),
		})
	}
	return out
}

// buildDiscussionNotes flattens the MR's discussions into prompt context,
// dropping system notes and keeping the newest notes when over budget.
//
// This is what turns human reactions into review input (§11): a thread we
// opened last time that is now resolved, or that carries a developer's reply,
// arrives at the model as a settled topic instead of being raised again.
func (s *Service) buildDiscussionNotes(discussions []gitlab.Discussion) []review.DiscussionNote {
	if !s.cfg.Context.IncludeDiscussions {
		return nil
	}
	var out []review.DiscussionNote
	for _, d := range discussions {
		for _, n := range d.Notes {
			if n.System || strings.TrimSpace(n.Body) == "" {
				continue
			}
			note := review.DiscussionNote{
				Author:   n.Author.Username,
				Body:     security.Truncate(strings.TrimSpace(n.Body), maxDiscussionLen),
				Resolved: n.Resolved,
				OwnBot:   s.cfg.ReviewerUsername != "" && n.Author.Username == s.cfg.ReviewerUsername,
			}
			if n.Position != nil {
				note.FilePath = n.Position.NewPath
				switch {
				case n.Position.NewLine != nil:
					note.Line = *n.Position.NewLine
				case n.Position.OldLine != nil:
					note.Line = *n.Position.OldLine
				}
			}
			out = append(out, note)
		}
	}

	budget := s.cfg.Context.MaxDiscussionBytes
	if budget <= 0 {
		return out
	}
	total := 0
	for i := len(out) - 1; i >= 0; i-- {
		total += len(out[i].Body) + len(out[i].Author) + len(out[i].FilePath) + 32
		if total > budget {
			out = out[i+1:]
			break
		}
	}
	return out
}

// buildPriorReview loads the previous review of this MR and the interdiff
// between its head and the current one, so a re-review concentrates on what
// actually changed instead of restating the first review.
//
// Best-effort throughout: no previous review, the same head SHA, or an
// unavailable mirror all yield nil and a first-review-style prompt.
func (s *Service) buildPriorReview(ctx context.Context, proj *gitlab.Project, iid int64, headSHA string) *review.PriorReview {
	if !s.cfg.Context.IncludePriorReview {
		return nil
	}
	prior, err := s.st.Review().GetLatestByMR(ctx, proj.ID, iid)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			s.log.Debugw("load prior review failed", "err", err)
		}
		return nil
	}
	if prior.HeadSha == "" || prior.HeadSha == headSHA {
		return nil
	}
	pr := &review.PriorReview{
		HeadSHA:  prior.HeadSha,
		Summary:  prior.Summary,
		Findings: s.priorFindings(ctx, prior.ID),
	}

	if s.cache != nil {
		interdiff, err := s.cache.DiffRange(ctx, s.cfg.Host, proj.PathWithNamespace,
			prior.HeadSha, headSHA, s.cfg.Context.MaxInterdiffBytes)
		if err != nil {
			s.log.Debugw("interdiff failed", "err", err)
		} else {
			pr.Interdiff = interdiff
		}
	}
	return pr
}

// priorFindings is the mr_findings half of §11's prior-review continuity: what
// the previous review of this MR raised, and whether it reached GitLab.
//
// Without it the prompt's "Prior findings and their dispositions" block and its
// "do not re-raise a published finding" rule refer to an empty list on every
// re-review, so the model re-derives findings that the deterministic
// ExistingFingerprints gate then silently discards — tokens spent to produce
// output that is thrown away.
//
// Best-effort like every other builder here: a read failure costs continuity,
// not the review.
func (s *Service) priorFindings(ctx context.Context, reviewID uuid.UUID) []review.PriorFinding {
	rows, err := s.st.Finding().ListByReview(ctx, reviewID)
	if err != nil {
		s.log.Debugw("load prior findings failed", "review_id", reviewID, "err", err)
		return nil
	}
	out := make([]review.PriorFinding, 0, len(rows))
	for _, f := range rows {
		status := review.PriorPending
		if f.NoteID.Valid {
			status = review.PriorPublished
		}
		pf := review.PriorFinding{
			Title:    f.Title,
			FilePath: f.FilePath,
			Severity: f.Severity,
			Status:   status,
		}
		// The line is the one Go computed and stored; recomputing it against a
		// newer diff is exactly how a prior finding starts pointing at the wrong
		// place. An unreadable position simply omits it.
		if pos := decodePosition(f.PositionJson, s.log); pos != nil {
			switch {
			case pos.NewLine != nil:
				pf.Line = *pos.NewLine
			case pos.OldLine != nil:
				pf.Line = *pos.OldLine
			}
		}
		out = append(out, pf)
	}
	return out
}

func changedLineCount(f *review.FileDiff) int {
	n := 0
	for _, h := range f.Hunks {
		for _, l := range h.Lines {
			if l.Kind != review.LineContext {
				n++
			}
		}
	}
	return n
}
