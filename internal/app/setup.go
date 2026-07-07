package app

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sxwebdev/ai-reviewer/internal/config"
	"github.com/sxwebdev/ai-reviewer/internal/gitlab"
	"github.com/sxwebdev/ai-reviewer/internal/security"
	"github.com/sxwebdev/ai-reviewer/internal/server"
)

// claudePath resolves the configured claude binary. Successful lookups are
// cached per bin name (the setup gate runs on every request and a found binary
// does not move); misses always re-probe so installing claude while the server
// runs is picked up by the setup Re-check flow.
func (a *App) claudePath() (string, bool) {
	bin := a.Config().LLM.Claude.Bin
	if c := a.claudeProbe.Load(); c != nil && c.bin == bin {
		return c.path, true
	}
	p, err := exec.LookPath(bin)
	if err != nil {
		return "", false
	}
	a.claudeProbe.Store(&claudeLookup{bin: bin, path: p})
	return p, true
}

// NeedsSetup reports whether the web UI must show the setup screen instead of
// the interface: GitLab host + a resolvable token (config or token_env) and
// the claude CLI on PATH are required. Presence only — token validity is
// enforced on the setup form submit and by doctor.
func (a *App) NeedsSetup() bool {
	if !a.Config().GitLabConfigured() {
		return true
	}
	_, found := a.claudePath()
	return !found
}

// SetupStatus returns setup-page prefills and environment checks.
func (a *App) SetupStatus() server.SetupStatus {
	cfg := a.Config()
	st := server.SetupStatus{
		Host:         cfg.GitLab.Host,
		Username:     cfg.GitLab.Username,
		TokenFromEnv: cfg.GitLab.Token == "" && cfg.GitLabToken() != "",
		TokenEnvName: cfg.GitLab.TokenEnv,
	}
	if p, found := a.claudePath(); found {
		st.ClaudeFound = true
		st.ClaudeDetail = p
	} else {
		st.ClaudeDetail = fmt.Sprintf("%q not found in PATH", cfg.LLM.Claude.Bin)
	}
	return st
}

