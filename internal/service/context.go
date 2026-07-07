package service

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sxwebdev/ai-reviewer/internal/gitlab"
	"github.com/sxwebdev/ai-reviewer/internal/review"
	"github.com/sxwebdev/ai-reviewer/internal/security"
	"github.com/sxwebdev/ai-reviewer/internal/toolchain"
)

// maxContextFileBytes guards against pathological single files before line
// segmentation even runs.
const maxContextFileBytes = 1 << 20

// buildFileContexts assembles full/windowed content of changed files within
// the configured budget. Content comes from the agent-mode worktree when
// available, otherwise from the GitLab raw-file API at the MR head SHA. All
// failures are best-effort: a file that cannot be read simply gets no context.
func (s *ReviewService) buildFileContexts(ctx context.Context, projectKey string, mr *gitlab.MergeRequest, files []*review.FileDiff, workDir string) []review.FileContext {
	budget := s.cfg.Context
	if !budget.IncludeFullFiles {
		return nil
	}

	// Most-edited files first so they win the shared budget.
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
		content, ok := s.readChangedFile(ctx, projectKey, mr, path, workDir)
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
			s.log.Debug("file context skipped: over budget", "path", path, "size", len(rendered))
			continue
		}
		// Cache the rendering so the prompt builder reuses it instead of
		// re-rendering the section for every pass.
		fc.Rendered = rendered
		remaining -= len(rendered)
		out = append(out, *fc)
	}
	// Restore diff order for a stable prompt layout.
	pos := make(map[string]int, len(files))
	for i, f := range files {
		pos[f.Path()] = i
	}
	sort.SliceStable(out, func(i, j int) bool { return pos[out[i].Path] < pos[out[j].Path] })
	return out
}

// readChangedFile loads a changed file's content at the MR head, preferring
// the local worktree. It rejects binary and oversized content.
func (s *ReviewService) readChangedFile(ctx context.Context, projectKey string, mr *gitlab.MergeRequest, path, workDir string) ([]byte, bool) {
	var content []byte
	var err error
	if workDir != "" {
		content, err = os.ReadFile(filepath.Join(workDir, path))
	} else {
		content, err = s.gl.GetRawFile(ctx, projectKey, path, mr.DiffRefs.HeadSHA)
	}
	if err != nil {
		s.log.Debug("read changed file failed", "path", path, "err", err)
		return nil, false
	}
	if len(content) > maxContextFileBytes || looksBinary(content) {
		return nil, false
	}
	return content, true
}

const (
	maxCommitCount   = 30
	maxCommitMsgLen  = 400
	maxDiscussionLen = 300
)

// buildCommits maps MR commits to prompt context: oldest first, capped, with
// message bodies truncated. Best-effort — errors yield no section.
func (s *ReviewService) buildCommits(ctx context.Context, projectKey string, iid int64) []review.CommitInfo {
	if !s.cfg.Context.IncludeCommits {
		return nil
	}
	commits, err := s.gl.ListMRCommits(ctx, projectKey, iid)
	if err != nil {
		s.log.Debug("list MR commits failed", "err", err)
		return nil
	}
	if len(commits) > maxCommitCount {
		commits = commits[:maxCommitCount] // API returns newest first — keep the newest
	}
	out := make([]review.CommitInfo, 0, len(commits))
	// Reverse so the prompt reads oldest → newest.
	for i := len(commits) - 1; i >= 0; i-- {
		c := commits[i]
		msg := strings.TrimSpace(strings.TrimPrefix(c.Message, c.Title))
		out = append(out, review.CommitInfo{
			ShortSHA: c.ShortID,
			Title:    c.Title,
			Message:  truncate(msg, maxCommitMsgLen),
		})
	}
	return out
}

