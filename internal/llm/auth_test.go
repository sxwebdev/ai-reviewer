package llm

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/sxwebdev/ai-reviewer/internal/security"
)

// serviceSecretEnv is what the reference deployment's `envFrom: secretRef` puts
// in *this process's* environment. None of it may reach an LLM subprocess that
// reads attacker-authored merge requests. Note AI_REVIEWER_CLAUDE_CODE_OAUTH_TOKEN:
// the prefixed spelling is the one the service actually reads, and it is exactly
// what a strip list of bare names misses.
var serviceSecretEnv = []string{
	"AI_REVIEWER_GITLAB_TOKEN=glpat-serviceaccountpat0001",
	"AI_REVIEWER_SLACK_TOKEN=xoxb-000000-serviceslack",
	"AI_REVIEWER_POSTGRES_PASSWORD=postgres-password-0001",
	"AI_REVIEWER_CLAUDE_CODE_OAUTH_TOKEN=prefixed-oauth-token-0001",
	"VAULT_TOKEN=hvs.vaulttokenvalue0001",
	"AWS_SECRET_ACCESS_KEY=aws-secret-access-key-0001",
}

// inheritedEnv is a hostile-but-realistic parent environment: a shared CI node
// that already carries an API key, an OAuth token and a Bedrock redirect, plus
// the service's own secrets.
func inheritedEnv() []string {
	env := []string{
		"PATH=/usr/bin",
		"HOME=/root",
		"LANG=en_US.UTF-8",
		"LC_TIME=ru_RU.UTF-8",
		"USE_BUILTIN_RIPGREP=0",
		"DISABLE_AUTOUPDATER=1",
		EnvAPIKey + "=sk-ant-inheritedapikeyvalue00",
		EnvOAuthToken + "=inherited-oauth-token-value",
		"CLAUDE_CODE_USE_BEDROCK=1",
		"CLAUDE_CODE_USE_VERTEX=1",
		"ANTHROPIC_AUTH_TOKEN=inherited-auth-token-value",
		"ANTHROPIC_BASE_URL=https://proxy.internal",
	}
	return append(env, serviceSecretEnv...)
}

// envName is the variable name of a "K=V" entry.
func envName(kv string) string {
	k, _, _ := strings.Cut(kv, "=")
	return k
}

// envMap indexes the returned environment and fails on a duplicate key: the
// whole point of stripping before appending is that exec never has to break a
// tie between two assignments of the same variable.
func envMap(t *testing.T, env []string) map[string]string {
	t.Helper()
	out := make(map[string]string, len(env))
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			t.Fatalf("malformed environment entry %q", kv)
		}
		if _, dup := out[k]; dup {
			t.Fatalf("duplicate environment key %q in %v", k, env)
		}
		out[k] = v
	}
	return out
}

func TestClaudeAuthEnvMatrix(t *testing.T) {
	const (
		oauthSecret = "configured-oauth-token-9f2c1ab3"
		apiSecret   = "sk-ant-configuredapikey1234567"
	)

	for _, tc := range []struct {
		name    string
		cfg     AuthConfig
		wantSet map[string]string // variable → exact expected value
		wantOut []string          // variables that must be absent
	}{
		{
			name:    "existing-login injects nothing and strips both credentials",
			cfg:     AuthConfig{Mode: AuthExistingLogin},
			wantSet: nil,
			wantOut: []string{EnvAPIKey, EnvOAuthToken},
		},
		{
			// A parent API key must not win: it would bill a different account
			// than the subscription the operator configured.
			name:    "oauth-token injects the token and removes the inherited API key",
			cfg:     AuthConfig{Mode: AuthOAuthToken, OAuthToken: oauthSecret},
			wantSet: map[string]string{EnvOAuthToken: oauthSecret},
			wantOut: []string{EnvAPIKey},
		},
		{
			name:    "api-key injects the key and removes the inherited OAuth token",
			cfg:     AuthConfig{Mode: AuthAPIKey, APIKey: apiSecret},
			wantSet: map[string]string{EnvAPIKey: apiSecret},
			wantOut: []string{EnvOAuthToken},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auth, err := NewClaudeAuth(tc.cfg)
			if err != nil {
				t.Fatalf("NewClaudeAuth: %v", err)
			}

			base := inheritedEnv()
			got, err := auth.Env(base)
			if err != nil {
				t.Fatalf("Env: %v", err)
			}
			env := envMap(t, got)

			for k, want := range tc.wantSet {
				if env[k] != want {
					t.Errorf("%s = %q, want the configured secret", k, env[k])
				}
			}
			for _, k := range tc.wantOut {
				if v, present := env[k]; present {
					t.Errorf("%s must be absent, got %q", k, v)
				}
			}

			// The conflicting provider variables go in every mode.
			for _, k := range conflictingEnv {
				if v, present := env[k]; present {
					t.Errorf("conflicting variable %s survived as %q", k, v)
				}
			}

			// The service's own secrets never reach the subprocess, in any mode.
			// This is the allowlist's whole reason for existing.
			for _, kv := range serviceSecretEnv {
				if v, present := env[envName(kv)]; present {
					t.Errorf("service secret %s reached the claude subprocess as %q", envName(kv), v)
				}
			}

			// Allowlisted variables are untouched — this is an environment for a
			// subprocess that still needs PATH and HOME, and whose Grep tool breaks
			// on musl without USE_BUILTIN_RIPGREP.
			for k, want := range map[string]string{
				"PATH": "/usr/bin", "HOME": "/root", "LANG": "en_US.UTF-8",
				"LC_TIME": "ru_RU.UTF-8", "USE_BUILTIN_RIPGREP": "0", "DISABLE_AUTOUPDATER": "1",
			} {
				if env[k] != want {
					t.Errorf("allowlisted %s = %q, want %q", k, env[k], want)
				}
			}

			if !slices.Equal(base, inheritedEnv()) {
				t.Errorf("Env mutated its input: %v", base)
			}
		})
	}
}

