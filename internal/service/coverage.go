package service

import (
	"context"

	"github.com/sxwebdev/ai-reviewer/internal/coverage"
	"github.com/sxwebdev/ai-reviewer/internal/review"
)

// CoverageSettings configures opt-in changed-line coverage measurement.
type CoverageSettings struct {
	Enabled   bool
	Providers []string
	Options   coverage.Options
}

// buildCoverageReport runs the opt-in changed-line coverage measurement.
// Requires an agent-mode worktree; best-effort — nil on any failure, per-root
// failures become skip notes inside the report.
func (s *ReviewService) buildCoverageReport(ctx context.Context, workDir string, files []*review.FileDiff) *coverage.Report {
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
	s.log.Info("changed-line coverage measured",
		"pct", report.Pct, "added", report.TotalAdded, "covered", report.TotalCovered, "skipped", len(skips))
	return report
}
