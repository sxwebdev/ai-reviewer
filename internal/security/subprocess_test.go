package security

import (
	"slices"
	"strings"
	"testing"
)

// serviceSecretEnv is what `envFrom: secretRef` puts in this process's own
// environment. None of it may cross a process boundary into code an attacker
// wrote. The AI_REVIEWER_-prefixed spellings are the ones the service actually
// reads, and the ones a strip list of bare variable names never catches.
var serviceSecretEnv = []string{
	"AI_REVIEWER_GITLAB_TOKEN=glpat-subprocservicepat0001",
	"AI_REVIEWER_SLACK_TOKEN=xoxb-000000-subprocslack",
	"AI_REVIEWER_POSTGRES_PASSWORD=subproc-postgres-password",
	"AI_REVIEWER_CLAUDE_CODE_OAUTH_TOKEN=subproc-prefixed-oauth",
	"ANTHROPIC_API_KEY=sk-ant-subprocapikey000001",
	"VAULT_TOKEN=hvs.subprocvaulttoken0001",
	"AWS_SECRET_ACCESS_KEY=subproc-aws-secret-key",
	"SSH_AUTH_SOCK=/tmp/ssh-agent.sock",
	"GITHUB_TOKEN=ghp_subprocforgetoken0001",
	// Registry auth: the reason npm_config_* is not admitted as a prefix.
	"npm_config__auth=subproc-npm-registry-auth",
	"NPM_CONFIG__AUTH=subproc-npm-registry-auth2",
}

func envKeys(t *testing.T, env []string) map[string]string {
	t.Helper()
	out := make(map[string]string, len(env))
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			t.Fatalf("malformed environment entry %q", kv)
		}
		out[k] = v
	}
	return out
}

func assertNoServiceSecret(t *testing.T, env map[string]string) {
	t.Helper()
	for _, kv := range serviceSecretEnv {
		name, _, _ := strings.Cut(kv, "=")
		if v, present := env[name]; present {
			t.Errorf("%s crossed the process boundary as %q", name, v)
		}
	}
}

func TestEnvFilterAdmitsOnlyTheBaseSetByDefault(t *testing.T) {
	base := append([]string{
		"PATH=/usr/bin", "HOME=/home/app", "TZ=Europe/Moscow",
		"LANG=en_US.UTF-8", "LC_TIME=ru_RU.UTF-8",
		"HTTPS_PROXY=http://proxy:3128", "SSL_CERT_FILE=/etc/ssl/ca.pem",
		"SOMETHING_ELSE=nope",
		"not-an-assignment",
	}, serviceSecretEnv...)

	got := envKeys(t, EnvFilter{}.Apply(base))

	for k, want := range map[string]string{
		"PATH": "/usr/bin", "HOME": "/home/app", "TZ": "Europe/Moscow",
		"LANG": "en_US.UTF-8", "LC_TIME": "ru_RU.UTF-8",
		"HTTPS_PROXY": "http://proxy:3128", "SSL_CERT_FILE": "/etc/ssl/ca.pem",
	} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
	if v, present := got["SOMETHING_ELSE"]; present {
		t.Errorf("an unlisted variable was admitted as %q", v)
	}
	assertNoServiceSecret(t, got)
}

func TestEnvFilterAllowAndDeny(t *testing.T) {
	base := append([]string{
		"PATH=/usr/bin",
		"MY_VAR=allowed-by-name",
		"TEAM_REGION=allowed-by-prefix",
		"TEAM_TIER=allowed-by-prefix",
	}, serviceSecretEnv...)

	got := envKeys(t, EnvFilter{
		Allow:       []string{"MY_VAR", "AI_REVIEWER_GITLAB_TOKEN"},
		AllowPrefix: []string{"TEAM_"},
		// Deny wins over Allow, so a caller can hold a variable back even when
		// something else in its own configuration admits it.
		Deny: []string{"AI_REVIEWER_GITLAB_TOKEN", "PATH"},
	}.Apply(base))

	for _, k := range []string{"MY_VAR", "TEAM_REGION", "TEAM_TIER"} {
		if got[k] == "" {
			t.Errorf("%s was not admitted", k)
		}
	}
	if v, present := got["PATH"]; present {
		t.Errorf("Deny lost to the base set: PATH = %q", v)
	}
	if v, present := got["AI_REVIEWER_GITLAB_TOKEN"]; present {
		t.Errorf("Deny lost to Allow: the PAT was admitted as %q", v)
	}
}

func TestEnvFilterDoesNotMutateItsInput(t *testing.T) {
	base := []string{"PATH=/usr/bin", "AI_REVIEWER_GITLAB_TOKEN=glpat-donotmutate00001"}
	before := slices.Clone(base)
	EnvFilter{}.Apply(base)
	if !slices.Equal(base, before) {
		t.Errorf("Apply mutated its input: %v", base)
	}
}

// TestToolchainEnvKeepsBuildSettingsAndDropsCredentials is the guard for the
// verifier and coverage subprocesses. They run the merge request's own
// toolchain — go_test, tsc and the coverage providers execute its code outright
// — so the environment they get is the whole boundary.
func TestToolchainEnvKeepsBuildSettingsAndDropsCredentials(t *testing.T) {
	base := append([]string{
		"PATH=/usr/bin", "HOME=/home/app",
		"GOFLAGS=-mod=mod", "GOMODCACHE=/work/gomodcache", "GOCACHE=/work/gocache",
		"GOPROXY=https://proxy.internal", "CGO_ENABLED=0",
		"NODE_PATH=/work/node_modules", "NPM_CONFIG_REGISTRY=https://registry.internal",
		"PYTHONDONTWRITEBYTECODE=1",
	}, serviceSecretEnv...)

	got := envKeys(t, ToolchainEnv(base))

	for k, want := range map[string]string{
		"PATH": "/usr/bin", "HOME": "/home/app",
		"GOFLAGS": "-mod=mod", "GOMODCACHE": "/work/gomodcache", "GOCACHE": "/work/gocache",
		"GOPROXY": "https://proxy.internal", "CGO_ENABLED": "0",
		"NODE_PATH": "/work/node_modules", "NPM_CONFIG_REGISTRY": "https://registry.internal",
		"PYTHONDONTWRITEBYTECODE": "1",
	} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q — a toolchain setting was dropped", k, got[k], want)
		}
	}
	assertNoServiceSecret(t, got)
}

// The npm_config_* families carry registry credentials, so they are admitted by
// name and never by prefix. coverage.node.install runs lifecycle scripts, which
// is attacker code, and a registry token is a credential like any other.
func TestToolchainEnvDoesNotAdmitNpmConfigByPrefix(t *testing.T) {
	for _, name := range []string{"npm_config_", "NPM_CONFIG_", "GO", "PYTHON", "NODE_"} {
		if slices.Contains(BaseEnvPrefixes, name) {
			t.Errorf("%q is a base prefix; it would admit every variable starting with it", name)
		}
	}
	got := envKeys(t, ToolchainEnv([]string{
		"npm_config__auth=registry-token",
		"npm_config_registry=https://registry.internal",
	}))
	if v, present := got["npm_config__auth"]; present {
		t.Errorf("a registry credential was admitted as %q", v)
	}
	if got["npm_config_registry"] == "" {
		t.Error("npm_config_registry must still be admitted by name")
	}
}
