package llm

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/sxwebdev/ai-reviewer/internal/security"
)

// AuthMode selects how the claude subprocess authenticates.
type AuthMode string

const (
	// AuthExistingLogin uses whatever login the machine already has (keychain
	// or `claude /login` state). Nothing is injected, and no auth variable is
	// inherited, so the login cannot be silently overridden.
	AuthExistingLogin AuthMode = "existing-login"
	// AuthOAuthToken injects a subscription token from `claude setup-token`.
	AuthOAuthToken AuthMode = "oauth-token"
	// AuthAPIKey injects a metered Anthropic API key.
	AuthAPIKey AuthMode = "api-key"
)

// AuthModes is the closed set, for config validation and error messages.
var AuthModes = []AuthMode{AuthExistingLogin, AuthOAuthToken, AuthAPIKey}

// Valid reports whether m is one of the three supported modes.
func (m AuthMode) Valid() bool { return slices.Contains(AuthModes, m) }

// The two variables claude reads for credentials. This package owns both in
// every mode: neither is ever inherited, and only the one the mode selects is
// injected — so a stray value on the node cannot decide which account gets
// billed.
const (
	EnvAPIKey     = "ANTHROPIC_API_KEY"
	EnvOAuthToken = "CLAUDE_CODE_OAUTH_TOKEN"
)

// conflictingEnv redirect claude to a different provider, endpoint or billing
// account. Inherited from a shared node they would quietly change the model and
// the invoice, and the symptom (subtly different reviews) is nearly impossible
// to trace back to an environment variable. They can never be inherited — not
// even via PassthroughEnv — but the operator may still set an explicit value
// through ExtraEnv, which is the documented opt-in (plan §12.2).
var conflictingEnv = []string{
	"CLAUDE_CODE_USE_BEDROCK",
	"CLAUDE_CODE_USE_VERTEX",
	"ANTHROPIC_AUTH_TOKEN",
	"ANTHROPIC_BASE_URL",
}

// claudeEnvNames is what the claude subprocess needs on top of
// security.BaseEnvNames, which owns the general case and the reasoning behind
// the allowlist (in short: the service's own secrets live in its environment,
// and this subprocess reads attacker-authored merge requests).
//
// The switches are deliberately enumerated one by one rather than admitted by a
// CLAUDE_CODE_* prefix, because that prefix is exactly how
// CLAUDE_CODE_USE_BEDROCK and CLAUDE_CODE_OAUTH_TOKEN are spelled. Two are
// load-bearing in the shipped image: DISABLE_AUTOUPDATER (an immutable image
// must not self-update) and USE_BUILTIN_RIPGREP=0 (the bundled ripgrep is
// glibc-linked and cannot run on musl, so dropping it silently breaks the Grep
// tool).
//
// Anything a specific deployment needs beyond this goes in
// llm.claude.passthrough_env (inherit a value) or llm.claude.extra_env (set one).
var claudeEnvNames = []string{
	"CLAUDE_CONFIG_DIR", "DISABLE_AUTOUPDATER", "USE_BUILTIN_RIPGREP",
	"DISABLE_TELEMETRY", "DISABLE_ERROR_REPORTING", "DISABLE_BUG_COMMAND",
	"DISABLE_NON_ESSENTIAL_MODEL_CALLS",
}

// ClaudeAuth builds the environment for one claude subprocess. It is a separate,
// pure component precisely so the mode matrix can be asserted in tests without
// running the CLI.
type ClaudeAuth interface {
	// Env returns the environment for the subprocess, derived from base
	// (normally os.Environ()). base is never mutated.
	Env(base []string) ([]string, error)
	// Describe returns a log-safe summary. It never contains a secret value.
	Describe() string
}

// AuthConfig is the resolved auth section of the config (llm.claude.auth).
// Secrets arrive already unmasked from YAML/env/Vault.
type AuthConfig struct {
	Mode       AuthMode
	OAuthToken string
	APIKey     string
	// ExtraEnv is the operator's explicit opt-in for variables this package
	// otherwise strips, plus any additional variable claude should see. Values
	// here win over the inherited environment.
	ExtraEnv map[string]string
	// PassthroughEnv extends the inheritance allowlist with variables that must
	// keep the *parent's* value (as opposed to ExtraEnv, which sets one). An
	// entry may end in "*" to admit a whole prefix. The credential and provider
	// variables this package governs can never be re-admitted this way.
	PassthroughEnv []string
}

