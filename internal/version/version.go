// Package version holds build-time version metadata, overridable via -ldflags.
package version

// Build metadata. Override with:
//
//	-ldflags "-X github.com/sxwebdev/ai-reviewer/internal/version.Version=1.2.3"
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// String returns a human-readable version string.
func String() string {
	return Version + " (" + Commit + ", " + Date + ")"
}
