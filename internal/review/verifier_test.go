package review

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

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

func requireGo(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}
}

func runBuildVerifier(t *testing.T, dir string, findings []ValidatedFinding) []ValidatedFinding {
	t.Helper()
	got, _ := runVerifiers(context.Background(), dir,
		[]Verifier{newGoBuildVerifier(discardLog())}, findings, discardLog())
	return got
}

// The exact regression: a blocking finding claims new(0.3) does not compile, but
// under Go 1.26 the package compiles — so the false positive must be dropped.
func TestGoBuildVerifierDropsFalseCompileError(t *testing.T) {
	requireGo(t)
	dir := writeModule(t, map[string]string{
		"go.mod":       "module bt\n\ngo 1.26\n",
		"eval/eval.go": "package eval\n\nvar Min = new(0.3)\n", // valid Go 1.26
	})
	findings := []ValidatedFinding{
		{FilePath: "eval/eval.go", Title: "new(0.3) does not compile",
			Body: "new takes a type, not a value; this breaks the build", Severity: "blocking"},
		{FilePath: "eval/eval.go", Title: "missing test", Body: "add coverage", Severity: "medium"},
	}
	got := runBuildVerifier(t, dir, findings)
	if len(got) != 1 || got[0].Title != "missing test" {
		t.Fatalf("false compile-error finding should be dropped; survivors=%v", titlesOf(got))
	}
}

// A genuine compile error must NOT be dropped — the build fails, so the claim stands.
func TestGoBuildVerifierKeepsRealCompileError(t *testing.T) {
	requireGo(t)
	dir := writeModule(t, map[string]string{
		"go.mod":     "module bt\n\ngo 1.26\n",
		"bad/bad.go": "package bad\n\nvar X = totallyUndefinedSymbol\n",
	})
	findings := []ValidatedFinding{
		{FilePath: "bad/bad.go", Title: "does not compile",
			Body: "undefined symbol; does not compile", Severity: "blocking"},
	}
	if got := runBuildVerifier(t, dir, findings); len(got) != 1 {
		t.Fatalf("a real compile error must be kept, got %d", len(got))
	}
}

// A finding that does not claim a build failure is never touched (and triggers no build).
func TestGoBuildVerifierIgnoresNonCompileClaims(t *testing.T) {
	requireGo(t)
	dir := writeModule(t, map[string]string{
		"go.mod": "module bt\n\ngo 1.26\n",
		"x/x.go": "package x\n\nfunc F() {}\n",
	})
	findings := []ValidatedFinding{
		{FilePath: "x/x.go", Title: "naming nit", Body: "rename F to Foo", Severity: "medium"},
	}
	if got := runBuildVerifier(t, dir, findings); len(got) != 1 {
		t.Fatalf("non-compile-claim finding must be untouched, got %d", len(got))
	}
}

// A TRUE compile claim about a _test.go file must survive: `go build` never
// compiles test files, so its success is not ground truth for them — the
// verifier must use `go test -c` instead.
func TestGoBuildVerifierTestFiles(t *testing.T) {
	requireGo(t)
	dir := writeModule(t, map[string]string{
		"go.mod":            "module bt\n\ngo 1.26\n",
		"pkg/ok.go":         "package pkg\n\nfunc F() {}\n",
		"pkg/bad_test.go":   "package pkg\n\nvar X = totallyUndefined\n",
		"good/g.go":         "package good\n\nfunc G() {}\n",
		"good/good_test.go": "package good\n\nimport \"testing\"\n\nfunc TestG(t *testing.T) { G() }\n",
	})
	findings := []ValidatedFinding{
		{FilePath: "pkg/bad_test.go", Title: "test file does not compile", Body: "undefined symbol; compile error", Severity: "blocking"},
		{FilePath: "good/good_test.go", Title: "does not compile", Body: "claims a compile error", Severity: "high"},
	}
	got := runBuildVerifier(t, dir, findings)
	byTitle := map[string]bool{}
	for _, f := range got {
		byTitle[f.Title] = true
	}
	if !byTitle["test file does not compile"] {
		t.Error("TRUE compile claim about a broken _test.go must be kept")
	}
	if byTitle["does not compile"] {
		t.Error("false compile claim about a valid _test.go must be dropped via go test -c")
	}
}

// Files under build constraints may be excluded from the local build entirely
// — a clean build proves nothing, so the verifier must keep the finding.
func TestGoBuildVerifierKeepsBuildTaggedFiles(t *testing.T) {
	requireGo(t)
	dir := writeModule(t, map[string]string{
		"go.mod":      "module bt\n\ngo 1.26\n",
		"p/other.go":  "package p\n\nfunc P() {}\n",
		"p/tagged.go": "//go:build sparc64\n\npackage p\n\nvar X = brokenOnOtherPlatforms\n",
	})
	findings := []ValidatedFinding{
		{FilePath: "p/tagged.go", Title: "does not compile", Body: "compile error under the tag", Severity: "high"},
	}
	if got := runBuildVerifier(t, dir, findings); len(got) != 1 {
		t.Fatal("build-tagged file cannot be verified locally — the finding must be kept")
	}
}

