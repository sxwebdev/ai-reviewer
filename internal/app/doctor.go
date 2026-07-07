package app

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/sxwebdev/ai-reviewer/internal/config"
)

// CheckStatus is the outcome of a single doctor check.
type CheckStatus string

const (
	StatusOK   CheckStatus = "ok"
	StatusWarn CheckStatus = "warn"
	StatusFail CheckStatus = "fail"
)

// DoctorCheck is a single environment/health check result.
type DoctorCheck struct {
	Name   string
	Status CheckStatus
	Detail string
}

// Doctor runs environment and configuration checks and returns their results.
// It never fails the process; the CLI decides the exit code from the results.
func (a *App) Doctor(ctx context.Context) []DoctorCheck {
	cfg := a.Config()
	var checks []DoctorCheck
	add := func(name string, status CheckStatus, detail string) {
		checks = append(checks, DoctorCheck{Name: name, Status: status, Detail: detail})
	}

	// git
	if p, err := exec.LookPath("git"); err == nil {
		add("git", StatusOK, p)
	} else {
		add("git", StatusFail, "git not found in PATH")
	}

	// claude CLI
	if p, err := exec.LookPath(cfg.LLM.Claude.Bin); err == nil {
		add("claude CLI", StatusOK, p)
	} else {
		add("claude CLI", StatusFail, fmt.Sprintf("%q not found in PATH", cfg.LLM.Claude.Bin))
	}

	// Claude auth hint (do not print token values)
	switch cfg.LLM.Claude.AuthMode {
	case "oauth-token":
		if os.Getenv(cfg.LLM.Claude.OAuthTokenEnv) == "" {
			add("claude auth", StatusWarn, fmt.Sprintf("%s not set", cfg.LLM.Claude.OAuthTokenEnv))
		} else {
			add("claude auth", StatusOK, "oauth token present")
		}
	case "api-key":
		if os.Getenv(cfg.LLM.Claude.APIKeyEnv) == "" {
			add("claude auth", StatusWarn, fmt.Sprintf("%s not set", cfg.LLM.Claude.APIKeyEnv))
		} else {
			add("claude auth", StatusOK, "api key present")
		}
	default:
		add("claude auth", StatusOK, "using existing Claude Code login")
	}

	// GitLab config
	if cfg.GitLab.Host == "" {
		add("gitlab host", StatusFail, "gitlab.host is empty")
	} else {
		add("gitlab host", StatusOK, cfg.GitLab.Host)
	}
	if cfg.GitLab.Username == "" {
		add("gitlab username", StatusWarn, "gitlab.username is empty")
	} else {
		add("gitlab username", StatusOK, cfg.GitLab.Username)
	}
	if cfg.GitLabToken() == "" {
		if te := cfg.GitLab.TokenEnv; te != "" && !config.IsValidEnvName(te) {
			// token_env holds something that is not a valid env var name — almost
			// certainly the token itself was pasted here. Do not echo the value.
			add("gitlab token", StatusFail, "gitlab.token is empty and gitlab.token_env is not a valid env var name — did you paste the token into token_env? Put it in gitlab.token instead")
		} else {
			add("gitlab token", StatusFail, "gitlab.token is not set (complete setup in the web UI, or export the token_env variable)")
		}
	} else {
		add("gitlab token", StatusOK, "present") // never echo the token value
	}

	// GitLab API reachability (only if host + token are present)
	if cfg.GitLab.Host != "" && cfg.GitLabToken() != "" {
		if gl, err := a.GitLabClient(); err != nil {
			add("gitlab api", StatusFail, err.Error())
		} else {
			pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			user, err := gl.CurrentUser(pingCtx)
			cancel()
			switch {
			case err != nil:
				add("gitlab api", StatusFail, err.Error())
			case cfg.GitLab.Username != "" && user.Username != cfg.GitLab.Username:
				add("gitlab api", StatusWarn, fmt.Sprintf("token user %q != config username %q", user.Username, cfg.GitLab.Username))
			default:
				add("gitlab api", StatusOK, "authenticated as "+user.Username)
			}
		}
	}

	// directories
	dataStatus, dataDetail := dirStatus(cfg.App.DataDir)
	add("data dir", dataStatus, dataDetail)
	cacheStatus, cacheDetail := dirStatus(cfg.Storage.CacheDir)
	add("cache dir", cacheStatus, cacheDetail)

	// database + migrations + FTS5
	if db, err := a.OpenState(); err != nil {
		add("database", StatusFail, err.Error())
	} else {
		add("database", StatusOK, cfg.Storage.DBPath)
		if err := db.FTS5Available(); err != nil {
			add("sqlite fts5", StatusFail, err.Error())
		} else {
			add("sqlite fts5", StatusOK, "available")
		}
	}

	// Go toolchain (optional, useful for Go-project analysis)
	if p, err := exec.LookPath("go"); err == nil {
		add("go toolchain", StatusOK, p)
	} else {
		add("go toolchain", StatusWarn, "go not found (Go-specific analysis disabled)")
	}

	return checks
}

func dirStatus(dir string) (CheckStatus, string) {
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return StatusWarn, dir + " (missing — created automatically on first run)"
		}
		return StatusFail, err.Error()
	}
	if !info.IsDir() {
		return StatusFail, dir + " is not a directory"
	}
	return StatusOK, dir
}
