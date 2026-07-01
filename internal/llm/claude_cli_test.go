package llm

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestClaudeErrDetailSurfacesEnvelopeResult(t *testing.T) {
	// The exact shape claude emits on a non-zero exit: error text on stdout.
	stdout := []byte(`{"type":"result","is_error":true,"result":"Not logged in · Please run /login","total_cost_usd":0}`)
	got := claudeErrDetail(stdout, "")
	if got != "Not logged in · Please run /login" {
		t.Errorf("claudeErrDetail = %q, want the envelope result", got)
	}
	if claudeErrHint(got) == "" {
		t.Error("expected an actionable login hint for a not-logged-in error")
	}
}

func TestClaudeErrDetailFallbacks(t *testing.T) {
	if got := claudeErrDetail([]byte("plain non-json boom"), ""); got != "plain non-json boom" {
		t.Errorf("stdout fallback = %q", got)
	}
	if got := claudeErrDetail(nil, "stderr boom"); got != "stderr boom" {
		t.Errorf("stderr fallback = %q", got)
	}
	if got := claudeErrDetail(nil, ""); got == "" {
		t.Error("want a non-empty default when both streams are empty")
	}
}

func TestClaudeErrDetailSurfacesSubtype(t *testing.T) {
	// error_max_structured_output_retries: is_error with an empty result — the
	// subtype must be surfaced instead of dumping the whole JSON envelope.
	stdout := []byte(`{"type":"result","subtype":"error_max_structured_output_retries","is_error":true,"result":""}`)
	got := claudeErrDetail(stdout, "")
	if got != "error: error_max_structured_output_retries" {
		t.Errorf("claudeErrDetail = %q, want the subtype surfaced", got)
	}
	if claudeErrHint(got) == "" {
		t.Error("expected an actionable hint for the structured-output error")
	}
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func writeFakeClaude(t *testing.T, script string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// A fake `claude` that fails with error_max_structured_output_retries whenever
// --json-schema is passed and succeeds otherwise, proving invoke() falls back to
// a schema-less call and extracts the JSON itself.
func TestInvokeFallsBackWhenSchemaUnsatisfiable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake shell-script binary is unix-only")
	}
	script := `#!/bin/sh
for a in "$@"; do
  if [ "$a" = "--json-schema" ]; then
    printf '%s' '{"type":"result","subtype":"error_max_structured_output_retries","is_error":true,"result":""}'
    exit 1
  fi
done
printf '%s' '{"type":"result","subtype":"success","is_error":false,"result":"{\"summary\":\"ok\",\"findings\":[]}"}'
exit 0
`
	c := NewClaudeCLI(ClaudeOptions{Bin: writeFakeClaude(t, script), Timeout: 30 * time.Second}, testLogger())
	resp, err := c.Review(context.Background(), Request{Prompt: "review", JSONSchema: ReviewJSONSchema})
	if err != nil {
		t.Fatalf("fallback should have recovered, got error: %v", err)
	}
	if resp == nil || resp.Summary != "ok" {
		t.Fatalf("unexpected response: %+v", resp)
	}
}
