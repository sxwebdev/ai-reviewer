package app

import (
	"github.com/sxwebdev/ai-reviewer/internal/gitlab"
	"github.com/sxwebdev/ai-reviewer/internal/review"
	"github.com/sxwebdev/ai-reviewer/internal/service"
)

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
		Profile:          review.DefaultProfile(),
		Token:            a.Cfg.GitLabToken(),
		CacheDir:         a.Cfg.Storage.CacheDir,
		IgnoreGlobs:      a.Cfg.Review.IgnoreGlobs,
	}
	return service.NewBundle(gl, db, eng, rc, a.Log), nil
}
