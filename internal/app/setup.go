package app

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
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
// token falls back to the currently resolvable one (env workflow). Returns the
// authenticated username. Used by the setup page, whose form carries no TLS
// fields, so the current config's TLS options are correct.
func (a *App) ValidateGitLab(ctx context.Context, host, token string) (string, error) {
	return a.validateGitLabWith(ctx, a.Config(), host, token)
}

// validateGitLabWith pings CurrentUser using the TLS/timeout options from the
// given config snapshot — the Settings form can change insecure_skip_verify /
// ca_cert_path in the same save, so the ping must use the pending values, not
// the live ones. The token is registered with the log redactor only after it
// validates, so failed attempts don't grow the redactor's literal list.
func (a *App) validateGitLabWith(ctx context.Context, cfg *config.Config, host, token string) (string, error) {
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

// errBadSetting marks user-facing validation failures for config settings.
var errBadSetting = errors.New("invalid setting")

// errEnvShadowed marks a saved-but-shadowed apply: the file was written, but an
// AI_REVIEWER_* env var overrides the value on reload. It is a warning, not a
// failure — the write is kept and the message surfaced to the user.
var errEnvShadowed = errors.New("overridden by environment")

// ApplyConfig validates, persists, and hot-applies any schema-known config keys
// (both the header switches and the full Settings form). Only fields whose value
// actually changed are written — a section form resubmits all of its fields, so
// diffing keeps the patch minimal, preserves the inline comments on untouched
// keys, and avoids a needless GitLab ping when no GitLab credential changed.
// Every value is validated before the file is touched, so the subsequent
// reload's Validate() cannot fail on our own write. If the reload fails for any
// reason other than env shadowing, the previous config file is restored
// (rollback) so a bad apply never leaves broken YAML on disk. A GitLab host or
// token change is pinged against the live API first, using the TLS options being
// saved in the same request.
func (a *App) ApplyConfig(ctx context.Context, values map[string]string) (server.ApplyResult, error) {
	a.applyMu.Lock()
	defer a.applyMu.Unlock()

	cfg := a.Config()
	schema := config.SettingsSchemaByKey()

	patch := map[string]string{}
	raw := map[string]bool{}
	restart := false
	hostChanged, tokenChanged := false, false
	newHost, newToken := cfg.GitLab.Host, "" // token "" = keep the current one

	for key, rawVal := range values {
		f, ok := schema[key]
		if !ok {
			return server.ApplyResult{}, fmt.Errorf("%w: unsupported key %q", errBadSetting, key)
		}
		v := strings.TrimSpace(rawVal)
		if f.Secret && v == "" {
			continue // never blank a secret — empty means "keep current"
		}
		if err := a.validateSetting(cfg, f, v); err != nil {
			return server.ApplyResult{}, err
		}
		// The value written to the file: a flow sequence for lists, the raw
		// string otherwise. Skip fields whose effective value is unchanged so a
		// section save only rewrites what the user actually edited.
		writeVal := v
		if f.Kind == config.KindList {
			writeVal = config.FormatYAMLList(parseListLines(v))
		}
		if settingMatches(f, writeVal, f.Get(cfg)) {
			continue
		}
		switch key {
		case "gitlab.host":
			newHost, hostChanged = v, true
		case "gitlab.token":
			newToken, tokenChanged = v, true
		}
		patch[key] = writeVal
		if f.Kind.Raw() {
			raw[key] = true
		}
		if f.Restart {
			restart = true
		}
	}
	if len(patch) == 0 {
		return server.ApplyResult{}, nil
	}

	// Reject mode=custom with no passes: it would silently degrade to a single
	// general pass. Consider the passes being saved in the same request.
	if err := a.validatePipelineMode(cfg, patch); err != nil {
		return server.ApplyResult{}, err
	}

	// Ping GitLab only when a credential actually changed and a host remains
	// (clearing the host un-configures GitLab — nothing to ping). Use a config
	// snapshot carrying the TLS options being saved in this same request.
	if (hostChanged || tokenChanged) && newHost != "" {
		if _, err := a.validateGitLabWith(ctx, candidateGitLabCfg(cfg, patch), newHost, newToken); err != nil {
			return server.ApplyResult{}, err
		}
		if newToken != "" {
			security.RegisterSecret(newToken)
		}
	}

	// Snapshot for rollback. Distinguish "file absent" from a read failure via
	// IsNotExist — a swallowed read error must never let rollback delete an
	// existing file.
	prev, readErr := os.ReadFile(a.ConfigPath)
	created := os.IsNotExist(readErr)
	if readErr != nil && !created {
		return server.ApplyResult{}, fmt.Errorf("read config %s: %w", a.ConfigPath, readErr)
	}
	if err := config.PatchFileMixed(a.ConfigPath, patch, raw); err != nil {
		return server.ApplyResult{}, err
	}
	if err := a.reloadAndRebuild(patch); err != nil {
		if errors.Is(err, errEnvShadowed) {
			// The write is valid and kept; the env var just wins at runtime.
			return server.ApplyResult{RestartRequired: restart, Warning: err.Error()}, nil
		}
		a.rollbackConfig(prev, created)
		return server.ApplyResult{}, err
	}
	return server.ApplyResult{RestartRequired: restart}, nil
}

// validatePipelineMode rejects an effective pipeline mode of "custom" with no
// passes, considering both the current config and the passes in this patch.
func (a *App) validatePipelineMode(cfg *config.Config, patch map[string]string) error {
	mode := cfg.Review.Pipeline.Mode
	if v, ok := patch["review.pipeline.mode"]; ok {
		mode = v
	}
	if mode != "custom" {
		return nil
	}
	passes := cfg.Review.Pipeline.Passes
	if v, ok := patch["review.pipeline.passes"]; ok {
		passes = parseYAMLListPatch(v)
	}
	if len(passes) == 0 {
		return fmt.Errorf("%w: pipeline mode \"custom\" requires at least one pass in \"Custom passes\"", errBadSetting)
	}
	return nil
}

// candidateGitLabCfg returns a shallow copy of cfg with the GitLab TLS/timeout
// fields from patch applied, so a validation ping reflects the values being
// saved in the same request rather than the live ones.
func candidateGitLabCfg(cfg *config.Config, patch map[string]string) *config.Config {
	cand := *cfg
	if v, ok := patch["gitlab.insecure_skip_verify"]; ok {
		cand.GitLab.InsecureSkipVerify = v == "true"
	}
	if v, ok := patch["gitlab.ca_cert_path"]; ok {
		cand.GitLab.CACertPath = v
	}
	if v, ok := patch["gitlab.timeout"]; ok {
		if d, err := time.ParseDuration(v); err == nil {
			cand.GitLab.Timeout = d
		}
	}
	return &cand
}

// parseYAMLListPatch reads back the items from a FormatYAMLList flow sequence
// (e.g. `["a", "b"]`) written into a patch, for cross-field validation.
func parseYAMLListPatch(flow string) []string {
	s := strings.TrimSpace(flow)
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if unq, err := strconv.Unquote(part); err == nil {
			part = unq
		}
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// validateSetting rejects a value that would not satisfy the field's type or
// the config's own Validate() rules, before anything is written.
func (a *App) validateSetting(cfg *config.Config, f config.SettingField, v string) error {
	switch f.Kind {
	case config.KindSelect:
		opts := f.Options
		if f.Key == "llm.model" {
			opts = modelIDs(cfg)
		}
		if !slices.Contains(opts, v) {
			return fmt.Errorf("%w: %s %q is not one of the offered choices", errBadSetting, f.Label, v)
		}
	case config.KindBool:
		if v != "true" && v != "false" {
			return fmt.Errorf("%w: %s must be true or false", errBadSetting, f.Label)
		}
	case config.KindInt:
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("%w: %s must be a whole number", errBadSetting, f.Label)
		}
		if f.Min != nil && n < *f.Min {
			return fmt.Errorf("%w: %s must be >= %d", errBadSetting, f.Label, *f.Min)
		}
		if f.Max != nil && n > *f.Max {
			return fmt.Errorf("%w: %s must be <= %d", errBadSetting, f.Label, *f.Max)
		}
	case config.KindDuration:
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("%w: %s must be a duration like 30s or 15m", errBadSetting, f.Label)
		}
		if d < 0 {
			return fmt.Errorf("%w: %s must not be negative", errBadSetting, f.Label)
		}
	case config.KindText, config.KindPassword:
		if f.Required && v == "" {
			return fmt.Errorf("%w: %s must not be empty", errBadSetting, f.Label)
		}
	case config.KindList:
		// Any set of non-empty lines is acceptable.
	}
	return nil
}

