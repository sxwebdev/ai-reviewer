package service

import (
	"context"
	"regexp"

	"github.com/sxwebdev/ai-reviewer/internal/gitlab"
	"github.com/sxwebdev/ai-reviewer/internal/review"
	"github.com/sxwebdev/ai-reviewer/internal/toolchain"
)

// fixSubjectRe marks commit subjects that look like bug fixes.
var fixSubjectRe = regexp.MustCompile(`(?i)\b(fix|bug|hotfix|revert|regression|patch)\b`)

// buildRiskReport computes the deterministic risk report for the MR's diffs.
// Best-effort: without a mirror (or on any git failure) it degrades to
// history-free scoring (churn/bug-fix factors empty).
func (s *ReviewService) buildRiskReport(ctx context.Context, proj *gitlab.Project, files []*review.FileDiff) *review.RiskReport {
	if !s.cfg.Risk.Enabled {
		return nil
	}

	in := review.RiskInput{FilesChanged: len(files)}
	changed := map[string]bool{}
	for _, f := range files {
		path := f.Path()
		changed[path] = true
		isTest := toolchain.IsTestPath(path)
		hasCode := false
		for _, h := range f.Hunks {
			for _, l := range h.Lines {
				switch l.Kind {
				case review.LineAdded:
					in.LinesAdded++
					hasCode = true
				case review.LineRemoved:
					in.LinesRemoved++
					hasCode = true
				}
			}
		}
		if isTest {
			in.TestsTouched = true
		} else if hasCode {
			in.BehaviorFiles++
		}
		if toolchain.MatchGlob(path, s.cfg.Risk.SensitiveGlobs) {
			in.SensitiveHits = append(in.SensitiveHits, path)
		}
	}
	in.NewDependencies = review.DetectNewDependencies(files)

	if s.cache != nil {
		if history, err := s.cache.RecentHistory(ctx, s.cfg.Host, proj.PathWithNamespace, s.cfg.Risk.HistoryCommits); err != nil {
			s.log.Debug("risk: git history unavailable", "err", err)
		} else {
			in.ChurnByFile = map[string]int{}
			in.FixesByFile = map[string]int{}
			for _, c := range history {
				isFix := fixSubjectRe.MatchString(c.Subject)
				for _, p := range c.Paths {
					if !changed[p] {
						continue
					}
					in.ChurnByFile[p]++
					if isFix {
						in.FixesByFile[p]++
					}
				}
			}
		}
	}

	report := review.ComputeRisk(in)
	return &report
}
