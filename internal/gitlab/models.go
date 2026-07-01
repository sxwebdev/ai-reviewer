// Package gitlab is a hand-written client for the GitLab API v4, plus the API
// data models. It has no dependencies on higher-level packages so that the
// review engine can depend on it without an import cycle.
package gitlab

// DiffRefs are the three SHAs required to anchor an inline comment position.
// They come from an MR's diff_refs / versions.
type DiffRefs struct {
	BaseSHA  string `json:"base_sha"`
	HeadSHA  string `json:"head_sha"`
	StartSHA string `json:"start_sha"`
}

// Position describes where an inline note attaches in a diff. Go owns position
// construction; the LLM never supplies these fields directly.
//
// GitLab rules (verified against docs.gitlab.com):
//   - added line   -> set NewLine only (OldLine nil)
//   - removed line -> set OldLine only (NewLine nil)
//   - context line -> set BOTH OldLine and NewLine
//
// OldPath/NewPath are always set; PositionType is "text".
type Position struct {
	BaseSHA      string `json:"base_sha"`
	HeadSHA      string `json:"head_sha"`
	StartSHA     string `json:"start_sha"`
	PositionType string `json:"position_type"`
	OldPath      string `json:"old_path"`
	NewPath      string `json:"new_path"`
	OldLine      *int   `json:"old_line,omitempty"`
	NewLine      *int   `json:"new_line,omitempty"`
}
