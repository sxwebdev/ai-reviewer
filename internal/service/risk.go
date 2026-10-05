package service

import (
	"context"
	"regexp"

	"github.com/sxwebdev/ai-reviewer/internal/coverage"
	"github.com/sxwebdev/ai-reviewer/internal/gitlab"
	"github.com/sxwebdev/ai-reviewer/internal/review"
	"github.com/sxwebdev/ai-reviewer/internal/toolchain"
)

// fixSubjectRe marks commit subjects that look like bug fixes, which is what
// turns raw churn into the "this file keeps breaking" risk factor.
var fixSubjectRe = regexp.MustCompile(`(?i)\b(fix|bug|hotfix|revert|regression|patch)\b`)

// buildRiskReport computes the deterministic risk report for the MR's diffs.
// Without a mirror (or on any git failure) it degrades to history-free scoring:
// the churn and bug-fix factors are simply absent.
func (s *Service) buildRiskReport(ctx context.Context, proj *gitlab.Project, files []*review.FileDiff) *review.RiskReport {
	if !s.cfg.Risk.Enabled {
		return nil
	}

	in := review.RiskInput{FilesChanged: len(files)}
	changed := make(map[string]bool, len(files))
	for _, f := range files {
		path := f.Path()
		changed[path] = true
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
		if toolchain.IsTestPath(path) {
			in.TestsTouched = true
		} else if hasCode && toolchain.IsSourceFile(path) {
			// Docs, config and data changes must not trip the "behaviour
			// changed without tests" factor.
			in.BehaviorFiles++
		}
		if toolchain.MatchGlob(path, s.cfg.Risk.SensitiveGlobs) {
			in.SensitiveHits = append(in.SensitiveHits, path)
		}
	}
	in.NewDependencies = review.DetectNewDependencies(files)

	if s.cache != nil {
		history, err := s.cache.RecentHistory(ctx, s.cfg.Host, proj.PathWithNamespace, s.cfg.Risk.HistoryCommits)
		if err != nil {
			s.log.Debugw("risk: git history unavailable", "err", err)
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

// buildCoverageReport runs the opt-in changed-line coverage measurement.
//
// It executes the reviewed repository's test code, which on a shared host is a
// real escalation of the threat model (§20.4) — hence the explicit opt-in and
// the hard requirement of a worktree. Best-effort: nil on any failure, with
// per-root problems surfaced as skip notes inside the report rather than as an
// error.
func (s *Service) buildCoverageReport(ctx context.Context, workDir string, files []*review.FileDiff) *coverage.Report {
	if !s.cfg.Coverage.Enabled || workDir == "" {
		return nil
	}

	var changed []string
	added := map[string][]int{}
	for _, f := range files {
		if f.Deleted {
			continue
		}
		path := f.Path()
		changed = append(changed, path)
		if lines := review.AddedLines(f); len(lines) > 0 {
			added[path] = lines
		}
	}
	if len(added) == 0 {
		return nil // nothing added — nothing to measure
	}

	providers := coverage.BuiltinProviders(s.cfg.Coverage.Providers, nil, s.cfg.Coverage.Options, s.log)
	if len(providers) == 0 {
		return nil
	}
	profile, skips, notes := coverage.Collect(ctx, workDir, changed, providers, s.cfg.Coverage.Options, s.log)
	report := coverage.Intersect(profile, added)
	report.Skipped = skips
	report.Notes = notes
	if len(report.Files) == 0 && len(report.Skipped) == 0 {
		return nil
	}
	s.log.Infow("changed-line coverage measured",
		"pct", report.Pct, "added", report.TotalAdded, "covered", report.TotalCovered, "skipped", len(skips))
	return report
}