// buildDiscussionNotes flattens existing discussions into prompt context,
// skipping system notes and staying within the discussion byte budget (oldest
// notes are dropped first when over budget).
func (s *ReviewService) buildDiscussionNotes(discussions []gitlab.Discussion) []review.DiscussionNote {
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
				Body:     truncate(strings.TrimSpace(n.Body), maxDiscussionLen),
				Resolved: n.Resolved,
				OwnBot:   n.Author.Username == s.cfg.ReviewerUsername,
			}
			if n.Position != nil {
				note.FilePath = n.Position.NewPath
				if n.Position.NewLine != nil {
					note.Line = *n.Position.NewLine
				} else if n.Position.OldLine != nil {
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

// buildPriorReview loads the newest completed review of this MR when its head
// SHA differs from the current one, together with its findings' dispositions
// and (when the mirror is available) the interdiff prevHead..newHead.
// Best-effort: any failure yields nil and a fresh-style review.
func (s *ReviewService) buildPriorReview(ctx context.Context, proj *gitlab.Project, mr *gitlab.MergeRequest, mrID int64) *review.PriorReview {
	if !s.cfg.Context.IncludePriorReview || s.db == nil {
		return nil
	}
	prior, err := s.db.GetLatestCompletedReviewForMR(ctx, mrID)
	if err != nil || prior.HeadSHA == "" || prior.HeadSHA == mr.DiffRefs.HeadSHA {
		return nil
	}
	pr := &review.PriorReview{HeadSHA: prior.HeadSHA, Summary: prior.Summary}

	if findings, err := s.db.ListFindingsByReview(ctx, prior.ID); err == nil {
		for _, f := range findings {
			pf := review.PriorFinding{
				Title: f.Title, FilePath: f.FilePath, Severity: f.Severity,
				Status: f.Status, RejectionReason: f.RejectionReason,
			}
			if f.NewLine != nil {
				pf.Line = int(*f.NewLine)
			} else if f.OldLine != nil {
				pf.Line = int(*f.OldLine)
			}
			pr.Findings = append(pr.Findings, pf)
		}
	}

	if s.cache != nil {
		interdiff, err := s.cache.DiffRange(ctx, s.cfg.Host, proj.PathWithNamespace,
			prior.HeadSHA, mr.DiffRefs.HeadSHA, s.cfg.Context.MaxInterdiffBytes)
		if err != nil {
			s.log.Debug("interdiff failed", "err", err)
		} else {
			pr.Interdiff = interdiff
		}
	}
	return pr
}

// maxRelatedIdentifiers caps how many diff identifiers feed the FTS query.
const maxRelatedIdentifiers = 12

// findRelatedFiles suggests likely-related repository files by matching the
// diff's strongest identifiers against the FTS index built for this head SHA
// (agent mode only). Changed, vendored, generated, and test files are
// excluded. Best-effort: no index or no matches yields no section.
func (s *ReviewService) findRelatedFiles(ctx context.Context, projectID int64, headSHA string, files []*review.FileDiff) []review.RelatedFile {
	limit := s.cfg.Context.MaxRelatedFiles
	if limit <= 0 || s.db == nil {
		return nil
	}
	idents := review.ExtractIdentifiers(files, maxRelatedIdentifiers)
	if len(idents) == 0 {
		return nil
	}
	// FTS5 OR-query over quoted identifiers.
	quoted := make([]string, len(idents))
	for i, id := range idents {
		quoted[i] = `"` + id + `"`
	}
	query := strings.Join(quoted, " OR ")

	changed := map[string]bool{}
	for _, f := range files {
		if f.NewPath != "" {
			changed[f.NewPath] = true
		}
		if f.OldPath != "" {
			changed[f.OldPath] = true
		}
	}

	matches, err := s.db.SearchRepoFiles(ctx, projectID, headSHA, query, limit*3)
	if err != nil {
		s.log.Debug("related-files search failed", "err", err)
		return nil
	}
	var out []review.RelatedFile
	for _, m := range matches {
		if changed[m.Path] || m.IsVendor || m.IsGenerated || m.IsTest {
			continue
		}
		out = append(out, review.RelatedFile{Path: m.Path, Reason: "shares identifiers with the diff"})
		if len(out) >= limit {
			break
		}
	}
	return out
}

// truncate cuts s to max bytes (single shared implementation).
func truncate(s string, max int) string { return security.Truncate(s, max) }

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

// looksBinary delegates to the shared classifier.
func looksBinary(content []byte) bool { return toolchain.LooksBinary(content) }
