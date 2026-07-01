package app

import (
	"github.com/sxwebdev/ai-reviewer/internal/gitlab"
	"github.com/sxwebdev/ai-reviewer/internal/llm"
)

// GitLabClient builds a GitLab API client from the app config. It fails if the
// host is not configured, guiding the user to run init/edit config.
func (a *App) GitLabClient() (*gitlab.Client, error) {
	return gitlab.New(gitlab.Config{
		Host:               a.Cfg.GitLab.Host,
		Token:              a.Cfg.GitLabToken(),
		Timeout:            a.Cfg.GitLab.Timeout,
		InsecureSkipVerify: a.Cfg.GitLab.InsecureSkipVerify,
		CACertPath:         a.Cfg.GitLab.CACertPath,
	})
}

// LLMClient builds the configured LLM client. Currently the Claude CLI provider.
func (a *App) LLMClient() llm.Client {
	return llm.NewClaudeCLI(llm.ClaudeOptions{
		Bin:            a.Cfg.LLM.Claude.Bin,
		Model:          a.Cfg.LLM.Model,
		PermissionMode: a.Cfg.LLM.Claude.PermissionMode,
		Timeout:        a.Cfg.LLM.Timeout,
		ExtraArgs:      a.Cfg.LLM.Claude.ExtraArgs,
	}, a.Log)
}
