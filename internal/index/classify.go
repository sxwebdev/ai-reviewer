// Package index builds a local file index (with FTS5 content) of a repository
// worktree, used for relevance retrieval during review.
package index

import (
	"bytes"
	"path"
	"strings"
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

func isTestPath(rel string) bool {
	base := path.Base(rel)
	return strings.HasSuffix(base, "_test.go") ||
		strings.HasSuffix(base, ".test.js") || strings.HasSuffix(base, ".test.ts") ||
		strings.HasSuffix(base, ".spec.js") || strings.HasSuffix(base, ".spec.ts") ||
		strings.HasPrefix(base, "test_") // python
}

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

// looksBinary reports whether content appears to be binary (has a NUL byte).
func looksBinary(content []byte) bool {
	head := content
	if len(head) > 8000 {
		head = head[:8000]
	}
	return bytes.IndexByte(head, 0) >= 0
}

// ignored reports whether rel matches any ignore glob (supporting "dir/**").
func ignored(rel string, globs []string) bool {
	base := path.Base(rel)
	for _, g := range globs {
		if strings.HasSuffix(g, "/**") {
			prefix := strings.TrimSuffix(g, "/**")
			if rel == prefix || strings.HasPrefix(rel, prefix+"/") {
				return true
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