// parseListLines turns a textarea value (one item per line) into a trimmed,
// empty-dropped slice for FormatYAMLList.
func parseListLines(v string) []string {
	var out []string
	for _, line := range strings.Split(v, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// rollbackConfig restores the config file after a failed apply and reloads so
// the running config matches disk again. created reports whether this apply
// created the file (it did not exist before) — only then is removal correct;
// otherwise prev holds the prior bytes to restore. Best-effort: a rollback
// failure is logged, not surfaced (the original apply error is what the user
// needs to see).
func (a *App) rollbackConfig(prev []byte, created bool) {
	if created {
		_ = os.Remove(a.ConfigPath)
	} else if err := os.WriteFile(a.ConfigPath, prev, 0o600); err != nil {
		a.Log.Error("config rollback failed", "err", err)
		return
	}
	if cfg, err := config.Load(a.ConfigPath); err == nil {
		if a.overrides != nil {
			a.overrides(cfg)
		}
		if b, berr := a.buildBundle(cfg); berr == nil {
			a.bundle.Store(b)
			a.storeConfig(cfg)
		}
	}
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
		return fmt.Errorf("%w: saved to %s, but %s — unset the variable(s) for the change to take effect",
			errEnvShadowed, a.ConfigPath, strings.Join(shadowed, ", "))
	}
	return nil
}

// envShadowedKeys returns, for each requested key whose effective value after
// the reload differs from what was written, a "key (ENV_VAR)" description —
// i.e. an AI_REVIEWER_* variable is overriding the file. It compares by field
// type (durations/ints/bools/lists semantically, not by raw string) so a
// normalized value like "60s" → "1m0s" is not mistaken for a shadow. Values are
// deliberately omitted: gitlab.token may be a secret. The env var names come
// from the schema, which derives them exactly (see config.SettingsSchema).
func envShadowedKeys(cfg *config.Config, requested map[string]string) []string {
	schema := config.SettingsSchemaByKey()
	var shadowed []string
	for _, key := range slices.Sorted(maps.Keys(requested)) {
		f, ok := schema[key]
		if !ok {
			continue
		}
		if !settingMatches(f, requested[key], f.Get(cfg)) {
			shadowed = append(shadowed, fmt.Sprintf("%s (%s)", key, f.EnvName))
		}
	}
	return shadowed
}

// settingMatches reports whether the effective (post-reload) value equals what
// was requested, comparing semantically per field kind. `want` is the value as
// written (for lists, a FormatYAMLList flow sequence); `got` is f.Get(cfg).
func settingMatches(f config.SettingField, want, got string) bool {
	switch f.Kind {
	case config.KindInt:
		wn, werr := strconv.Atoi(want)
		gn, gerr := strconv.Atoi(got)
		return werr == nil && gerr == nil && wn == gn
	case config.KindDuration:
		wd, werr := time.ParseDuration(want)
		gd, gerr := time.ParseDuration(got)
		return werr == nil && gerr == nil && wd == gd
	case config.KindList:
		return want == config.FormatYAMLList(parseListLines(got))
	default:
		return want == got
	}
}

// settingsViewFrom builds the Settings-page form model from the schema and a
// config snapshot: each field's current value, resolved select options, and
// whether an env var shadows it.
func settingsViewFrom(cfg *config.Config) server.SettingsView {
	var view server.SettingsView
	index := map[string]int{} // section name → position in view.Sections
	for _, f := range config.SettingsSchema() {
		fv := server.SettingsFieldView{
			Key:         f.Key,
			Label:       f.Label,
			Help:        f.Help,
			Kind:        f.Kind.String(),
			Options:     f.Options,
			Danger:      f.Danger,
			Restart:     f.Restart,
			Secret:      f.Secret,
			EnvName:     f.EnvName,
			EnvShadowed: f.EnvName != "" && os.Getenv(f.EnvName) != "",
		}
		if !f.Secret {
			fv.Value = f.Get(cfg) // secrets are never echoed back
		}
		if f.Key == "llm.model" {
			fv.Options = modelIDs(cfg) // dynamic: stock IDs + any configured custom model
		}
		i, ok := index[f.Section]
		if !ok {
			index[f.Section] = len(view.Sections)
			view.Sections = append(view.Sections, server.SettingsSection{Name: f.Section})
			i = index[f.Section]
		}
		view.Sections[i].Fields = append(view.Sections[i].Fields, fv)
		if f.Danger {
			view.Sections[i].HasDanger = true
		}
	}
	return view
}

// SettingsView returns the current editable-config form model.
func (a *App) SettingsView() server.SettingsView { return settingsViewFrom(a.Config()) }

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
