// Package coverage measures test coverage of an MR's changed lines: it runs
// the repository's own test toolchains (opt-in — this executes repository
// code), collects per-line coverage profiles, and intersects them with the
// diff's added lines. The result is a fact — "these added lines are not
// executed by any test" — replacing LLM guesses about missing tests.
//
// The package depends only on internal/toolchain and the standard library;
// callers hand it plain data (changed files, added line numbers) so it never
// imports review/state/gitlab.
package coverage

import (
	"context"
	"os"
	"os/exec"
	"sort"
	"time"
)

// FileProfile is per-line coverage for one file. A line PRESENT in Hits is an
// instrumentable/executable line (the "universe"); Hits[line] > 0 means
// covered. Added lines absent from the universe (comments, declarations,
// blank lines) are excluded from percentages entirely.
type FileProfile struct {
	Hits map[int]int
}

// Profile maps repo-relative file paths to their line coverage.
type Profile map[string]*FileProfile

// Runner abstracts command execution so provider tests never need real
// toolchains. dir is the working directory; env entries are appended to the
// inherited environment.
type Runner func(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error)

// ExecRunner is the production Runner.
func ExecRunner(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	return cmd.CombinedOutput()
}

// Provider runs one toolchain's tests with coverage under a project root
// (absolute path) and returns a profile with paths relative to that root.
type Provider interface {
	Name() string
	Markers() []string       // project-root marker files (for grouping)
	Detect(root string) bool // cheap: config present, toolchain available
	Covers(path string) bool // whether this provider can measure the file at all
	Run(ctx context.Context, root string) (Profile, string, error)
	// Run's string is an optional note (e.g. "tests failed; partial profile used").
}

// Options bounds a coverage collection.
type Options struct {
	Timeout     time.Duration // per provider Run (default 5m)
	NodeInstall bool          // allow dependency install for node roots
}

func (o Options) withDefaults() Options {
	if o.Timeout <= 0 {
		o.Timeout = 5 * time.Minute
	}
	return o
}

// FileCoverage is the changed-line coverage of one file.
type FileCoverage struct {
	Path      string `json:"path"`
	Added     int    `json:"added"`   // added lines present in the profile universe
	Covered   int    `json:"covered"` // of those, executed by tests
	Uncovered []int  `json:"uncovered,omitempty"`
}

// SkipNote explains why a root/file was not measured.
type SkipNote struct {
	Root     string `json:"root"`
	Provider string `json:"provider,omitempty"`
	Reason   string `json:"reason"`
}

// Report is the changed-line coverage result.
type Report struct {
	Files        []FileCoverage `json:"files"`
	TotalAdded   int            `json:"total_added"`
	TotalCovered int            `json:"total_covered"`
	Pct          float64        `json:"pct"` // over measured files only
	Skipped      []SkipNote     `json:"skipped,omitempty"`
	Notes        []string       `json:"notes,omitempty"`
}

// Intersect computes changed-line coverage: for each file with a profile, the
// added lines that exist in the profile's universe form the denominator; hits
// among them the numerator. Files without any profile are NOT reported as 0% —
// the caller records them as skipped, which is a load-bearing distinction.
// Pure.
func Intersect(p Profile, addedLines map[string][]int) *Report {
	r := &Report{}
	paths := make([]string, 0, len(addedLines))
	for path := range addedLines {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	for _, path := range paths {
		fp, ok := p[path]
		if !ok || len(fp.Hits) == 0 {
			continue // unmeasured — caller's skip notes explain why
		}
		fc := FileCoverage{Path: path}
		for _, line := range addedLines[path] {
			hits, inUniverse := fp.Hits[line]
			if !inUniverse {
				continue // comment/decl/blank — not executable
			}
			fc.Added++
			if hits > 0 {
				fc.Covered++
			} else {
				fc.Uncovered = append(fc.Uncovered, line)
			}
		}
		if fc.Added == 0 {
			continue // nothing executable added in this file
		}
		sort.Ints(fc.Uncovered)
		r.Files = append(r.Files, fc)
		r.TotalAdded += fc.Added
		r.TotalCovered += fc.Covered
	}
	if r.TotalAdded > 0 {
		r.Pct = 100 * float64(r.TotalCovered) / float64(r.TotalAdded)
	}
	return r
}