// Nested-module monorepo: the verifier must resolve the NEAREST go.mod, not
// require one at the worktree root.
func TestGoBuildVerifierNestedModule(t *testing.T) {
	requireGo(t)
	dir := writeModule(t, map[string]string{
		// no root go.mod — the module lives in services/api
		"README.md":                 "docs\n",
		"services/api/go.mod":       "module api\n\ngo 1.26\n",
		"services/api/handler/h.go": "package handler\n\nvar Min = new(0.3)\n", // valid Go 1.26
	})
	findings := []ValidatedFinding{
		{FilePath: "services/api/handler/h.go", Title: "new(0.3) does not compile",
			Body: "new takes a type; compile error", Severity: "high"},
	}
	got := runBuildVerifier(t, dir, findings)
	if len(got) != 0 {
		t.Fatalf("false compile claim in a nested module must be dropped, got %v", titlesOf(got))
	}
}

// No worktree available → verification is a no-op (findings pass through).
func TestRunVerifiersNoWorkdirIsNoop(t *testing.T) {
	findings := []ValidatedFinding{
		{FilePath: "eval/eval.go", Title: "does not compile", Body: "x", Severity: "blocking"},
	}
	got, _ := runVerifiers(context.Background(), "",
		[]Verifier{newGoBuildVerifier(discardLog())}, findings, discardLog())
	if len(got) != 1 {
		t.Fatalf("no-workdir must pass findings through, got %d", len(got))
	}
}

// go vet corroboration: a real vet diagnostic (Printf arity) annotates the
// finding; a clean package never drops or annotates.
func TestGoVetVerifierAnnotatesAndNeverDrops(t *testing.T) {
	requireGo(t)
	dir := writeModule(t, map[string]string{
		"go.mod": "module vt\n\ngo 1.26\n",
		"p/p.go": "package p\n\nimport \"fmt\"\n\nfunc F() { fmt.Printf(\"%d %d\", 1) }\n",
		"q/q.go": "package q\n\nfunc G() {}\n",
	})
	findings := []ValidatedFinding{
		{FilePath: "p/p.go", Title: "printf bug", Body: "arity mismatch", Severity: "high", Category: "correctness"},
		{FilePath: "q/q.go", Title: "logic bug", Body: "suspicious", Severity: "high", Category: "correctness"},
	}
	got, _ := runVerifiers(context.Background(), dir,
		[]Verifier{newGoVetVerifier(discardLog())}, findings, discardLog())
	if len(got) != 2 {
		t.Fatalf("vet verifier must never drop, got %d survivors", len(got))
	}
	if !strings.Contains(got[0].ValidationError, "go vet also reports issues") {
		t.Errorf("vet-flagged file must be annotated: %+v", got[0])
	}
	if got[1].ValidationError != "" {
		t.Errorf("clean package must not be annotated: %+v", got[1])
	}
}

// A diagnostic in http_client.go must not corroborate a finding about
// client.go — exact path matching, no substring coincidences.
func TestGoVetVerifierExactFileMatch(t *testing.T) {
	requireGo(t)
	dir := writeModule(t, map[string]string{
		"go.mod":           "module vt\n\ngo 1.26\n",
		"p/http_client.go": "package p\n\nimport \"fmt\"\n\nfunc H() { fmt.Printf(\"%d %d\", 1) }\n",
		"p/client.go":      "package p\n\nfunc C() {}\n",
	})
	findings := []ValidatedFinding{
		{FilePath: "p/client.go", Title: "logic bug", Body: "suspicious", Severity: "high", Category: "correctness"},
	}
	got, _ := runVerifiers(context.Background(), dir,
		[]Verifier{newGoVetVerifier(discardLog())}, findings, discardLog())
	if len(got) != 1 {
		t.Fatal("vet never drops")
	}
	if got[0].ValidationError != "" {
		t.Errorf("diagnostic in http_client.go must not annotate client.go: %+v", got[0])
	}
}

func TestBuiltinVerifiersUnknownSkipped(t *testing.T) {
	vs := BuiltinVerifiers([]string{"go_build", "nonsense", "go_vet", "go_test"}, discardLog())
	if len(vs) != 3 {
		t.Fatalf("want 3 verifiers (unknown skipped), got %d", len(vs))
	}
	names := []string{vs[0].Name(), vs[1].Name(), vs[2].Name()}
	want := []string{"go_build", "go_vet", "go_test"}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("verifier order wrong: %v", names)
		}
	}
}

func TestHasBuildConstraintAfterLongHeader(t *testing.T) {
	dir := t.TempDir()
	header := "// " + strings.Repeat("license text ", 120) + "\n" // > 1KB before the constraint
	content := header + "//go:build integration\n\npackage p\n"
	if err := os.WriteFile(filepath.Join(dir, "tagged.go"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if !hasBuildConstraint(dir, "tagged.go") {
		t.Error("constraint after a >1KB header must still be detected")
	}
	if err := os.WriteFile(filepath.Join(dir, "plain.go"), []byte(header+"package p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if hasBuildConstraint(dir, "plain.go") {
		t.Error("file without a constraint must report false")
	}
}
