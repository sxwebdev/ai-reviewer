package app

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sxwebdev/ai-reviewer/internal/security"
	"github.com/tkcrm/mx/logger"
)

// captureStdout swaps os.Stdout for a pipe, runs fn, and returns what was
// written. mx's logger binds os.Stdout at construction, so fn must build the
// logger it uses.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	fn()

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// TestNewLoggerRedacts pins the redaction wiring: NewLogger must wrap the zap
// core with security.NewRedactingCore, so a registered secret cannot reach the
// output through a message or a field no matter which call site logs it.
func TestNewLoggerRedacts(t *testing.T) {
	const token = "glpat-doNotLeakThisValue" //nolint:gosec // test fixture
	security.RegisterSecret(token)

	out := captureStdout(t, func() {
		lg := NewLogger(logger.Config{Level: logger.LogLevelInfo, Format: logger.LoggerFormatJSON})
		lg.Infow("cloning with "+token, "token", token)
		_ = lg.Sync()
	})

	if strings.Contains(out, token) {
		t.Fatalf("the application logger leaked a registered secret:\n%s", out)
	}
	if !strings.Contains(out, "cloning with") {
		t.Fatalf("redaction ate the whole record:\n%s", out)
	}
	// The app/version fields identify the process in aggregated logs.
	if !strings.Contains(out, `"app":"`+AppName+`"`) {
		t.Errorf("missing app field:\n%s", out)
	}
}

func TestNewLoadsConfigAndHonoursDebug(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yml := `
log:
  level: warn
gitlab:
  base_url: https://gitlab.example.com
  token: glpat-test
slack:
  token: xoxb-test
postgres:
  username: ai_reviewer
teams:
  - name: payments
    slack_channel: C012345678
    ai_review: { enabled: true }
    repositories: [backend/payments]
`
	if err := os.WriteFile(path, []byte(yml), 0o600); err != nil {
		t.Fatal(err)
	}
	boot := logger.New(logger.WithConfig(logger.Config{Level: logger.LogLevelFatal, Format: logger.LoggerFormatJSON}))

	a, err := New(t.Context(), boot, Options{ConfigPaths: []string{path}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	if a.Config.GitLab.BaseURL != "https://gitlab.example.com" {
		t.Errorf("config not loaded: %+v", a.Config.GitLab)
	}
	if a.Config.Log.Level != logger.LogLevelWarn {
		t.Errorf("log level = %q, want the file's warn", a.Config.Log.Level)
	}

	// --debug must win over the file, which is the entire point of the flag.
	b, err := New(t.Context(), boot, Options{ConfigPaths: []string{path}, Debug: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	if b.Config.Log.Level != logger.LogLevelDebug {
		t.Errorf("log level = %q, want debug from the flag", b.Config.Log.Level)
	}
}

func TestNewReportsConfigErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("gitlab:\n  base_url: not-a-url\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	boot := logger.New(logger.WithConfig(logger.Config{Level: logger.LogLevelFatal, Format: logger.LoggerFormatJSON}))
	if _, err := New(t.Context(), boot, Options{ConfigPaths: []string{path}}); err == nil {
		t.Fatal("an invalid config must fail the load")
	}
}
