package app

import (
	"github.com/sxwebdev/ai-reviewer/internal/config"
	"github.com/sxwebdev/ai-reviewer/internal/coverage"
	"github.com/sxwebdev/ai-reviewer/internal/gitlab"
	"github.com/sxwebdev/ai-reviewer/internal/review"
	"github.com/sxwebdev/ai-reviewer/internal/service"
)

// pipelineFromConfig resolves the review.pipeline config block (including the
// mode presets) into the engine's pipeline config.
func pipelineFromConfig(rc config.ReviewConfig) review.PipelineConfig {
	p := rc.Pipeline
	out := review.PipelineConfig{
		MaxParallel:       p.MaxParallel,
		VerifyMode:        p.VerifyMode,
		VerifyMaxFindings: p.VerifyMaxFindings,
		Verifiers:         p.Verifiers,
	}
	// Tri-state completeness: an explicit "on" survives cheap mode; "auto"
	// follows the preset (on for everything but cheap).
	switch p.Completeness {
	case "on":
		out.Completeness = true
	case "off":
		out.Completeness = false
	default: // auto
		out.Completeness = p.Mode != "cheap"
	}
	switch p.Mode {
	case "cheap":
		out.Passes = []string{review.PassGeneral}
		out.VerifyMode = review.VerifyOff
	case "deep":
		out.Passes = []string{
			review.PassGeneral, review.PassCorrectness, review.PassConcurrency,
			review.PassSecurity, review.PassContracts,
		}
	case "custom":
		out.Passes = p.Passes
	default: // standard
		out.Passes = []string{review.PassGeneral, review.PassCorrectness}
	}
	return out
}

// riskSettingsFromConfig maps the review.risk config block to service settings.
func riskSettingsFromConfig(rc config.ReviewConfig) service.RiskSettings {
	return service.RiskSettings{
		Enabled:        rc.Risk.Enabled,
		HistoryCommits: rc.Risk.HistoryCommits,
		SensitiveGlobs: rc.Risk.SensitiveGlobs,
	}
}

// coverageSettingsFromConfig maps the review.coverage config block.
func coverageSettingsFromConfig(rc config.ReviewConfig) service.CoverageSettings {
	return service.CoverageSettings{
		Enabled:   rc.Coverage.Enabled,
		Providers: rc.Coverage.Providers,
		Options: coverage.Options{
			Timeout:     rc.Coverage.Timeout,
			NodeInstall: rc.Coverage.Node.Install,
		},
	}
}

// contextBudgetFromConfig maps the review.context config block to the engine's
// enrichment budget.
func contextBudgetFromConfig(rc config.ReviewConfig) review.ContextBudget {
	return review.ContextBudget{
		IncludeFullFiles:   rc.Context.IncludeFullFiles,
		MaxFileLines:       rc.Context.MaxFileLines,
		HunkWindowLines:    rc.Context.HunkWindowLines,
		MaxTotalBytes:      rc.Context.MaxTotalKB << 10,
		IncludeCommits:     rc.Context.IncludeCommits,
		IncludeDiscussions: rc.Context.IncludeDiscussions,
		MaxDiscussionBytes: rc.Context.MaxDiscussionKB << 10,
		IncludePriorReview: rc.Context.PriorReview,
		MaxInterdiffBytes:  rc.Context.InterdiffMaxKB << 10,
		MaxRelatedFiles:    rc.Context.RelatedFiles,
	}
}

// Services opens state and wires the full service bundle used by the web UI and
// CLI actions. When GitLab is not yet configured the bundle uses a stub client
// so the web UI still starts; sync/review then surface a clear error.
func (a *App) Services() (*service.Bundle, error) {
	db, err := a.OpenState()
	if err != nil {
		return nil, err
	}
	var gl gitlab.API
	if a.Cfg.GitLab.Host == "" {
		gl = gitlab.Unconfigured()
	} else {
		c, err := a.GitLabClient()
		if err != nil {
			return nil, err
		}
		gl = c
	}
	eng := review.NewEngine(a.LLMClient(), a.Log)
	rc := service.ReviewConfig{
		Host:             a.Cfg.GitLab.Host,
		ReviewerUsername: a.Cfg.GitLab.Username,
		Model:            a.Cfg.LLM.Model,
		LLMProvider:      a.Cfg.LLM.Provider,
		AgentMode:        a.Cfg.Review.AgentMode && a.Cfg.LLM.Claude.AgentMode,
		AllowedTools:     a.Cfg.LLM.Claude.AllowedTools,
		Profile:          profileFromConfig(a.Cfg.Review),
		Token:            a.Cfg.GitLabToken(),
		CacheDir:         a.Cfg.Storage.CacheDir,
		IgnoreGlobs:      a.Cfg.Review.IgnoreGlobs,
		Context:          contextBudgetFromConfig(a.Cfg.Review),
		Pipeline:         pipelineFromConfig(a.Cfg.Review),
		Risk:             riskSettingsFromConfig(a.Cfg.Review),
		Coverage:         coverageSettingsFromConfig(a.Cfg.Review),
	}
	return service.NewBundle(gl, db, eng, rc, a.Log), nil
}