type claudeAuth struct {
	mode AuthMode
	// varName/value are the single credential this mode injects; both are empty
	// for existing-login.
	varName string
	value   string

	extraEnv  map[string]string
	extraKeys []string // sorted, so Env output and Describe are deterministic

	// passNames/passPrefixes are the operator's additions to the inheritance
	// allowlist, split by whether the entry ended in "*".
	passNames    []string
	passPrefixes []string
}

var _ ClaudeAuth = claudeAuth{}

// NewClaudeAuth validates the auth config and registers its secret with the
// process-wide redactor, so the token is masked in logs, subprocess output and
// error text from this point on — before any subprocess can print it.
func NewClaudeAuth(cfg AuthConfig) (ClaudeAuth, error) {
	if !cfg.Mode.Valid() {
		return nil, fmt.Errorf("llm.claude.auth.mode %q is not one of %v", cfg.Mode, AuthModes)
	}

	for k := range cfg.ExtraEnv {
		switch {
		case strings.TrimSpace(k) == "":
			return nil, fmt.Errorf("llm.claude.extra_env has an empty variable name")
		case strings.Contains(k, "="):
			return nil, fmt.Errorf("llm.claude.extra_env name %q contains '='", k)
		case k == EnvAPIKey || k == EnvOAuthToken:
			// Allowing this would let extra_env silently defeat the mode matrix.
			return nil, fmt.Errorf("llm.claude.extra_env must not set %s; it is governed by llm.claude.auth.mode", k)
		}
	}

	a := claudeAuth{
		mode:      cfg.Mode,
		extraEnv:  maps.Clone(cfg.ExtraEnv),
		extraKeys: slices.Sorted(maps.Keys(cfg.ExtraEnv)),
	}

	for _, p := range cfg.PassthroughEnv {
		p = strings.TrimSpace(p)
		name, wildcard := strings.CutSuffix(p, "*")
		switch {
		case name == "" && !wildcard:
			return nil, fmt.Errorf("llm.claude.passthrough_env has an empty variable name")
		case name == "":
			// "*" would hand the subprocess the whole service environment, which
			// is the exact defect the allowlist exists to close.
			return nil, fmt.Errorf("llm.claude.passthrough_env %q would inherit every variable, "+
				"including the service's own secrets; name the variables instead", p)
		case strings.ContainsAny(name, "=*"):
			return nil, fmt.Errorf("llm.claude.passthrough_env %q is not a variable name "+
				"(only a single trailing '*' is a wildcard)", p)
		case name == EnvAPIKey || name == EnvOAuthToken:
			return nil, fmt.Errorf("llm.claude.passthrough_env must not name %s; "+
				"it is governed by llm.claude.auth.mode", name)
		case slices.Contains(conflictingEnv, name):
			return nil, fmt.Errorf("llm.claude.passthrough_env must not name %s; "+
				"set an explicit value through llm.claude.extra_env if you really mean it", name)
		case wildcard && governedByPrefix(name) != "":
			// "ANTHROPIC_*" or "CLAUDE_CODE_*" would re-admit the very variables
			// the mode matrix owns, without the operator necessarily noticing.
			return nil, fmt.Errorf("llm.claude.passthrough_env %q would re-admit %s, which this package governs; "+
				"name the variables you need individually", p, governedByPrefix(name))
		case wildcard:
			a.passPrefixes = append(a.passPrefixes, name)
		default:
			a.passNames = append(a.passNames, name)
		}
	}
	slices.Sort(a.passNames)
	slices.Sort(a.passPrefixes)

	oauth := strings.TrimSpace(cfg.OAuthToken)
	apiKey := strings.TrimSpace(cfg.APIKey)

	switch cfg.Mode {
	case AuthExistingLogin:
		// A configured secret here means the operator believes it is in use
		// while the subprocess actually authenticates some other way — fail
		// loudly rather than run with a surprising identity.
		if oauth != "" || apiKey != "" {
			return nil, fmt.Errorf("llm.claude.auth.mode=%s takes no secret, but oauth_token/api_key is set; "+
				"clear it or switch to %s/%s", AuthExistingLogin, AuthOAuthToken, AuthAPIKey)
		}
	case AuthOAuthToken:
		if oauth == "" {
			return nil, fmt.Errorf("llm.claude.auth.mode=%s requires oauth_token "+
				"(AI_REVIEWER_CLAUDE_CODE_OAUTH_TOKEN or Vault key CLAUDE_CODE_OAUTH_TOKEN); run `claude setup-token` to mint one", AuthOAuthToken)
		}
		a.varName, a.value = EnvOAuthToken, oauth
	case AuthAPIKey:
		if apiKey == "" {
			return nil, fmt.Errorf("llm.claude.auth.mode=%s requires api_key "+
				"(AI_REVIEWER_ANTHROPIC_API_KEY or Vault key ANTHROPIC_API_KEY)", AuthAPIKey)
		}
		a.varName, a.value = EnvAPIKey, apiKey
	}

	if a.value != "" {
		security.RegisterSecret(a.value)
	}
	return a, nil
}

