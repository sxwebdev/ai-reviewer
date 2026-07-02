package review

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/sxwebdev/ai-reviewer/internal/toolchain"
)

const tscVerifyTimeout = 120 * time.Second

// typecheckFailurePhrases mark a finding as asserting a TypeScript
// type/compile failure. Bilingual, like compileFailurePhrases.
var typecheckFailurePhrases = []string{
	"type error", "typescript error", "does not typecheck", "won't typecheck",
	"fails to typecheck", "fails type checking", "type mismatch", "ts(2",
	"tsc error", "not assignable to", "compile error", "does not compile",
	"doesn't compile", "compilation error",
	"не типизируется", "ошибка типов", "ошибка типизации", "не компилируется",
	"ошибка компиляции",
}

func claimsTypecheckFailure(f ValidatedFinding) bool {
	hay := strings.ToLower(f.Title + "\n" + f.Body)
	for _, p := range typecheckFailurePhrases {
		if strings.Contains(hay, p) {
			return true
		}
	}
	return false
}

// tscDiagRe parses "path(line,col): error TSxxxx: message" diagnostics.
var tscDiagRe = regexp.MustCompile(`(?m)^(.+?)\(\d+,\d+\): error TS\d+`)

type tscOutcome struct {
	ran        bool            // tsc executed and produced a judgeable result
	clean      bool            // no diagnostics
	errorFiles map[string]bool // repo-relative files with diagnostics
}

// tscVerifier refutes false "type error / does not compile" claims about
// TypeScript files by running `tsc --noEmit` on the finding's nearest
// tsconfig.json project. Conservative: Drop only on a fully clean check;
// missing tsc, timeouts, or failures to run → Keep. Opt-in via config because
// it executes the repository's own tsc binary (node_modules/.bin) — the same
// trust level as the go_test verifier.
type tscVerifier struct {
	log     *slog.Logger
	results map[string]tscOutcome // per tsconfig root, review lifetime
}

func newTSCVerifier(log *slog.Logger) *tscVerifier {
	return &tscVerifier{log: log, results: map[string]tscOutcome{}}
}

func (v *tscVerifier) Name() string { return "tsc" }

func (v *tscVerifier) Applies(f ValidatedFinding) bool {
	p := strings.ToLower(f.FilePath)
	if strings.HasSuffix(p, ".d.ts") {
		return false
	}
	if !strings.HasSuffix(p, ".ts") && !strings.HasSuffix(p, ".tsx") &&
		!strings.HasSuffix(p, ".mts") && !strings.HasSuffix(p, ".cts") {
		return false
	}
	return claimsTypecheckFailure(f)
}

func (v *tscVerifier) Verify(ctx context.Context, workDir string, f ValidatedFinding) VerifierResult {
	root, ok := toolchain.NearestRoot(workDir, f.FilePath, []string{"tsconfig.json"})
	if !ok {
		return VerifierResult{Verdict: VerdictKeep}
	}
	out, cached := v.results[root]
	if !cached {
		out = runTSC(ctx, workDir, root)
		v.results[root] = out
	}
	switch {
	case !out.ran:
		return VerifierResult{Verdict: VerdictKeep}
	case out.clean:
		return VerifierResult{Verdict: VerdictDrop,
			Note: "tsc --noEmit passes for " + root + ", contradicting the type-error claim"}
	case out.errorFiles[f.FilePath]:
		return VerifierResult{Verdict: VerdictAnnotate, Note: "tsc also reports errors in this file"}
	default:
		// Errors elsewhere: a broken sibling neither refutes nor confirms
		// claims about this file.
		return VerifierResult{Verdict: VerdictKeep}
	}
}

// runTSC executes tsc --noEmit for one tsconfig root and classifies the output.
func runTSC(ctx context.Context, workDir, root string) tscOutcome {
	tscBin := findTSC(workDir, root)
	if tscBin == "" {
		return tscOutcome{}
	}
	absRoot := filepath.Join(workDir, filepath.FromSlash(root))
	ctx, cancel := context.WithTimeout(ctx, tscVerifyTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, tscBin, "--noEmit", "--pretty", "false", "-p", absRoot)
	cmd.Dir = absRoot
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return tscOutcome{} // timeout → environmental, not judgeable
	}
	if err == nil {
		return tscOutcome{ran: true, clean: true}
	}
	// Non-zero exit: judgeable only when we can see diagnostics.
	matches := tscDiagRe.FindAllStringSubmatch(string(out), -1)
	if len(matches) == 0 {
		return tscOutcome{} // crashed/misconfigured — not a type-check verdict
	}
	files := map[string]bool{}
	for _, m := range matches {
		p := filepath.ToSlash(m[1])
		if !strings.HasPrefix(p, root+"/") && root != "." {
			p = path.Join(root, p) // tsc prints paths relative to cwd (the root)
		}
		files[p] = true
	}
	return tscOutcome{ran: true, errorFiles: files}
}

// findTSC locates the repository's tsc: node_modules/.bin/tsc walking up from
// the tsconfig root to the worktree top (monorepo hoisting), then PATH.
func findTSC(workDir, root string) string {
	dir := root
	for {
		cand := filepath.Join(workDir, filepath.FromSlash(dir), "node_modules", ".bin", "tsc")
		if info, err := os.Stat(cand); err == nil && !info.IsDir() {
			return cand
		}
		if dir == "." || dir == "/" {
			break
		}
		dir = path.Dir(dir)
	}
	if p, err := exec.LookPath("tsc"); err == nil {
		return p
	}
	return ""
}
