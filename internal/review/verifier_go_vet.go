package review

import (
	"context"
	"log/slog"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const vetVerifyTimeout = 120 * time.Second

// vetDiagPathRe extracts the file path from vet diagnostics ("pkg/f.go:12:2: …").
var vetDiagPathRe = regexp.MustCompile(`([^\s:]+\.go):\d+`)

// goVetVerifier corroborates correctness/concurrency findings with `go vet`.
// It is annotate/confirm-only and NEVER drops: vet misses most real bugs, so
// its silence is not a refutation — but when vet flags the finding's exact
// file, the note tells the human reviewer a deterministic tool agrees
// something is off. Monorepo-aware: the module root is resolved per finding.
type goVetVerifier struct {
	log      *slog.Logger
	vetFiles map[string]map[string]bool // root\x00pkgDir -> root-relative files with diagnostics
}

func newGoVetVerifier(log *slog.Logger) *goVetVerifier {
	return &goVetVerifier{log: log, vetFiles: map[string]map[string]bool{}}
}

func (v *goVetVerifier) Name() string { return "go_vet" }

func (v *goVetVerifier) Applies(f ValidatedFinding) bool {
	if !strings.HasSuffix(f.FilePath, ".go") {
		return false
	}
	switch f.Category {
	case "correctness", "concurrency":
		return true
	}
	return false
}

func (v *goVetVerifier) Verify(ctx context.Context, workDir string, f ValidatedFinding) VerifierResult {
	goBin, rootRel, ok := goToolchainFor(workDir, f.FilePath)
	if !ok {
		return VerifierResult{Verdict: VerdictKeep}
	}
	pkgDir := goPkgDirWithin(rootRel, f.FilePath)

	key := rootRel + "\x00" + pkgDir
	files, cached := v.vetFiles[key]
	if !cached {
		files = runVet(ctx, goBin, workDir, rootRel, pkgDir)
		v.vetFiles[key] = files
	}
	// Exact root-relative path match — a diagnostic in http_client.go must not
	// corroborate a finding about client.go via substring coincidence.
	fileRel := f.FilePath
	if rootRel != "." {
		fileRel = strings.TrimPrefix(f.FilePath, rootRel+"/")
	}
	if files[fileRel] {
		return VerifierResult{Verdict: VerdictAnnotate, Note: "go vet also reports issues in this file"}
	}
	return VerifierResult{Verdict: VerdictKeep}
}

// runVet returns the set of root-relative files go vet flagged for one
// package (empty when clean or unrunnable — both are no-signal).
func runVet(ctx context.Context, goBin, workDir, rootRel, pkgDir string) map[string]bool {
	out, ok := runGoCmd(ctx, vetVerifyTimeout, goBin, workDir, rootRel, "vet", "-mod=readonly", "./"+pkgDir)
	if ok {
		return nil // clean
	}
	files := map[string]bool{}
	absRoot := filepath.Join(workDir, filepath.FromSlash(rootRel))
	for _, m := range vetDiagPathRe.FindAllStringSubmatch(string(out), -1) {
		p := m[1]
		if filepath.IsAbs(p) {
			rel, err := filepath.Rel(absRoot, p)
			if err != nil || strings.HasPrefix(rel, "..") {
				continue
			}
			p = filepath.ToSlash(rel)
		}
		files[strings.TrimPrefix(filepath.ToSlash(p), "./")] = true
	}
	return files
}
