package review

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeTree writes files (path -> content) into a temp dir; 0o755 for files
// under node_modules/.bin so they are executable.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if strings.Contains(rel, "node_modules/.bin/") {
			mode = 0o755
		}
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func tsFinding(file, title, body string) ValidatedFinding {
	return ValidatedFinding{FilePath: file, Title: title, Body: body, Severity: "high", Category: "correctness"}
}

// fakeTSC returns a shell script emitting the given output with the exit code.
func fakeTSC(t *testing.T, output string, exit int, marker string) string {
	t.Helper()
	return fmt.Sprintf("#!/bin/sh\necho run >> %s\nprintf '%%s\\n' '%s'\nexit %d\n", marker, output, exit)
}

func TestTSCVerifierDropOnClean(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix shell required")
	}
	marker := filepath.Join(t.TempDir(), "runs")
	wd := writeTree(t, map[string]string{
		"web/tsconfig.json":         "{}",
		"web/src/App.tsx":           "export {}",
		"web/node_modules/.bin/tsc": fakeTSC(t, "", 0, marker),
	})
	findings := []ValidatedFinding{
		tsFinding("web/src/App.tsx", "type error in App", "this does not typecheck"),
		tsFinding("web/src/App.tsx", "naming", "rename please"), // no claim → untouched
	}
	got := runVerifiers(context.Background(), wd, []Verifier{newTSCVerifier(discardLog())}, findings, discardLog())
	if len(got) != 1 || got[0].Title != "naming" {
		t.Fatalf("clean tsc must drop the type-error claim: %v", titlesOf(got))
	}
	if runs, _ := os.ReadFile(marker); strings.Count(string(runs), "run") != 1 {
		t.Errorf("tsc must run once per root (cache), got %q", runs)
	}
}

func TestTSCVerifierAnnotateWhenFileFlagged(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix shell required")
	}
	marker := filepath.Join(t.TempDir(), "runs")
	wd := writeTree(t, map[string]string{
		"tsconfig.json":         "{}",
		"src/App.tsx":           "export {}",
		"node_modules/.bin/tsc": fakeTSC(t, "src/App.tsx(3,5): error TS2322: nope", 2, marker),
	})
	findings := []ValidatedFinding{tsFinding("src/App.tsx", "type error", "not assignable to string")}
	got := runVerifiers(context.Background(), wd, []Verifier{newTSCVerifier(discardLog())}, findings, discardLog())
	if len(got) != 1 {
		t.Fatal("flagged file must be kept")
	}
	if !strings.Contains(got[0].ValidationError, "tsc also reports errors") {
		t.Errorf("annotation missing: %+v", got[0])
	}
}

func TestTSCVerifierKeepWhenErrorsElsewhere(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix shell required")
	}
	marker := filepath.Join(t.TempDir(), "runs")
	wd := writeTree(t, map[string]string{
		"tsconfig.json":         "{}",
		"src/App.tsx":           "export {}",
		"node_modules/.bin/tsc": fakeTSC(t, "src/Other.tsx(1,1): error TS2304: x", 2, marker),
	})
	findings := []ValidatedFinding{tsFinding("src/App.tsx", "type error", "does not compile")}
	got := runVerifiers(context.Background(), wd, []Verifier{newTSCVerifier(discardLog())}, findings, discardLog())
	if len(got) != 1 || got[0].ValidationError != "" {
		t.Fatalf("errors in other files must neither drop nor annotate: %+v", got)
	}
}

func TestTSCVerifierKeepWithoutTsconfigOrTSC(t *testing.T) {
	wd := writeTree(t, map[string]string{"src/App.tsx": "export {}"})
	findings := []ValidatedFinding{tsFinding("src/App.tsx", "type error", "does not typecheck")}
	got := runVerifiers(context.Background(), wd, []Verifier{newTSCVerifier(discardLog())}, findings, discardLog())
	if len(got) != 1 {
		t.Fatal("no tsconfig → keep")
	}
}

func TestTSCVerifierApplies(t *testing.T) {
	v := newTSCVerifier(discardLog())
	if v.Applies(tsFinding("a.d.ts", "type error", "x")) {
		t.Error("d.ts must not apply")
	}
	if v.Applies(tsFinding("a.ts", "slow loop", "performance")) {
		t.Error("non-claim must not apply")
	}
	if !v.Applies(tsFinding("a.tsx", "broken types", "ts(2322) not assignable to")) {
		t.Error("tsx with claim must apply")
	}
}

func requirePython(t *testing.T) string {
	t.Helper()
	for _, bin := range []string{"python3", "python"} {
		if p, err := exec.LookPath(bin); err == nil {
			return p
		}
	}
	t.Skip("python not available")
	return ""
}

func TestPySyntaxVerifier(t *testing.T) {
	requirePython(t)
	wd := writeTree(t, map[string]string{
		"good.py": "def f():\n    return 1\n",
		"bad.py":  "def f(:\n",
	})
	findings := []ValidatedFinding{
		{FilePath: "good.py", Title: "syntax error", Body: "invalid syntax here", Severity: "high"},
		{FilePath: "bad.py", Title: "syntax error", Body: "invalid syntax here", Severity: "high"},
		{FilePath: "good.py", Title: "logic", Body: "off by one", Severity: "high"},
	}
	got := runVerifiers(context.Background(), wd, []Verifier{newPySyntaxVerifier(discardLog())}, findings, discardLog())
	byTitleFile := map[string]ValidatedFinding{}
	for _, f := range got {
		byTitleFile[f.FilePath+"/"+f.Title] = f
	}
	if _, ok := byTitleFile["good.py/syntax error"]; ok {
		t.Error("false syntax claim on parsing file must be dropped")
	}
	bad, ok := byTitleFile["bad.py/syntax error"]
	if !ok || !strings.Contains(bad.ValidationError, "python3 confirms") {
		t.Errorf("real syntax error must be kept and annotated: %+v", bad)
	}
	if _, ok := byTitleFile["good.py/logic"]; !ok {
		t.Error("non-claim finding must be untouched")
	}
}

func TestBuiltinVerifiersIncludeLangVerifiers(t *testing.T) {
	vs := BuiltinVerifiers([]string{"tsc", "py_syntax"}, discardLog())
	if len(vs) != 2 || vs[0].Name() != "tsc" || vs[1].Name() != "py_syntax" {
		t.Fatalf("tsc/py_syntax must be registered: %v", vs)
	}
}
