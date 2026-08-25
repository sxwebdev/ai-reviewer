package security

import (
	"slices"
	"strings"
)

// The environment handed to a subprocess is a boundary, not a convenience.
//
// This service holds a GitLab PAT, a Slack token, a Postgres password and a
// Claude credential, and the reference deployment injects all of them with
// `envFrom: secretRef` — so they live in this process's own environment. It
// then spawns processes over code an attacker wrote: the Claude agent reads it,
// and `go test`, `tsc` and the coverage providers *execute* it (which is why
// those are documented opt-ins, plan §20.4). An inherited `os.Environ()` hands
// every one of those secrets to that code.
//
// So inheritance is an allowlist, never a strip list. A strip list can only
// remove the names somebody thought of, and the names that matter most here are
// the AI_REVIEWER_-prefixed ones a list of bare variable names does not think
// of at all. Anything not named below simply does not cross the boundary.

// BaseEnvNames is what any subprocess needs to run at all, regardless of what
// it is. Callers add their own domain's variables through EnvFilter.Allow.
var BaseEnvNames = []string{
	// Process basics. Without PATH nothing resolves; without HOME, tools that
	// keep caches or config under it fall back to surprising places or fail.
	"PATH", "HOME", "USER", "LOGNAME", "SHELL",
	"TMPDIR", "TMP", "TEMP",

	// Locale and time. LC_* is covered by BaseEnvPrefixes.
	"TZ", "LANG", "LANGUAGE",

	// Terminal behaviour and colour.
	"TERM", "COLORTERM", "NO_COLOR", "FORCE_COLOR",

	// XDG base directories: on a read-only root filesystem the deployment must
	// be able to point caches somewhere writable.
	"XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_RUNTIME_DIR",

	// Egress: a corporate proxy and a custom CA are the two settings without
	// which a subprocess simply cannot reach the network it needs.
	"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
	"http_proxy", "https_proxy", "all_proxy", "no_proxy",
	"SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS",
}

// BaseEnvPrefixes extends BaseEnvNames with families whose exact names are
// open-ended. Only LC_* qualifies: a prefix is a far blunter instrument than a
// name, and each new one needs the same scrutiny as a new secret.
var BaseEnvPrefixes = []string{"LC_"}

// ToolchainEnvNames is what a language toolchain needs on top of the base set:
// build and module caches, proxies, and the knobs a monorepo's CI sets.
//
// Note what is deliberately absent. The npm_config_* / NPM_CONFIG_* families
// are not allowed as prefixes, because registry credentials live in exactly
// that shape (npm_config__auth, npm_config_//registry/:_authToken) and
// coverage.node.install runs package-manager lifecycle scripts — attacker code.
// Private-registry auth normally comes from ~/.npmrc, which HOME still reaches.
// SSH_AUTH_SOCK and forge tokens are absent for the same reason: they are
// credentials, and no verifier needs one.
var ToolchainEnvNames = []string{
	// Go.
	"GOROOT", "GOPATH", "GOBIN", "GOCACHE", "GOMODCACHE", "GOTMPDIR",
	"GOFLAGS", "GOPROXY", "GONOPROXY", "GOSUMDB", "GONOSUMDB", "GOPRIVATE", "GOINSECURE",
	"GOTOOLCHAIN", "GO111MODULE", "GOWORK", "GOOS", "GOARCH", "GOARM", "GOAMD64", "GOEXPERIMENT",
	"CGO_ENABLED", "CGO_CFLAGS", "CGO_LDFLAGS", "CC", "CXX",

	// Node / TypeScript.
	"NODE_PATH", "NODE_OPTIONS",
	"NPM_CONFIG_REGISTRY", "npm_config_registry",
	"NPM_CONFIG_PREFIX", "npm_config_prefix",
	"NPM_CONFIG_CACHE", "npm_config_cache",
	"PNPM_HOME", "COREPACK_HOME", "YARN_CACHE_FOLDER",

	// Python.
	"PYTHONPATH", "PYTHONHOME", "PYTHONDONTWRITEBYTECODE", "VIRTUAL_ENV",
}

// EnvFilter selects which of the parent process's variables a subprocess
// inherits. The zero value admits the base set and nothing else.
type EnvFilter struct {
	// Allow names admitted in addition to BaseEnvNames.
	Allow []string
	// AllowPrefix admits every variable starting with one of these.
	AllowPrefix []string
	// Deny is checked first and wins over everything: it is how a caller keeps
	// a variable out that it intends to set explicitly, or that it owns and
	// must never inherit.
	Deny []string
}

// Apply returns the entries of base (in "KEY=VALUE" form, normally
// os.Environ()) that f admits, in their original order. base is not mutated,
// and entries that are not assignments are dropped.
func (f EnvFilter) Apply(base []string) []string {
	out := make([]string, 0, len(BaseEnvNames)+len(f.Allow))
	for _, kv := range base {
		key, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue // not a variable assignment; nothing a child could read
		}
		if f.admits(key) {
			out = append(out, kv)
		}
	}
	return out
}

// admits reports whether one variable name crosses the boundary.
func (f EnvFilter) admits(key string) bool {
	if slices.Contains(f.Deny, key) {
		return false
	}
	if slices.Contains(BaseEnvNames, key) || slices.Contains(f.Allow, key) {
		return true
	}
	for _, p := range BaseEnvPrefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	for _, p := range f.AllowPrefix {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

// ToolchainEnv is the environment for a build, type-check, test or coverage
// subprocess: the base set plus ToolchainEnvNames.
//
// It is applied uniformly to every verifier rather than per-verifier. Whether a
// given one executes repository code today (go_test and tsc do, go_build and
// py_syntax do not) is a property that changes with a flag or a version, and
// deciding it at each call site is how one of them ends up unfiltered.
func ToolchainEnv(base []string) []string {
	return EnvFilter{Allow: ToolchainEnvNames}.Apply(base)
}
