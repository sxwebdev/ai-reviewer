// Package index builds a local file index (with FTS5 content) of a repository
// worktree, used for relevance retrieval during review.
package index

import (
	"bytes"
	"path"
	"strings"

	"github.com/sxwebdev/ai-reviewer/internal/toolchain"
)

// languageByExt maps file extensions to a language label.
var languageByExt = map[string]string{
	".go": "go", ".py": "python", ".js": "javascript", ".ts": "typescript",
	".tsx": "typescript", ".jsx": "javascript", ".java": "java", ".rb": "ruby",
	".rs": "rust", ".c": "c", ".h": "c", ".cc": "cpp", ".cpp": "cpp", ".hpp": "cpp",
	".cs": "csharp", ".php": "php", ".kt": "kotlin", ".swift": "swift", ".scala": "scala",
	".sh": "shell", ".sql": "sql", ".proto": "protobuf", ".yaml": "yaml", ".yml": "yaml",
	".json": "json", ".toml": "toml", ".md": "markdown", ".html": "html", ".css": "css",
}

func languageFor(rel string) string {
	if l, ok := languageByExt[strings.ToLower(path.Ext(rel))]; ok {
		return l
	}
	return ""
}

// vendorPrefixes and suffixes mark generated/vendored paths.
var vendorPrefixes = []string{"vendor/", "node_modules/", "dist/", "build/", "third_party/", ".git/"}

func isVendorPath(rel string) bool {
	for _, p := range vendorPrefixes {
		if strings.HasPrefix(rel, p) {
			return true
		}
	}
	return false
}

// isTestPath delegates to the shared classifier so the index and the risk
// builder never disagree about what counts as a test file.
func isTestPath(rel string) bool { return toolchain.IsTestPath(rel) }

// generatedMarker matches the conventional Go generated-file header.
var generatedMarker = []byte("Code generated")

func isGenerated(rel string, content []byte) bool {
	if strings.HasSuffix(rel, ".pb.go") || strings.HasSuffix(rel, ".min.js") ||
		strings.Contains(rel, ".generated.") {
		return true
	}
	// Only scan the first ~1KB for the marker.
	head := content
	if len(head) > 1024 {
		head = head[:1024]
	}
	return bytes.Contains(head, generatedMarker)
}

// looksBinary delegates to the shared classifier.
func looksBinary(content []byte) bool { return toolchain.LooksBinary(content) }

// ignored delegates to the shared glob matcher (supports "dir/**", "**/x/**",
// "**/*.ext", and base-name patterns) so index ignore globs and risk
// sensitive globs share one dialect.
func ignored(rel string, globs []string) bool { return toolchain.MatchGlob(rel, globs) }
