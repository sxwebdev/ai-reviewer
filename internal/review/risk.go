package review

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// RiskInput is the raw, pre-collected data for deterministic risk scoring.
// The service assembles it (git-history I/O lives there); scoring itself is
// pure so it stays table-testable.
type RiskInput struct {
	FilesChanged    int
	LinesAdded      int
	LinesRemoved    int
	ChurnByFile     map[string]int // commits touching each changed file (recent history)
	FixesByFile     map[string]int // fix/bug-pattern commits touching each changed file
	SensitiveHits   []string       // changed paths matching sensitive globs
	TestsTouched    bool           // any changed file is a test file
	BehaviorFiles   int            // changed non-test source files
	NewDependencies []string       // added/updated deps in manifest diffs
}

// RiskFactor is one scored contribution with a human-readable explanation.
type RiskFactor struct {
	Name   string  `json:"name"`
	Weight float64 `json:"weight"`
	Detail string  `json:"detail"`
}

// RiskReport is the deterministic risk assessment of an MR.
type RiskReport struct {
	Score   int          `json:"score"` // 0-100
	Level   string       `json:"level"` // low|medium|high|critical
	Factors []RiskFactor `json:"factors"`
}

// Factor thresholds/caps for ComputeRisk. Documented here as the single place
// to tune the formula:
//
//	diff_size        ≤25  (added+removed)/40 — 1000 changed lines saturates
//	diff_spread      ≤10  1 point per changed file
//	churn            ≤15  3 points per file changed in ≥5 recent commits
//	bugfix_history   ≤15  5 points per file with ≥2 fix-pattern commits
//	sensitive_paths  ≤15  5 points per sensitive path hit
//	no_tests          10  behaviour code changed but no test file touched
//	new_dependencies ≤10  5 points per added/updated dependency
//
// Levels: <25 low, <50 medium, <75 high, ≥75 critical.
const (
	churnHotThreshold  = 5
	fixesHotThreshold  = 2
	riskLevelMedium    = 25
	riskLevelHigh      = 50
	riskLevelCritical  = 75
	maxDetailPathsList = 5
)

// ComputeRisk scores an MR's risk from deterministic signals. Pure.
func ComputeRisk(in RiskInput) RiskReport {
	var factors []RiskFactor
	add := func(name string, weight float64, detail string) {
		if weight > 0 {
			factors = append(factors, RiskFactor{Name: name, Weight: weight, Detail: detail})
		}
	}

	changed := in.LinesAdded + in.LinesRemoved
	add("diff_size", min(25, float64(changed)/40),
		fmt.Sprintf("%d lines changed (+%d/-%d)", changed, in.LinesAdded, in.LinesRemoved))
	add("diff_spread", min(10, float64(in.FilesChanged)),
		fmt.Sprintf("%d files changed", in.FilesChanged))

	hotChurn := hotFiles(in.ChurnByFile, churnHotThreshold)
	add("churn", min(15, 3*float64(len(hotChurn))),
		fmt.Sprintf("frequently-changed files touched: %s", pathList(hotChurn, in.ChurnByFile, "commits")))

	bugMagnets := hotFiles(in.FixesByFile, fixesHotThreshold)
	add("bugfix_history", min(15, 5*float64(len(bugMagnets))),
		fmt.Sprintf("files with prior bug fixes touched: %s", pathList(bugMagnets, in.FixesByFile, "fixes")))

	add("sensitive_paths", min(15, 5*float64(len(in.SensitiveHits))),
		"sensitive paths changed: "+strings.Join(capList(in.SensitiveHits), ", "))

	if in.BehaviorFiles > 0 && !in.TestsTouched {
		add("no_tests", 10, fmt.Sprintf("%d source files changed but no test files touched", in.BehaviorFiles))
	}
	add("new_dependencies", min(10, 5*float64(len(in.NewDependencies))),
		"dependencies added/updated: "+strings.Join(capList(in.NewDependencies), ", "))

	score := 0.0
	for _, f := range factors {
		score += f.Weight
	}
	s := int(min(100, score))
	return RiskReport{Score: s, Level: riskLevel(s), Factors: factors}
}

func riskLevel(score int) string {
	switch {
	case score < riskLevelMedium:
		return "low"
	case score < riskLevelHigh:
		return "medium"
	case score < riskLevelCritical:
		return "high"
	default:
		return "critical"
	}
}

// hotFiles returns the files whose count meets the threshold, sorted.
func hotFiles(counts map[string]int, threshold int) []string {
	var out []string
	for f, n := range counts {
		if n >= threshold {
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out
}

// pathList renders "a.go (14 commits), b.go (7 commits)" capped at a few entries.
func pathList(paths []string, counts map[string]int, unit string) string {
	var parts []string
	for _, p := range capList(paths) {
		if strings.HasSuffix(p, "…") {
			parts = append(parts, p)
			continue
		}
		parts = append(parts, fmt.Sprintf("%s (%d %s)", p, counts[p], unit))
	}
	return strings.Join(parts, ", ")
}

func capList(items []string) []string {
	if len(items) <= maxDetailPathsList {
		return items
	}
	out := make([]string, maxDetailPathsList, maxDetailPathsList+1)
	copy(out, items[:maxDetailPathsList])
	return append(out, fmt.Sprintf("+%d more…", len(items)-maxDetailPathsList))
}

// Dependency-manifest heuristics: added lines that look like a dependency
// declaration. Diff-local by necessity (no full JSON/TOML context), so these
// are deliberately loose and reported as "added/updated".
var (
	goModDepRe   = regexp.MustCompile(`^\s*([\w./-]+\.[\w./-]+) v[\w.+-]+`)
	pkgJSONDepRe = regexp.MustCompile(`^\s*"([^"]+)"\s*:\s*"[~^]?\d[^"]*"`)
	// pkgJSONDepsBlockRe marks the opening of a dependencies-like block, so a
	// version-shaped value outside one (e.g. "version": "2.1.0") never counts.
	pkgJSONDepsBlockRe = regexp.MustCompile(`"(dev|peer|optional)?[Dd]ependencies"\s*:`)
	pyDepRe            = regexp.MustCompile(`^\s*([\w.\[\]-]+)\s*[=<>~!]=?`)
)

// DetectNewDependencies scans added manifest lines for new/updated deps. Pure.
// For package.json, entries count only inside a dependencies-like block seen
// in the same hunk — a hunk without the block header yields nothing (a
// deliberate false-negative; the alternative counted "version" bumps as deps).
func DetectNewDependencies(files []*FileDiff) []string {
	var out []string
	for _, f := range files {
		path := f.Path()
		base := path
		if i := strings.LastIndexByte(base, '/'); i >= 0 {
			base = base[i+1:]
		}
		var re *regexp.Regexp
		isPkgJSON := false
		switch {
		case base == "go.mod":
			re = goModDepRe
		case base == "package.json":
			re, isPkgJSON = pkgJSONDepRe, true
		case base == "pyproject.toml" || strings.HasPrefix(base, "requirements"):
			re = pyDepRe
		default:
			continue
		}
		for _, h := range f.Hunks {
			inDepsBlock := false
			for _, l := range h.Lines {
				if isPkgJSON {
					switch {
					case pkgJSONDepsBlockRe.MatchString(l.Content):
						inDepsBlock = true
						continue
					case strings.Contains(l.Content, "}"):
						inDepsBlock = false // block (or nesting) closed
					}
				}
				if l.Kind != LineAdded || (isPkgJSON && !inDepsBlock) {
					continue
				}
				if m := re.FindStringSubmatch(l.Content); m != nil {
					out = append(out, m[1])
				}
			}
		}
	}
	return out
}
