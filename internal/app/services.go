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
	// Tri-state completeness: an explicit "on" survives cheap mode (and, in
	// the engine, bypasses the intent-text gate); "auto" follows the preset
	// (auto for everything but cheap).
	switch p.Completeness {
	case "on":
		out.Completeness = review.CompletenessOn
	case "off":
		out.Completeness = review.CompletenessOff
	default: // auto
		if p.Mode != "cheap" {
			out.Completeness = review.CompletenessAuto
		} else {
			out.Completeness = review.CompletenessOff
		}
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
// CLI actions, storing it as the App's current bundle (see Bundle). When GitLab
// is not yet configured the bundle uses a stub client so the web UI still
// starts; the setup gate then walks the user through configuration.
func (a *App) Services() (*service.Bundle, error) {
	b, err := a.buildBundle(a.Config())
	if err != nil {
		return nil, err
	}
	a.bundle.Store(b)
	return b, nil
}

// buildBundle wires a service bundle from an explicit config snapshot without
// storing it — the hot-apply path builds first and swaps only on success.
func (a *App) buildBundle(cfg *config.Config) (*service.Bundle, error) {
	db, err := a.OpenState()
	if err != nil {
		return nil, err
	}
	var gl gitlab.API
	if cfg.GitLab.Host == "" {
		gl = gitlab.Unconfigured()
	} else {
		c, err := gitlabClientFor(cfg)
		if err != nil {
			return nil, err
		}
		gl = c
	}
	eng := review.NewEngine(a.llmClientFor(cfg), a.Log)
	rc := service.ReviewConfig{
		Host:             cfg.GitLab.Host,
		ReviewerUsername: cfg.GitLab.Username,
		Model:            cfg.LLM.Model,
		LLMProvider:      cfg.LLM.Provider,
		AgentMode:        cfg.Review.AgentMode && cfg.LLM.Claude.AgentMode,
		AllowedTools:     cfg.LLM.Claude.AllowedTools,
		SkillTools:       cfg.LLM.Claude.SkillTools,
		Profile:          profileFromConfig(cfg.Review),
		Token:            cfg.GitLabToken(),
		CacheDir:         cfg.Storage.CacheDir,
		IgnoreGlobs:      cfg.Review.IgnoreGlobs,
		Context:          contextBudgetFromConfig(cfg.Review),
		Pipeline:         pipelineFromConfig(cfg.Review),
		Risk:             riskSettingsFromConfig(cfg.Review),
		Coverage:         coverageSettingsFromConfig(cfg.Review),
	}
	return service.NewBundle(gl, db, eng, rc, a.Log), nil
}
