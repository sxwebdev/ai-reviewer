package review

import (
	"context"
	"log/slog"
	"strings"
	"time"
)

const testVerifyTimeout = 180 * time.Second

// goTestVerifier runs the affected package's tests and annotates findings when
// the package is already failing at head. It is annotate-only: a green suite
// does not refute a logic claim (the bug may simply be untested), and a red
// suite does not confirm one. Opt-in via config (verifiers: [..., go_test])
// because it executes repository code — the same trust level as agent-mode git
// commands over the user's own MR, but still an explicit choice.
// Monorepo-aware: the module root is resolved per finding.
type goTestVerifier struct {
	log        *slog.Logger
	testFailed map[string]bool // root\x00pkgDir -> tests failed
}

func newGoTestVerifier(log *slog.Logger) *goTestVerifier {
	return &goTestVerifier{log: log, testFailed: map[string]bool{}}
}

func (v *goTestVerifier) Name() string { return "go_test" }

func (v *goTestVerifier) Applies(f ValidatedFinding) bool {
	return strings.HasSuffix(f.FilePath, ".go")
}

func (v *goTestVerifier) Verify(ctx context.Context, workDir string, f ValidatedFinding) VerifierResult {
	goBin, rootRel, ok := goToolchainFor(workDir, f.FilePath)
	if !ok {
		return VerifierResult{Verdict: VerdictKeep}
	}
	pkgDir := goPkgDirWithin(rootRel, f.FilePath)

	key := rootRel + "\x00" + pkgDir
	failed, cached := v.testFailed[key]
	if !cached {
		_, passed := runGoCmd(ctx, testVerifyTimeout, goBin, workDir, rootRel,
			"test", "-mod=readonly", "-count=1", "./"+pkgDir)
		failed = !passed
		v.testFailed[key] = failed
	}
	if failed {
		return VerifierResult{Verdict: VerdictAnnotate, Note: "package tests already failing at head"}
	}
	return VerifierResult{Verdict: VerdictKeep}
}