// ExistingLogin is the zero-config auth: use the machine's own login, inherit
// only the allowlisted environment and no credential variable at all. It is the
// safe default when nothing is configured.
func ExistingLogin() ClaudeAuth { return claudeAuth{mode: AuthExistingLogin} }

// Env derives the subprocess environment: inherit only the allowlisted
// variables from base, then add exactly what the mode and the operator dictate.
// Because inheritance is an allowlist, a variable set explicitly here can never
// collide with an inherited one, so the result has no duplicate keys and does
// not depend on how exec resolves them.
func (a claudeAuth) Env(base []string) ([]string, error) {
	if !a.mode.Valid() {
		return nil, fmt.Errorf("claude auth: uninitialised mode %q", a.mode)
	}
	if a.varName != "" && a.value == "" {
		// Defence in depth: NewClaudeAuth rejects this, but a zero-value struct
		// must not silently produce an environment with an empty credential.
		return nil, fmt.Errorf("claude auth: %s is empty in mode %s", a.varName, a.mode)
	}

	// Deny wins over every Allow, so neither the operator's passthrough list nor
	// a future addition to an allowlist can re-admit a variable this package
	// governs. The extra_env keys are denied too: an operator-supplied value
	// replaces the inherited one rather than stacking on top of it, and it is
	// appended below.
	deny := make([]string, 0, 2+len(conflictingEnv)+len(a.extraEnv))
	deny = append(deny, EnvAPIKey, EnvOAuthToken)
	deny = append(deny, conflictingEnv...)
	deny = append(deny, a.extraKeys...)

	out := security.EnvFilter{
		Allow:       slices.Concat(claudeEnvNames, a.passNames),
		AllowPrefix: a.passPrefixes,
		Deny:        deny,
	}.Apply(base)

	if a.varName != "" {
		out = append(out, a.varName+"="+a.value)
	}
	for _, k := range a.extraKeys {
		out = append(out, k+"="+a.extraEnv[k])
	}
	return out, nil
}

// governedByPrefix returns the governed variable a passthrough prefix would
// re-admit, or "" if it would re-admit none.
func governedByPrefix(prefix string) string {
	for _, k := range append([]string{EnvAPIKey, EnvOAuthToken}, conflictingEnv...) {
		if strings.HasPrefix(k, prefix) {
			return k
		}
	}
	return ""
}

// Describe summarises the mode for startup logs and `doctor`. It names
// variables, never values.
func (a claudeAuth) Describe() string {
	var b strings.Builder
	b.WriteString("claude auth: ")
	b.WriteString(string(a.mode))

	switch a.mode {
	case AuthExistingLogin:
		b.WriteString(" (inherited machine login; " + EnvAPIKey + " and " + EnvOAuthToken + " removed)")
	case AuthOAuthToken:
		b.WriteString(" (" + EnvOAuthToken + " injected; " + EnvAPIKey + " removed)")
	case AuthAPIKey:
		b.WriteString(" (" + EnvAPIKey + " injected; " + EnvOAuthToken + " removed)")
	}

	// The environment policy is worth a startup line of its own: "the subprocess
	// did not see $X" is otherwise an invisible cause of a confusing failure.
	fmt.Fprintf(&b, "; env=allowlist(%d names + %d prefixes)",
		len(security.BaseEnvNames)+len(claudeEnvNames), len(security.BaseEnvPrefixes))
	if len(a.passNames) > 0 || len(a.passPrefixes) > 0 {
		passed := slices.Concat(a.passNames, wildcarded(a.passPrefixes))
		b.WriteString("; passthrough_env=[" + strings.Join(passed, ",") + "]")
	}
	if len(a.extraKeys) > 0 {
		b.WriteString("; extra_env=[" + strings.Join(a.extraKeys, ",") + "]")
	}
	return b.String()
}

// wildcarded re-renders prefixes in the form the operator wrote them.
func wildcarded(prefixes []string) []string {
	out := make([]string, len(prefixes))
	for i, p := range prefixes {
		out[i] = p + "*"
	}
	return out
}