func TestClaudeAuthExtraEnvOptIn(t *testing.T) {
	auth, err := NewClaudeAuth(AuthConfig{
		Mode:       AuthOAuthToken,
		OAuthToken: "configured-oauth-token-extraenv",
		// The operator deliberately points claude at a gateway; the inherited
		// value must lose to the configured one, not merge with it.
		ExtraEnv: map[string]string{"ANTHROPIC_BASE_URL": "https://gateway.company.ru"},
	})
	if err != nil {
		t.Fatalf("NewClaudeAuth: %v", err)
	}

	env := envMap(t, mustEnv(t, auth, inheritedEnv()))
	if got := env["ANTHROPIC_BASE_URL"]; got != "https://gateway.company.ru" {
		t.Errorf("ANTHROPIC_BASE_URL = %q, want the operator's value", got)
	}
	// Opting one variable in must not opt the others in.
	if _, present := env["CLAUDE_CODE_USE_BEDROCK"]; present {
		t.Error("CLAUDE_CODE_USE_BEDROCK survived even though it was not in extra_env")
	}
}

// TestClaudeAuthPassthroughEnvExtendsTheAllowlist covers the operator's escape
// hatch: a deployment that needs a variable the built-in allowlist does not
// name must be able to say so without reopening the door for everything else.
func TestClaudeAuthPassthroughEnvExtendsTheAllowlist(t *testing.T) {
	auth, err := NewClaudeAuth(AuthConfig{
		Mode:           AuthExistingLogin,
		PassthroughEnv: []string{"CORPORATE_MIRROR", "TEAM_*"},
	})
	if err != nil {
		t.Fatalf("NewClaudeAuth: %v", err)
	}

	base := append(inheritedEnv(),
		"CORPORATE_MIRROR=https://mirror.internal",
		"TEAM_REGION=eu",
		"TEAM_TIER=gold",
		"UNRELATED_VAR=nope",
	)
	env := envMap(t, mustEnv(t, auth, base))

	for k, want := range map[string]string{
		"CORPORATE_MIRROR": "https://mirror.internal",
		"TEAM_REGION":      "eu",
		"TEAM_TIER":        "gold",
	} {
		if env[k] != want {
			t.Errorf("passthrough %s = %q, want %q", k, env[k], want)
		}
	}
	if v, present := env["UNRELATED_VAR"]; present {
		t.Errorf("a variable nobody asked for survived as %q", v)
	}
	// Opting variables in must not opt the secrets in.
	for _, kv := range serviceSecretEnv {
		if _, present := env[envName(kv)]; present {
			t.Errorf("passthrough_env re-admitted the service secret %s", envName(kv))
		}
	}
}

