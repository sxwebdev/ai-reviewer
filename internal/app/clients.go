package app

import (
	"github.com/sxwebdev/ai-reviewer/internal/config"
	"github.com/sxwebdev/ai-reviewer/internal/gitlab"
	"github.com/sxwebdev/ai-reviewer/internal/llm"
)

// gitlabConfigFor assembles the GitLab client config from cfg, with host and
// token supplied by the caller (live values from config, or setup-form values
// under validation). Keeping this in one place means a new transport option
// (TLS, timeout, ...) is wired for both the live client and setup validation.
func gitlabConfigFor(cfg *config.Config, host, token string) gitlab.Config {
	return gitlab.Config{
		Host:               host,
		Token:              token,
		Timeout:            cfg.GitLab.Timeout,
		InsecureSkipVerify: cfg.GitLab.InsecureSkipVerify,
		CACertPath:         cfg.GitLab.CACertPath,
	}
}

// GitLabClient builds a GitLab API client from the app config. It fails if the
// host is not configured, guiding the user to complete setup in the web UI.
func (a *App) GitLabClient() (*gitlab.Client, error) {
	return gitlabClientFor(a.Config())
}

func gitlabClientFor(cfg *config.Config) (*gitlab.Client, error) {
	return gitlab.New(gitlabConfigFor(cfg, cfg.GitLab.Host, cfg.GitLabToken()))
}

// LLMClient builds the configured LLM client. Currently the Claude CLI provider.
func (a *App) LLMClient() llm.Client {
	return a.llmClientFor(a.Config())
}

func (a *App) llmClientFor(cfg *config.Config) llm.Client {
	return llm.NewClaudeCLI(llm.ClaudeOptions{
		Bin:            cfg.LLM.Claude.Bin,
		Model:          cfg.LLM.Model,
		PermissionMode: cfg.LLM.Claude.PermissionMode,
		Timeout:        cfg.LLM.Timeout,
		ExtraArgs:      cfg.LLM.Claude.ExtraArgs,
	}, a.Log)
}
