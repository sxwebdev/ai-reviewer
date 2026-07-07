package review

import (
	"context"
	"log/slog"
)

// VerifierVerdict is a deterministic verifier's judgement of one finding.
type VerifierVerdict int

const (
	// VerdictKeep means the verifier has no signal — the finding is untouched.
	VerdictKeep VerifierVerdict = iota
	// VerdictDrop means ground truth definitively refutes the finding.
	VerdictDrop
	// VerdictAnnotate keeps the finding and attaches a supporting note.
	VerdictAnnotate
)

// VerifierResult is a verdict plus an optional human-readable note.
type VerifierResult struct {
	Verdict VerifierVerdict
	Note    string
}

// Verifier is a deterministic, tool-backed check that can refute or support a
// finding using the read-only worktree (e.g. `go build` refuting a "does not
// compile" claim). Implementations must be conservative: VerdictDrop only on
// definitive refutation; any environmental failure (missing toolchain, broken
// deps) must yield VerdictKeep so a flaky environment never suppresses a real
// finding. Language dispatch lives in Applies, so the pipeline itself stays
// language-agnostic — other languages are new implementations plus a config
// name.
type Verifier interface {
	Name() string
	Applies(f ValidatedFinding) bool
	Verify(ctx context.Context, workDir string, f ValidatedFinding) VerifierResult
}

// BuiltinVerifiers instantiates the named verifiers (fresh per review so their
// per-package caches have review lifetime). Unknown names are skipped with a
// warning.
func BuiltinVerifiers(names []string, log *slog.Logger) []Verifier {
	var out []Verifier
	for _, n := range names {
		switch n {
		case "go_build":
			out = append(out, newGoBuildVerifier(log))
		case "go_vet":
			out = append(out, newGoVetVerifier(log))
		case "go_test":
			out = append(out, newGoTestVerifier(log))
		case "tsc":
			out = append(out, newTSCVerifier(log))
		case "py_syntax":
			out = append(out, newPySyntaxVerifier(log))
		default:
			log.Warn("unknown verifier skipped", "verifier", n)
		}
	}
	return out
}

// runVerifiers applies each verifier to each finding it covers. A VerdictDrop
// removes the finding immediately; VerdictAnnotate appends the note to the
// finding's validation error trail.
func runVerifiers(ctx context.Context, workDir string, vs []Verifier, findings []ValidatedFinding, log *slog.Logger) ([]ValidatedFinding, []SuppressedFinding) {
	if workDir == "" || len(vs) == 0 || len(findings) == 0 {
		return findings, nil
	}
	out := make([]ValidatedFinding, 0, len(findings))
	var suppressed []SuppressedFinding
	for _, f := range findings {
		dropped := false
		for _, v := range vs {
			if !v.Applies(f) {
				continue
			}
			res := v.Verify(ctx, workDir, f)
			switch res.Verdict {
			case VerdictDrop:
				log.Warn("verifier refuted finding",
					"verifier", v.Name(), "file", f.FilePath, "title", f.Title, "note", res.Note)
				reason := v.Name() + " refuted it"
				if res.Note != "" {
					reason += ": " + res.Note
				}
				suppressed = append(suppressed, suppressedFromValidated(f, SuppressVerifier, reason))
				dropped = true
			case VerdictAnnotate:
				f.ValidationError = appendNote(f.ValidationError, v.Name()+": "+res.Note)
			}
			if dropped {
				break
			}
		}
		if !dropped {
			out = append(out, f)
		}
	}
	return out, suppressed
}