// TestClaudeAuthPassthroughEnvCannotReadmitGovernedVariables pins the part that
// makes the escape hatch safe: passthrough is an addition to the allowlist, not
// an override of the mode matrix.
func TestClaudeAuthPassthroughEnvCannotReadmitGovernedVariables(t *testing.T) {
	for _, tc := range []struct {
		name     string
		pass     []string
		wantText string
	}{
		{"star alone", []string{"*"}, "would inherit every variable"},
		{"the mode's credential", []string{EnvOAuthToken}, "governed by llm.claude.auth.mode"},
		{"the other credential", []string{EnvAPIKey}, "governed by llm.claude.auth.mode"},
		{"a provider redirect", []string{"ANTHROPIC_BASE_URL"}, "extra_env"},
		{"a prefix covering a credential", []string{"ANTHROPIC_*"}, "which this package governs"},
		{"a prefix covering a redirect", []string{"CLAUDE_CODE_*"}, "which this package governs"},
		{"an interior wildcard", []string{"A*B"}, "not a variable name"},
		{"an assignment", []string{"A=B"}, "not a variable name"},
		{"an empty name", []string{"  "}, "empty variable name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewClaudeAuth(AuthConfig{Mode: AuthExistingLogin, PassthroughEnv: tc.pass})
			if err == nil {
				t.Fatalf("passthrough_env %v was accepted", tc.pass)
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Errorf("error %q does not mention %q", err, tc.wantText)
			}
		})
	}
}

// An operator may point an *allowlisted* variable somewhere else — a proxy or a
// timezone the subprocess should see differently from the service. The
// configured value must replace the inherited one rather than stack on top of
// it: envMap fails on a duplicate key, because a duplicate would leave the
// outcome to however exec resolves it.
func TestClaudeAuthExtraEnvReplacesAnAllowlistedVariable(t *testing.T) {
	auth, err := NewClaudeAuth(AuthConfig{
		Mode:     AuthExistingLogin,
		ExtraEnv: map[string]string{"TZ": "UTC", "HTTPS_PROXY": "http://gateway:3128"},
	})
	if err != nil {
		t.Fatalf("NewClaudeAuth: %v", err)
	}

	base := append(inheritedEnv(), "TZ=Europe/Moscow", "HTTPS_PROXY=http://inherited:3128")
	env := envMap(t, mustEnv(t, auth, base))

	for k, want := range map[string]string{"TZ": "UTC", "HTTPS_PROXY": "http://gateway:3128"} {
		if env[k] != want {
			t.Errorf("%s = %q, want the operator's value %q", k, env[k], want)
		}
	}
}

// TestClaudeAuthGovernedVariablesLoseToNothing is the defence-in-depth check on
// the deny list. NewClaudeAuth rejects a passthrough entry naming a governed
// variable, so this constructs the state directly: a future allowlist entry, or
// a bug in that validation, must still not be able to let the mode matrix be
// overridden by the node's environment.
func TestClaudeAuthGovernedVariablesLoseToNothing(t *testing.T) {
	a := claudeAuth{
		mode:      AuthExistingLogin,
		passNames: []string{EnvAPIKey, EnvOAuthToken, "ANTHROPIC_BASE_URL", "CLAUDE_CODE_USE_BEDROCK"},
	}

	env := envMap(t, mustEnv(t, a, inheritedEnv()))
	for _, k := range append([]string{EnvAPIKey, EnvOAuthToken}, conflictingEnv...) {
		if v, present := env[k]; present {
			t.Errorf("%s was re-admitted as %q; the deny list lost to an allow entry", k, v)
		}
	}
}

func TestClaudeAuthRejectsBadConfig(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cfg      AuthConfig
		wantText string
	}{
		{
			name:     "unknown mode",
			cfg:      AuthConfig{Mode: "bearer"},
			wantText: "not one of",
		},
		{
			name:     "oauth-token without a token",
			cfg:      AuthConfig{Mode: AuthOAuthToken},
			wantText: "requires oauth_token",
		},
		{
			// Whitespace-only is the shape an unset Vault key takes after YAML
			// folding; it must fail like an empty one, not inject " ".
			name:     "oauth-token with whitespace only",
			cfg:      AuthConfig{Mode: AuthOAuthToken, OAuthToken: "   "},
			wantText: "requires oauth_token",
		},
		{
			name:     "api-key without a key",
			cfg:      AuthConfig{Mode: AuthAPIKey},
			wantText: "requires api_key",
		},
		{
			name:     "existing-login with a configured secret",
			cfg:      AuthConfig{Mode: AuthExistingLogin, OAuthToken: "configured-but-unused-token"},
			wantText: "takes no secret",
		},
		{
			name:     "extra_env may not override the mode's credential",
			cfg:      AuthConfig{Mode: AuthAPIKey, APIKey: "sk-ant-valid1234567890abcdef", ExtraEnv: map[string]string{EnvOAuthToken: "x"}},
			wantText: "governed by llm.claude.auth.mode",
		},
		{
			name:     "extra_env name containing =",
			cfg:      AuthConfig{Mode: AuthExistingLogin, ExtraEnv: map[string]string{"A=B": "c"}},
			wantText: "contains '='",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewClaudeAuth(tc.cfg)
			if err == nil {
				t.Fatal("expected a configuration error")
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Errorf("error %q does not mention %q", err, tc.wantText)
			}
		})
	}
}

