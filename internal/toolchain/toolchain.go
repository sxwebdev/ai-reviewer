// Package toolchain provides language/toolchain project-root discovery and
// path classification shared by coverage providers, review verifiers, and the
// risk builder. Stdlib-only by design: review and coverage both import it, so
// it must never grow dependencies on state/gitlab/llm.
package toolchain

import (
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// Project-root marker files per toolchain.
var (
	GoMarkers   = []string{"go.mod"}
	NodeMarkers = []string{"package.json"}
	PyMarkers   = []string{"pyproject.toml", "setup.cfg", "setup.py"}
)

// NearestRoot walks up from filePath's directory to workDir looking for any of
// the marker files, returning the root directory relative to workDir ("." for
// the top level) and whether one was found. filePath is repo-relative
// (slash-separated); monorepos resolve to the closest enclosing root.
func NearestRoot(workDir, filePath string, markers []string) (string, bool) {
	dir := path.Dir(filePath)
	for {
		for _, m := range markers {
			abs := filepath.Join(workDir, filepath.FromSlash(dir), m)
			if info, err := os.Stat(abs); err == nil && !info.IsDir() {
				return dir, true
			}
		}
		if dir == "." || dir == "/" {
			return "", false
		}
		dir = path.Dir(dir)
	}
}

// GroupByRoot buckets repo-relative files by their nearest project root. Files
// with no root land in the "" bucket.
func GroupByRoot(workDir string, files []string, markers []string) map[string][]string {
	out := map[string][]string{}
	for _, f := range files {
		root, ok := NearestRoot(workDir, f, markers)
		if !ok {
			out[""] = append(out[""], f)
			continue
		}
		out[root] = append(out[root], f)
	}
	return out
}

// IsTestPath reports whether a repo-relative path is a test file
// (multi-language; mirrors internal/index's classification).
func IsTestPath(rel string) bool {
	base := path.Base(rel)
	return strings.HasSuffix(base, "_test.go") ||
		strings.HasSuffix(base, ".test.js") || strings.HasSuffix(base, ".test.ts") ||
		strings.HasSuffix(base, ".test.jsx") || strings.HasSuffix(base, ".test.tsx") ||
		strings.HasSuffix(base, ".spec.js") || strings.HasSuffix(base, ".spec.ts") ||
		strings.HasSuffix(base, ".spec.jsx") || strings.HasSuffix(base, ".spec.tsx") ||
		strings.HasPrefix(base, "test_") || // python
		strings.Contains(rel, "/__tests__/")
}

// sourceExts are extensions of files that carry executable behaviour (code,
// migrations, API contracts, scripts) — the set the risk scorer treats as
// "source" when deciding whether behaviour changed without tests.
var sourceExts = map[string]bool{
	".go": true, ".js": true, ".jsx": true, ".ts": true, ".tsx": true,
	".mjs": true, ".cjs": true, ".mts": true, ".cts": true, ".vue": true, ".svelte": true,
	".py": true, ".rb": true, ".php": true, ".java": true, ".kt": true, ".kts": true,
	".rs": true, ".c": true, ".h": true, ".cc": true, ".cpp": true, ".hpp": true,
	".cs": true, ".swift": true, ".scala": true, ".ex": true, ".exs": true,
	".sql": true, ".proto": true, ".sh": true, ".bash": true,
}

// IsSourceFile reports whether a repo-relative path looks like executable
// source code (as opposed to docs, config, or data).
func IsSourceFile(rel string) bool {
	return sourceExts[strings.ToLower(path.Ext(rel))]
}

// LooksBinary reports whether content appears to be binary (a NUL byte within
// the head). Shared by the indexer and the file-context builder so both
// classify the same file identically.
func LooksBinary(content []byte) bool {
	head := content
	if len(head) > 8000 {
		head = head[:8000]
	}
	return slices.Contains(head, 0)
}

// MatchGlob reports whether rel matches any glob, supporting the "dir/**"
// prefix form and base-name matches (mirrors internal/index's ignored).
func MatchGlob(rel string, globs []string) bool {
	base := path.Base(rel)
	for _, g := range globs {
		if suffix, ok := strings.CutSuffix(g, "/**"); ok {
			prefix := strings.TrimPrefix(suffix, "**/")
			if g[0] == '*' { // "**/dir/**": match anywhere in the path
				if strings.Contains(rel, "/"+prefix+"/") || strings.HasPrefix(rel, prefix+"/") {
					return true
				}
				continue
			}
			if rel == suffix || strings.HasPrefix(rel, suffix+"/") {
				return true
			}
			continue
		}
		if pat, anyDepth := strings.CutPrefix(g, "**/"); anyDepth {
			// "**/dir/*.ext" matches at any depth, including zero: try the
			// stripped pattern against rel and every path suffix of rel.
			rest := rel
			for {
				if ok, _ := path.Match(pat, rest); ok {
					return true
				}
				i := strings.IndexByte(rest, '/')
				if i < 0 {
					break
				}
				rest = rest[i+1:]
			}
			continue
		}
		if ok, _ := path.Match(g, rel); ok {
			return true
		}
		if ok, _ := path.Match(g, base); ok {
			return true
		}
	}
	return false
}
