package coverage

import (
	"context"
	"log/slog"
	"path"
	"path/filepath"

	"github.com/sxwebdev/ai-reviewer/internal/toolchain"
)

// BuiltinProviders instantiates the named providers. Unknown names are
// skipped with a warning, never fatal (mirrors review.BuiltinVerifiers).
func BuiltinProviders(names []string, run Runner, opts Options, log *slog.Logger) []Provider {
	if run == nil {
		run = ExecRunner
	}
	var out []Provider
	for _, n := range names {
		switch n {
		case "go":
			out = append(out, NewGoProvider(run, log))
		case "node":
			out = append(out, NewNodeProvider(run, opts.NodeInstall, log))
		default:
			log.Warn("unknown coverage provider skipped", "provider", n)
		}
	}
	return out
}

// Collect groups changed files by (provider, nearest project root), runs each
// detected provider once per root, and merges the profiles rekeyed to
// repo-relative paths. One root's failure or timeout becomes a SkipNote and
// never fails the collection.
func Collect(ctx context.Context, workDir string, changedFiles []string, providers []Provider, opts Options, log *slog.Logger) (Profile, []SkipNote, []string) {
	opts = opts.withDefaults()
	merged := Profile{}
	var skips []SkipNote
	var notes []string
	claimed := map[string]bool{} // files matched to some provider root

	for _, prov := range providers {
		byRoot := toolchain.GroupByRoot(workDir, changedFiles, prov.Markers())
		for root, files := range byRoot {
			if root == "" {
				continue // no project root for this provider
			}
			// Only files this provider can actually measure count as claimed —
			// a .ts file under a go.mod root must still surface as unmeasured,
			// and a root with no measurable changes must not run its tests.
			measurable := 0
			for _, f := range files {
				if prov.Covers(f) {
					claimed[f] = true
					measurable++
				}
			}
			if measurable == 0 {
				continue
			}
			absRoot := filepath.Join(workDir, filepath.FromSlash(root))
			if !prov.Detect(absRoot) {
				skips = append(skips, SkipNote{Root: root, Provider: prov.Name(), Reason: "toolchain not detected"})
				continue
			}
			runCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
			profile, note, err := prov.Run(runCtx, absRoot)
			timedOut := runCtx.Err() == context.DeadlineExceeded
			cancel()
			if err != nil {
				reason := err.Error()
				if timedOut {
					reason = "timed out after " + opts.Timeout.String()
				}
				log.Warn("coverage run failed", "provider", prov.Name(), "root", root, "reason", reason)
				skips = append(skips, SkipNote{Root: root, Provider: prov.Name(), Reason: reason})
				continue
			}
			if note != "" {
				notes = append(notes, prov.Name()+" ("+root+"): "+note)
			}
			for rel, fp := range profile {
				merged[filepath.ToSlash(path.Join(root, rel))] = fp
			}
		}
	}

	for _, f := range changedFiles {
		if !claimed[f] {
			skips = append(skips, SkipNote{Root: path.Dir(f), Reason: "no coverage provider for " + f})
		}
	}
	return merged, skips, notes
}
