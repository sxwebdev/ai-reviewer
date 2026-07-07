package review

import (
	"fmt"
	"strings"
)

// ContextBudget bounds how much enrichment context (beyond the diffs) may be
// added to the review prompt. Sizes are in bytes of rendered prompt text.
type ContextBudget struct {
	IncludeFullFiles bool // include changed-file content sections
	MaxFileLines     int  // per-file cap; longer files fall back to hunk windows
	HunkWindowLines  int  // context lines around each hunk when windowing
	MaxTotalBytes    int  // total budget across all enrichment sections

	IncludeCommits     bool // include the MR's commit messages
	IncludeDiscussions bool // include existing discussion content (not just the count)
	MaxDiscussionBytes int  // budget for the rendered discussions section

	IncludePriorReview bool // include the previous review + interdiff on re-review
	MaxInterdiffBytes  int  // budget for the interdiff section

	MaxRelatedFiles int // FTS-suggested related files listed for investigation (0 = off)
}

// DefaultContextBudget returns the default enrichment budget.
func DefaultContextBudget() ContextBudget {
	return ContextBudget{
		IncludeFullFiles:   true,
		MaxFileLines:       500,
		HunkWindowLines:    60,
		MaxTotalBytes:      256 << 10,
		IncludeCommits:     true,
		IncludeDiscussions: true,
		MaxDiscussionBytes: 4 << 10,
		IncludePriorReview: true,
		MaxInterdiffBytes:  32 << 10,
		MaxRelatedFiles:    5,
	}
}

// RelatedFile is a likely-related repository file suggested to the model as an
// investigation lead (agent mode).
type RelatedFile struct {
	Path   string
	Reason string
}

// CommitInfo is one MR commit shown to the model (intent context the MR
// description often lacks).
type CommitInfo struct {
	ShortSHA string
	Title    string
	Message  string // truncated body beyond the title; may be empty
}

// DiscussionNote is one existing MR discussion note shown to the model so it
// can avoid re-raising discussed topics and build on unresolved ones.
type DiscussionNote struct {
	Author   string
	Body     string // truncated
	FilePath string // inline notes only
	Line     int    // inline notes only
	Resolved bool
	OwnBot   bool // authored by this tool's reviewer identity
}

// PriorFinding is one finding from the previous review of this MR, with its
// human disposition, so a re-review does not re-raise settled topics.
type PriorFinding struct {
	Title           string
	FilePath        string
	Line            int
	Severity        string
	Status          string // proposed|approved|rejected|drafted|published|failed
	RejectionReason string
}

// PriorReview is the previous review of this MR (at an older head SHA) plus
// the interdiff between that head and the current one.
type PriorReview struct {
	HeadSHA   string
	Summary   string
	Findings  []PriorFinding
	Interdiff string // git diff prevHead..newHead, truncated; may be empty
}

// FileSegment is a contiguous slice of a file's content starting at StartLine
// (1-based).
type FileSegment struct {
	StartLine int
	Lines     []string
}

// FileContext is the (possibly windowed) content of one changed file included
// in the prompt so the model sees code surrounding the hunks.
type FileContext struct {
	Path      string
	Truncated bool // segments are windows around hunks, not the whole file
	Segments  []FileSegment

	// Rendered caches the line-numbered rendering: the budget check in the
	// service and the prompt builder share one render instead of re-rendering
	// hundreds of KB per pass.
	Rendered string
}

// BuildFileContext segments content for prompting: the whole file when it has
// at most maxFileLines lines, otherwise merged windows of windowLines context
// around each hunk's new-side range. Returns nil when nothing useful remains
// (e.g. empty content).
func BuildFileContext(f *FileDiff, content string, maxFileLines, windowLines int) *FileContext {
	path := f.Path()
	lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	if len(lines) == 0 || (len(lines) == 1 && lines[0] == "") {
		return nil
	}
	if len(lines) <= maxFileLines {
		return &FileContext{Path: path, Segments: []FileSegment{{StartLine: 1, Lines: lines}}}
	}

	spans := hunkWindows(f.Hunks, windowLines, len(lines))
	if len(spans) == 0 {
		return nil
	}
	fc := &FileContext{Path: path, Truncated: true}
	for _, sp := range spans {
		fc.Segments = append(fc.Segments, FileSegment{StartLine: sp[0], Lines: lines[sp[0]-1 : sp[1]]})
	}
	return fc
}

// hunkWindows returns merged, sorted 1-based inclusive [start,end] spans of
// windowLines context around each hunk's new-side range, clamped to fileLines.
func hunkWindows(hunks []Hunk, windowLines, fileLines int) [][2]int {
	var spans [][2]int
	for _, h := range hunks {
		start := h.NewStart - windowLines
		end := h.NewStart + max(h.NewCount, 1) - 1 + windowLines
		if start < 1 {
			start = 1
		}
		if end > fileLines {
			end = fileLines
		}
		if start > end {
			continue
		}
		spans = append(spans, [2]int{start, end})
	}
	// Hunks are ordered; merge overlapping/adjacent spans.
	var merged [][2]int
	for _, sp := range spans {
		if n := len(merged); n > 0 && sp[0] <= merged[n-1][1]+1 {
			if sp[1] > merged[n-1][1] {
				merged[n-1][1] = sp[1]
			}
			continue
		}
		merged = append(merged, sp)
	}
	return merged
}

// RenderFileContext renders a FileContext with 1-based line numbers matching
// the annotated-diff numbering.
func RenderFileContext(fc FileContext) string {
	var b strings.Builder
	for i, seg := range fc.Segments {
		if i > 0 {
			b.WriteString("     ⋮\n")
		}
		for j, line := range seg.Lines {
			fmt.Fprintf(&b, "%6d | %s\n", seg.StartLine+j, line)
		}
	}
	return b.String()
}
