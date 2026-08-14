package review

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The verifiers spawn processes inside the merge request's own worktree, using
// the merge request's own toolchain configuration — and go_test and tsc execute
// its code outright, which is why they are opt-in (plan §20.4). This process's
// environment holds the service's GitLab PAT, Slack token, Postgres password
// and Claude credential, because the reference deployment injects them with
// `envFrom: secretRef`. So what these tests pin is a boundary: an inherited
// os.Environ() hands every one of those secrets to code an attacker wrote.
//
// Each test drives the real spawning path with a fake toolchain binary that
// records the environment it actually received.

// verifierSecretEnv is planted in this process before each spawn.
var verifierSecretEnv = map[string]string{
	"AI_REVIEWER_GITLAB_TOKEN":            "glpat-verifierservicepat001",
	"AI_REVIEWER_SLACK_TOKEN":             "xoxb-000000-verifierslack",
	"AI_REVIEWER_POSTGRES_PASSWORD":       "verifier-postgres-password",
	"AI_REVIEWER_CLAUDE_CODE_OAUTH_TOKEN": "verifier-prefixed-oauth-tok",
	"ANTHROPIC_API_KEY":                   "sk-ant-verifierapikey00001",
}

// plantSecrets puts the service's secrets in this process's environment, the
// way `envFrom: secretRef` does, and returns a toolchain setting that must
// survive filtering.
func plantSecrets(t *testing.T) {
	t.Helper()
	for k, v := range verifierSecretEnv {
		t.Setenv(k, v)
	}
	t.Setenv("GOFLAGS", "-mod=mod")
}

// fakeToolBinary writes an executable at dir/name that records its environment
// to the returned path and exits 0.
func fakeToolBinary(t *testing.T, dir, name string) (bin, dump string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake shell-script binary is unix-only")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	dump = filepath.Join(t.TempDir(), name+"-env.txt")
	bin = filepath.Join(dir, name)
	script := "#!/bin/sh\n/usr/bin/env > '" + dump + "'\nexit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, dump
}

// assertSubprocessEnv reads a recorded environment and checks the boundary held.
func assertSubprocessEnv(t *testing.T, dump string) {
	t.Helper()
	raw, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("the subprocess did not run: %v", err)
	}
	got := map[string]string{}
	for line := range strings.SplitSeq(string(raw), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			got[k] = v
		}
	}

	for name := range verifierSecretEnv {
		if v, present := got[name]; present {
			t.Errorf("service secret %s reached the verifier subprocess as %q", name, v)
		}
	}
	// The filter has to leave a usable toolchain behind, or every verifier
	// degrades to "cannot judge" and the whole layer quietly stops working.
	if got["PATH"] == "" {
		t.Error("PATH did not reach the subprocess")
	}
	if got["GOFLAGS"] != "-mod=mod" {
		t.Errorf("GOFLAGS = %q, want the inherited toolchain setting", got["GOFLAGS"])
	}
}

// runGoCmd is the shared spawn path for go_build, go_vet and go_test.
func TestRunGoCmdFiltersTheSubprocessEnvironment(t *testing.T) {
	plantSecrets(t)
	workDir := t.TempDir()
	goBin, dump := fakeToolBinary(t, t.TempDir(), "go")

	if _, ok := runGoCmd(t.Context(), 30*time.Second, goBin, workDir, ".", "build", "./..."); !ok {
		t.Fatal("the fake go binary did not run cleanly")
	}
	assertSubprocessEnv(t, dump)
}

// runTSC executes the repository's own node_modules/.bin/tsc — attacker-supplied
// code by construction.
func TestRunTSCFiltersTheSubprocessEnvironment(t *testing.T) {
	plantSecrets(t)
	workDir := t.TempDir()
	_, dump := fakeToolBinary(t, filepath.Join(workDir, "node_modules", ".bin"), "tsc")

	if out := runTSC(t.Context(), workDir, "."); !out.ran || !out.clean {
		t.Fatalf("the fake tsc did not run cleanly: %+v", out)
	}
	assertSubprocessEnv(t, dump)
}

// py_syntax only ast.parses, but it is filtered like the rest: which verifier
// executes repository code is a property that moves with a flag or a version.
func TestRunPyParseFiltersTheSubprocessEnvironment(t *testing.T) {
	plantSecrets(t)
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, "m.py"), []byte("x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	_, dump := fakeToolBinary(t, binDir, "python3")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if got := runPyParse(t.Context(), workDir, "m.py"); got != 0 {
		t.Fatalf("runPyParse = %d, want 0 (the fake python3 exits 0)", got)
	}
	assertSubprocessEnv(t, dump)
}
