package review

import (
	"context"
	"log/slog"
	"os"
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

// goBuildVerifier drops findings that assert a Go compile/build failure when
// the affected package actually builds. A successful build is ground truth
// that the claimed code compiles, so the claim is a false positive — the
// canonical case being the model not recognizing a recent Go language feature
// (e.g. Go 1.26's new(expr)). It is deliberately conservative: it only ever
// DROPS on a clean build; a failing or unrunnable build leaves the finding
// untouched, so environmental problems never suppress a finding. Two blind
// spots are guarded explicitly: _test.go files are compiled via `go test -c`
// (`go build` never compiles them, so its success proves nothing about them),
// and files under build constraints are always kept (the local build may
// exclude them entirely). Monorepo-aware: the module root is resolved per
// finding. Packages are built at most once per (root, pkg, test) — cached.
type goBuildVerifier struct {
	log      *slog.Logger
	compiles map[string]bool
}

func newGoBuildVerifier(log *slog.Logger) *goBuildVerifier {
	return &goBuildVerifier{log: log, compiles: map[string]bool{}}
}

func (v *goBuildVerifier) Name() string { return "go_build" }

func (v *goBuildVerifier) Applies(f ValidatedFinding) bool {
	return strings.HasSuffix(f.FilePath, ".go") && claimsCompileFailure(f)
}

func (v *goBuildVerifier) Verify(ctx context.Context, workDir string, f ValidatedFinding) VerifierResult {
	goBin, rootRel, ok := goToolchainFor(workDir, f.FilePath)
	if !ok {
		return VerifierResult{Verdict: VerdictKeep} // no toolchain / no module root
	}
	if hasBuildConstraint(workDir, f.FilePath) {
		return VerifierResult{Verdict: VerdictKeep} // may be excluded from this build
	}
	pkgDir := goPkgDirWithin(rootRel, f.FilePath)
	isTest := strings.HasSuffix(f.FilePath, "_test.go")

	key := rootRel + "\x00" + pkgDir + "\x00" + map[bool]string{true: "test", false: "build"}[isTest]
	compiled, cached := v.compiles[key]
	if !cached {
		args := []string{"build", "-mod=readonly", "-o", os.DevNull, "./" + pkgDir}
		if isTest {
			// Compile (not run) the package's tests — the only ground truth
			// for a _test.go compile claim.
			args = []string{"test", "-mod=readonly", "-c", "-o", os.DevNull, "./" + pkgDir}
		}
		_, compiled = runGoCmd(ctx, buildVerifyTimeout, goBin, workDir, rootRel, args...)
		v.compiles[key] = compiled
	}
	if compiled {
		return VerifierResult{Verdict: VerdictDrop, Note: "package builds cleanly, contradicting the build-failure claim"}
	}
	return VerifierResult{Verdict: VerdictKeep}
}
