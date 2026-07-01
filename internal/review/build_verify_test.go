package review

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func testEngine() *Engine {
	return &Engine{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func writeModule(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func titlesOf(fs []ValidatedFinding) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.Title)
	}
	return out
}

// The exact regression: a blocking finding claims new(0.3) does not compile, but
// under Go 1.26 the package compiles — so the false positive must be dropped.
func TestVerifyBuildClaimsDropsFalseCompileError(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	dir := writeModule(t, map[string]string{
		"go.mod":       "module bt\n\ngo 1.26\n",
		"eval/eval.go": "package eval\n\nvar Min = new(0.3)\n", // valid Go 1.26
	})
	findings := []ValidatedFinding{
		{FilePath: "eval/eval.go", Title: "new(0.3) does not compile",
			Body: "new takes a type, not a value; this breaks the build", Severity: "blocking"},
		{FilePath: "eval/eval.go", Title: "missing test", Body: "add coverage", Severity: "medium"},
	}
	got := testEngine().verifyBuildClaims(context.Background(), dir, findings)
	if len(got) != 1 || got[0].Title != "missing test" {
		t.Fatalf("false compile-error finding should be dropped; survivors=%v", titlesOf(got))
	}
}

// A genuine compile error must NOT be dropped — the build fails, so the claim stands.
func TestVerifyBuildClaimsKeepsRealCompileError(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	dir := writeModule(t, map[string]string{
		"go.mod":     "module bt\n\ngo 1.26\n",
		"bad/bad.go": "package bad\n\nvar X = totallyUndefinedSymbol\n",
	})
	findings := []ValidatedFinding{
		{FilePath: "bad/bad.go", Title: "does not compile",
			Body: "undefined symbol; does not compile", Severity: "blocking"},
	}
	got := testEngine().verifyBuildClaims(context.Background(), dir, findings)
	if len(got) != 1 {
		t.Fatalf("a real compile error must be kept, got %d", len(got))
	}
}

// A finding that does not claim a build failure is never touched (and triggers no build).
func TestVerifyBuildClaimsIgnoresNonCompileClaims(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
	dir := writeModule(t, map[string]string{
		"go.mod": "module bt\n\ngo 1.26\n",
		"x/x.go": "package x\n\nfunc F() {}\n",
	})
	findings := []ValidatedFinding{
		{FilePath: "x/x.go", Title: "naming nit", Body: "rename F to Foo", Severity: "medium"},
	}
	got := testEngine().verifyBuildClaims(context.Background(), dir, findings)
	if len(got) != 1 {
		t.Fatalf("non-compile-claim finding must be untouched, got %d", len(got))
	}
}

// No worktree available → verification is a no-op (findings pass through).
func TestVerifyBuildClaimsNoWorkdirIsNoop(t *testing.T) {
	findings := []ValidatedFinding{
		{FilePath: "eval/eval.go", Title: "does not compile", Body: "x", Severity: "blocking"},
	}
	got := testEngine().verifyBuildClaims(context.Background(), "", findings)
	if len(got) != 1 {
		t.Fatalf("no-workdir must pass findings through, got %d", len(got))
	}
}
