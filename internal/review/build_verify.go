package review

import (
	"context"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// buildVerifyTimeout bounds a single `go build` used to verify a finding's
// compile-failure claim. Dependencies compile into the shared build cache on the
// first package, so subsequent checks are fast.
const buildVerifyTimeout = 120 * time.Second

// compileFailurePhrases are lowercased substrings that mark a finding as
// asserting the code does not compile/build. Kept broad and bilingual because
// the comment language is configurable (en/ru/auto).
var compileFailurePhrases = []string{
	"does not compile", "doesn't compile", "won't compile", "will not compile",
	"fails to compile", "cannot compile", "not compile", "compile error",
	"compilation error", "compile-time error", "does not build", "won't build",
	"fails to build", "breaks the build", "break the build", "build failure",
	"build error", "not valid go", "invalid go", "is not a type",
	"не компилируется", "не скомпилируется", "ошибка компиляции",
	"ломает сборку", "сломает сборку", "не собирается", "не соберётся",
	"невалидный go", "не валидный go",
}

// claimsCompileFailure reports whether a finding asserts that the code does not
// compile or build.
func claimsCompileFailure(f ValidatedFinding) bool {
	hay := strings.ToLower(f.Title + "\n" + f.Body)
	for _, p := range compileFailurePhrases {
		if strings.Contains(hay, p) {
			return true
		}
	}
	return false
}

// verifyBuildClaims drops findings that assert a Go compile/build failure when
// the affected package actually builds. A successful `go build` is ground truth
// that the package compiles, so the claim is a false positive — the canonical
// case being the model not recognizing a recent Go language feature (e.g. Go
// 1.26's new(expr)). It is deliberately conservative: it only ever DROPS on a
// clean build; a failing or unrunnable build leaves the finding untouched, so
// environmental problems (missing deps, offline proxy) never suppress or confirm
// a finding. Packages are built at most once (cached), and only for findings
// that actually claim a build failure — a review with no such claims runs zero
// builds.
func (e *Engine) verifyBuildClaims(ctx context.Context, workDir string, findings []ValidatedFinding) []ValidatedFinding {
	if workDir == "" || len(findings) == 0 {
		return findings
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		return findings // no toolchain → cannot verify
	}
	if _, err := os.Stat(filepath.Join(workDir, "go.mod")); err != nil {
		return findings // not a module rooted at the worktree → skip
	}

	compiles := map[string]bool{}
	check := func(pkgDir string) bool {
		if v, ok := compiles[pkgDir]; ok {
			return v
		}
		v := packageCompiles(ctx, goBin, workDir, pkgDir)
		compiles[pkgDir] = v
		return v
	}

	out := make([]ValidatedFinding, 0, len(findings))
	for _, f := range findings {
		if strings.HasSuffix(f.FilePath, ".go") && claimsCompileFailure(f) {
			if pkgDir := path.Dir(f.FilePath); check(pkgDir) {
				e.log.Warn("build-verify: dropping finding — package compiles, contradicting its build-failure claim",
					"file", f.FilePath, "package", pkgDir, "title", f.Title, "severity", f.Severity)
				continue
			}
		}
		out = append(out, f)
	}
	return out
}

// packageCompiles runs `go build` for a single package and reports success. It
// writes output to os.DevNull so nothing lands in the read-only worktree, and
// uses -mod=readonly so go.mod/go.sum are never rewritten.
func packageCompiles(ctx context.Context, goBin, workDir, pkgDir string) bool {
	abs := filepath.Join(workDir, filepath.FromSlash(pkgDir))
	if info, err := os.Stat(abs); err != nil || !info.IsDir() {
		return false // can't locate the package → don't drop
	}
	ctx, cancel := context.WithTimeout(ctx, buildVerifyTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, goBin, "build", "-mod=readonly", "-o", os.DevNull, "./"+pkgDir)
	cmd.Dir = workDir
	cmd.Env = os.Environ()
	return cmd.Run() == nil
}
