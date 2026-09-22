package coverage

import (
	"runtime"
	"strings"
	"testing"
)

// TestExecRunnerFiltersTheSubprocessEnvironment pins the boundary at the one
// place this package crosses it.
//
// ExecRunner runs the repository's own test command, and with
// coverage.node.install its package manager's lifecycle scripts — attacker code,
// by design, which is why the whole feature is an explicit opt-in (plan §20.4).
// The parent process holds the service's GitLab PAT, Slack token and Postgres
// password, so an inherited os.Environ() would hand them straight over.
func TestExecRunnerFiltersTheSubprocessEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("/usr/bin/env is unix-only")
	}

	secrets := map[string]string{
		"AI_REVIEWER_GITLAB_TOKEN":            "glpat-coverageservicepat01",
		"AI_REVIEWER_SLACK_TOKEN":             "xoxb-000000-coverageslack",
		"AI_REVIEWER_POSTGRES_PASSWORD":       "coverage-postgres-password",
		"AI_REVIEWER_CLAUDE_CODE_OAUTH_TOKEN": "coverage-prefixed-oauth-tk",
		"npm_config__auth":                    "coverage-registry-auth-tok",
	}
	for k, v := range secrets {
		t.Setenv(k, v)
	}
	t.Setenv("GOFLAGS", "-mod=mod")

	out, err := ExecRunner(t.Context(), t.TempDir(), []string{"COVERAGE_EXTRA=1"}, "/usr/bin/env")
	if err != nil {
		t.Fatalf("ExecRunner: %v (%s)", err, out)
	}

	got := map[string]string{}
	for line := range strings.SplitSeq(string(out), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			got[k] = v
		}
	}

	for name := range secrets {
		if v, present := got[name]; present {
			t.Errorf("service secret %s reached the coverage subprocess as %q", name, v)
		}
	}
	// A filtered environment still has to run a toolchain, and the caller's own
	// entries must still be appended.
	if got["PATH"] == "" {
		t.Error("PATH did not reach the subprocess")
	}
	if got["GOFLAGS"] != "-mod=mod" {
		t.Errorf("GOFLAGS = %q, want the inherited toolchain setting", got["GOFLAGS"])
	}
	if got["COVERAGE_EXTRA"] != "1" {
		t.Errorf("COVERAGE_EXTRA = %q, want the caller's own entry to survive", got["COVERAGE_EXTRA"])
	}
}