// TestClaudeAuthZeroValueEnvFails covers the defence-in-depth branch: a
// hand-built claudeAuth that skipped NewClaudeAuth must not produce an
// environment with an empty credential.
func TestClaudeAuthZeroValueEnvFails(t *testing.T) {
	if _, err := (claudeAuth{}).Env(nil); err == nil {
		t.Error("zero-value auth produced an environment")
	}
	broken := claudeAuth{mode: AuthOAuthToken, varName: EnvOAuthToken}
	if _, err := broken.Env(nil); err == nil {
		t.Error("auth with an empty token produced an environment")
	}
}

func TestClaudeAuthDescribeNeverLeaksValues(t *testing.T) {
	const (
		oauthSecret = "describe-oauth-secret-77af31"
		apiSecret   = "sk-ant-describeapikeysecret881"
	)

	for _, tc := range []struct {
		cfg    AuthConfig
		secret string
		want   string
	}{
		{AuthConfig{Mode: AuthExistingLogin}, "", "existing-login"},
		{AuthConfig{Mode: AuthOAuthToken, OAuthToken: oauthSecret}, oauthSecret, "oauth-token"},
		{AuthConfig{Mode: AuthAPIKey, APIKey: apiSecret}, apiSecret, "api-key"},
	} {
		auth, err := NewClaudeAuth(tc.cfg)
		if err != nil {
			t.Fatalf("NewClaudeAuth: %v", err)
		}
		got := auth.Describe()
		if !strings.Contains(got, tc.want) {
			t.Errorf("Describe() = %q, want it to name the mode %q", got, tc.want)
		}
		if tc.secret != "" && strings.Contains(got, tc.secret) {
			t.Errorf("Describe() leaked the secret: %q", got)
		}
	}

	auth, err := NewClaudeAuth(AuthConfig{
		Mode:     AuthExistingLogin,
		ExtraEnv: map[string]string{"ANTHROPIC_BASE_URL": "https://gateway.company.ru"},
	})
	if err != nil {
		t.Fatalf("NewClaudeAuth: %v", err)
	}
	// extra_env is summarised by variable name only — its values may be
	// sensitive too (a gateway URL can embed a token).
	got := auth.Describe()
	if !strings.Contains(got, "ANTHROPIC_BASE_URL") {
		t.Errorf("Describe() = %q, want it to name the extra_env variables", got)
	}
	if strings.Contains(got, "gateway.company.ru") {
		t.Errorf("Describe() leaked an extra_env value: %q", got)
	}
}

// TestClaudeAuthSecretIsRedactedInLogs closes the loop with the logging choke
// point: NewClaudeAuth registers the secret, so even a call site that logs the
// whole environment cannot leak it.
func TestClaudeAuthSecretIsRedactedInLogs(t *testing.T) {
	const secret = "logged-oauth-token-6b1e94c2ad"

	auth, err := NewClaudeAuth(AuthConfig{Mode: AuthOAuthToken, OAuthToken: secret})
	if err != nil {
		t.Fatalf("NewClaudeAuth: %v", err)
	}
	env := mustEnv(t, auth, inheritedEnv())

	var buf bytes.Buffer
	enc := zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig())
	core := security.NewRedactingCore(zapcore.NewCore(enc, zapcore.AddSync(&buf), zapcore.DebugLevel))
	log := zap.New(core)

	log.Info("starting claude "+strings.Join(env, " "),
		zap.String("auth", auth.Describe()),
		zap.Strings("env", env),
	)

	if strings.Contains(buf.String(), secret) {
		t.Errorf("the configured token reached the log: %s", buf.String())
	}
}

func mustEnv(t *testing.T, auth ClaudeAuth, base []string) []string {
	t.Helper()
	env, err := auth.Env(base)
	if err != nil {
		t.Fatalf("Env: %v", err)
	}
	return env
}