// ValidateGitLab builds a real client from the submitted values (reusing
// TLS/timeout options from the current config) and pings CurrentUser. An empty
// token falls back to the currently resolvable one (env workflow). The token
// is registered with the log redactor only after it validates, so failed
// attempts don't grow the redactor's literal list. Returns the authenticated
// username.
func (a *App) ValidateGitLab(ctx context.Context, host, token string) (string, error) {
	cfg := a.Config()
	if token == "" {
		token = cfg.GitLabToken()
	}
	gl, err := gitlab.New(gitlabConfigFor(cfg, host, token))
	if err != nil {
		return "", err
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	user, err := gl.CurrentUser(pingCtx)
	if err != nil {
		return "", err
	}
	security.RegisterSecret(token)
	return user.Username, nil
}

// ApplySetup persists the GitLab settings to the config file and hot-applies
// them (reload + service bundle rebuild). An empty token means the env-provided
// one stays in use and nothing secret is written to disk.
func (a *App) ApplySetup(_ context.Context, host, username, token string) error {
	a.applyMu.Lock()
	defer a.applyMu.Unlock()
	values := map[string]string{
		"gitlab.host":     host,
		"gitlab.username": username,
	}
	if token != "" {
		security.RegisterSecret(token)
		values["gitlab.token"] = token
	}
	if err := config.PatchFile(a.ConfigPath, values); err != nil {
		return err
	}
	return a.reloadAndRebuild(values)
}

// errBadSetting marks user-facing validation failures for header switches.
var errBadSetting = errors.New("invalid setting")

// rawConfigKeys are the whitelisted header-switch keys whose config fields are
// non-string scalars (bool/number) and must be written unquoted. The single
// source of truth for the raw-vs-quoted decision in ApplySettings.
var rawConfigKeys = map[string]bool{"review.agent_mode": true}

// ApplySettings persists whitelisted config keys (the header switches) and
// hot-applies them.
func (a *App) ApplySettings(_ context.Context, values map[string]string) error {
	a.applyMu.Lock()
	defer a.applyMu.Unlock()
	cfg := a.Config()
	for key, v := range values {
		switch key {
		case "review.pipeline.mode":
			if !slices.Contains(pipelineModes(cfg), v) {
				return fmt.Errorf("%w: pipeline mode %q", errBadSetting, v)
			}
		case "llm.model":
			if !slices.Contains(modelIDs(cfg), v) {
				return fmt.Errorf("%w: model %q is not one of the offered choices", errBadSetting, v)
			}
		case "review.agent_mode":
			if v != "true" && v != "false" {
				return fmt.Errorf("%w: agent_mode %q must be true or false", errBadSetting, v)
			}
		default:
			return fmt.Errorf("%w: unsupported key %q", errBadSetting, key)
		}
	}
	// rawConfigKeys are written as bare YAML scalars in one atomic pass (a quoted
	// "true" fails to unmarshal into a Go bool); everything else is quoted.
	if err := config.PatchFileMixed(a.ConfigPath, values, rawConfigKeys); err != nil {
		return err
	}
	return a.reloadAndRebuild(values)
}

// reloadAndRebuild re-reads the config file (env overrides keep winning, same
// as at boot), rebuilds the service bundle from the new config, and only then
// swaps both — bundle first, config second — so a request can never observe an
// open setup gate together with the stale unconfigured bundle, and a rebuild
// failure leaves the running state untouched. CLI flag overrides are
// re-applied so a reload doesn't drop them. If an AI_REVIEWER_* env variable
// shadows a just-written value, the apply is reported as an error so the user
// is not shown a success for a setting that did not take effect. Callers hold
// applyMu.
func (a *App) reloadAndRebuild(requested map[string]string) error {
	oldCfg := a.Config()
	cfg, err := config.Load(a.ConfigPath)
	if err != nil {
		return err
	}
	if a.overrides != nil {
		a.overrides(cfg)
	}
	if tok := cfg.GitLabToken(); tok != "" {
		security.RegisterSecret(tok)
	}
	b, err := a.buildBundle(cfg)
	if err != nil {
		return err
	}
	a.bundle.Store(b)
	a.storeConfig(cfg)

	if oldCfg.Watch.MaxParallel != cfg.Watch.MaxParallel {
		a.Log.Warn("watch.max_parallel changed — the running worker pool keeps its size until restart",
			"old", oldCfg.Watch.MaxParallel, "new", cfg.Watch.MaxParallel)
	}
	if shadowed := envShadowedKeys(cfg, requested); len(shadowed) > 0 {
		return fmt.Errorf("saved to %s, but overridden by environment: %s — unset the variable(s) for the change to take effect",
			a.ConfigPath, strings.Join(shadowed, ", "))
	}
	return nil
}

// envShadowedKeys returns, for each requested key whose effective value ended
// up different after the reload, a "key (ENV_VAR)" description. Values are
// deliberately not included: gitlab.token may be a secret.
func envShadowedKeys(cfg *config.Config, requested map[string]string) []string {
	effective := map[string]string{
		"gitlab.host":          cfg.GitLab.Host,
		"gitlab.username":      cfg.GitLab.Username,
		"gitlab.token":         cfg.GitLab.Token,
		"review.pipeline.mode": cfg.Review.Pipeline.Mode,
		"llm.model":            cfg.LLM.Model,
		"review.agent_mode":    strconv.FormatBool(cfg.Review.AgentMode),
	}
	var shadowed []string
	for _, key := range slices.Sorted(maps.Keys(requested)) {
		want := requested[key]
		got, known := effective[key]
		if known && got != want {
			envName := "AI_REVIEWER_" + strings.ToUpper(strings.ReplaceAll(key, ".", "_"))
			shadowed = append(shadowed, fmt.Sprintf("%s (%s)", key, envName))
		}
	}
	return shadowed
}

// requireGitLab guards headless entrypoints (daemon/sync/review) that cannot
// walk the user through setup.
func (a *App) requireGitLab() error {
	if !a.Config().GitLabConfigured() {
		return errors.New("GitLab is not configured — run 'ai-reviewer serve' and complete setup in the browser")
	}
	return nil
}

// pipelineModes lists the modes offered by the header switch. "custom" is
// offered only when the config actually defines custom passes.
func pipelineModes(cfg *config.Config) []string {
	modes := []string{"cheap", "standard", "deep"}
	if cfg.Review.Pipeline.Mode == "custom" || len(cfg.Review.Pipeline.Passes) > 0 {
		modes = append(modes, "custom")
	}
	return modes
}

// stockModels are the version-pinned models offered by the header switch. IDs
// are passed verbatim to the claude CLI (--model); labels are display-only.
var stockModels = []server.ModelChoice{
	{ID: "claude-opus-4-8", Label: "Opus 4.8"},
	{ID: "claude-sonnet-5", Label: "Sonnet 5"},
	{ID: "claude-haiku-4-5-20251001", Label: "Haiku 4.5"},
	{ID: "claude-fable-5", Label: "Fable 5"},
}

// modelChoices lists the models offered by the header switch, keeping a
// hand-configured model selectable even when it is not a stock pinned ID.
func modelChoices(cfg *config.Config) []server.ModelChoice {
	if cfg.LLM.Model != "" && !slices.ContainsFunc(stockModels, func(m server.ModelChoice) bool { return m.ID == cfg.LLM.Model }) {
		return append([]server.ModelChoice{{ID: cfg.LLM.Model, Label: cfg.LLM.Model}}, stockModels...)
	}
	return stockModels
}

// modelIDs returns just the selectable model IDs (for validation).
func modelIDs(cfg *config.Config) []string {
	choices := modelChoices(cfg)
	ids := make([]string, len(choices))
	for i, m := range choices {
		ids[i] = m.ID
	}
	return ids
}

// uiConfigFrom derives the server's display config from a config snapshot. It
// is recomputed once per config store (see App.storeConfig), not per request.
func uiConfigFrom(cfg *config.Config) server.UIConfig {
	mode := cfg.Review.Pipeline.Mode
	if mode == "" {
		mode = "standard"
	}
	return server.UIConfig{
		Host:               cfg.GitLab.Host,
		LLMModel:           cfg.LLM.Model,
		CommentLanguage:    cfg.Review.PreferredCommentLanguage,
		SeverityThreshold:  cfg.Review.SeverityThreshold,
		MaxComments:        cfg.Review.MaxComments,
		AgentMode:          cfg.Review.AgentMode,
		AgentModeEffective: cfg.Review.AgentMode && cfg.LLM.Claude.AgentMode,
		SubscriptionAuth:   cfg.LLM.Claude.AuthMode != "api-key",
		PipelineMode:       mode,
		PipelineModes:      pipelineModes(cfg),
		Models:             modelChoices(cfg),
	}
}

// uiConfig returns the cached display-config snapshot.
func (a *App) uiConfig() server.UIConfig { return *a.uiSnap.Load() }
