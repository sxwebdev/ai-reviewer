package review

import (
	"fmt"

	"github.com/sxwebdev/ai-reviewer/internal/gitlab"
)

// LineIntent is what the LLM supplies: a file, a line number, and which side of
// the diff it refers to. Go turns this into a valid GitLab position — the LLM
// never provides SHAs or the final position.
type LineIntent struct {
	FilePath string
	Line     int
	LineKind string // "new" | "old" | "context" | "file"
}

// MapKind is the outcome tier of position mapping.
type MapKind int

const (
	// MapExact means the intended line was found in the diff.
	MapExact MapKind = iota
	// MapSnapped means we anchored to the nearest changed line in the file.
	MapSnapped
	// MapOverview means no inline anchor is possible; use an overview note.
	MapOverview
)

// MapOutcome describes how a position was (or was not) produced.
type MapOutcome struct {
	Kind   MapKind
	Reason string
}

// FindFileDiff returns the FileDiff whose new or old path matches path.
func FindFileDiff(files []*FileDiff, path string) *FileDiff {
	for _, f := range files {
		if f.NewPath == path || f.OldPath == path {
			return f
		}
	}
	return nil
}

// MapPosition maps an intent onto a GitLab inline position for the given file
// diff, applying the fallback ladder: exact line → nearest changed line in the
// same file → overview note (nil position).
func MapPosition(fd *FileDiff, refs gitlab.DiffRefs, intent LineIntent) (*gitlab.Position, MapOutcome) {
	if fd == nil {
		return nil, MapOutcome{Kind: MapOverview, Reason: "file not in diff"}
	}
	// File-level or binary → overview.
	if intent.LineKind == "file" || fd.IsBinary || len(fd.Hunks) == 0 {
		return nil, MapOutcome{Kind: MapOverview, Reason: "no inline anchor for file"}
	}

	base := basePosition(fd, refs)

	switch intent.LineKind {
	case "old":
		if dl, ok := findLine(fd, LineRemoved, sideOld, intent.Line); ok {
			return withOld(base, dl.OldLine), MapOutcome{Kind: MapExact}
		}
	case "context":
		if dl, ok := findLine(fd, LineContext, sideNew, intent.Line); ok {
			return withBoth(base, dl.OldLine, dl.NewLine), MapOutcome{Kind: MapExact}
		}
		// The model may have used the old-side number for a context line.
		if dl, ok := findLine(fd, LineContext, sideOld, intent.Line); ok {
			return withBoth(base, dl.OldLine, dl.NewLine), MapOutcome{Kind: MapExact}
		}
		// Or it may have meant an added line at that number.
		if dl, ok := findLine(fd, LineAdded, sideNew, intent.Line); ok {
			return withNew(base, dl.NewLine), MapOutcome{Kind: MapExact}
		}
	default: // "new" and anything unspecified
		if dl, ok := findLine(fd, LineAdded, sideNew, intent.Line); ok {
			return withNew(base, dl.NewLine), MapOutcome{Kind: MapExact}
		}
	}

	// Fallback: snap to the nearest changed line, but only within a small
	// distance — a wildly wrong line number degrades to an overview note rather
	// than a confident inline comment on an unrelated hunk.
	if intent.LineKind == "old" {
		if dl, dist, ok := nearest(fd, LineRemoved, sideOld, intent.Line); ok && dist <= maxSnapDistance {
			return withOld(base, dl.OldLine), MapOutcome{Kind: MapSnapped, Reason: snapReason(dist)}
		}
	}
	if dl, dist, ok := nearest(fd, LineAdded, sideNew, intent.Line); ok && dist <= maxSnapDistance {
		return withNew(base, dl.NewLine), MapOutcome{Kind: MapSnapped, Reason: snapReason(dist)}
	}
	if dl, dist, ok := nearest(fd, LineRemoved, sideOld, intent.Line); ok && dist <= maxSnapDistance {
		return withOld(base, dl.OldLine), MapOutcome{Kind: MapSnapped, Reason: snapReason(dist)}
	}
	return nil, MapOutcome{Kind: MapOverview, Reason: "no changed line within range to anchor to"}
}

// maxSnapDistance bounds how far a finding may be relocated to the nearest
// changed line before it degrades to an overview note.
const maxSnapDistance = 25

func snapReason(dist int) string {
	return fmt.Sprintf("snapped to nearest changed line (%d away)", dist)
}

type side int

const (
	sideOld side = iota
	sideNew
)

func lineNumber(dl DiffLine, s side) int {
	if s == sideOld {
		return dl.OldLine
	}
	return dl.NewLine
}

// findLine returns the first diff line of the given kind whose line number on
// the given side equals target.
func findLine(fd *FileDiff, kind LineKind, s side, target int) (DiffLine, bool) {
	for _, h := range fd.Hunks {
		for _, dl := range h.Lines {
			if dl.Kind == kind && lineNumber(dl, s) == target {
				return dl, true
			}
		}
	}
	return DiffLine{}, false
}

// nearest returns the diff line of the given kind minimizing |line - target|,
// along with that distance.
func nearest(fd *FileDiff, kind LineKind, s side, target int) (DiffLine, int, bool) {
	var best DiffLine
	bestDist := -1
	for _, h := range fd.Hunks {
		for _, dl := range h.Lines {
			if dl.Kind != kind {
				continue
			}
			d := lineNumber(dl, s) - target
			if d < 0 {
				d = -d
			}
			if bestDist < 0 || d < bestDist {
				bestDist = d
				best = dl
			}
		}
	}
	return best, bestDist, bestDist >= 0
}

func basePosition(fd *FileDiff, refs gitlab.DiffRefs) gitlab.Position {
	oldPath, newPath := fd.OldPath, fd.NewPath
	if oldPath == "" {
		oldPath = newPath
	}
	if newPath == "" {
		newPath = oldPath
	}
	return gitlab.Position{
		BaseSHA:      refs.BaseSHA,
		HeadSHA:      refs.HeadSHA,
		StartSHA:     refs.StartSHA,
		PositionType: "text",
		OldPath:      oldPath,
		NewPath:      newPath,
	}
}

func withNew(p gitlab.Position, newLine int) *gitlab.Position {
	p.NewLine = new(newLine)
	return &p
}

func withOld(p gitlab.Position, oldLine int) *gitlab.Position {
	p.OldLine = new(oldLine)
	return &p
}

func withBoth(p gitlab.Position, oldLine, newLine int) *gitlab.Position {
	p.OldLine = new(oldLine)
	p.NewLine = new(newLine)
	return &p
}
