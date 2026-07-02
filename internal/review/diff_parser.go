// Package review implements the AI review engine: context building, diff
// parsing, GitLab position mapping, finding validation, dedupe, and the
// orchestrator that drives the LLM pipeline.
package review

import (
	"fmt"
	"strconv"
	"strings"
)

// LineKind classifies a diff line.
type LineKind int

const (
	// LineContext is an unchanged line present in both sides.
	LineContext LineKind = iota
	// LineAdded is a line present only on the new side.
	LineAdded
	// LineRemoved is a line present only on the old side.
	LineRemoved
)

// DiffLine is a single line within a hunk. OldLine/NewLine are 0 when the line
// does not exist on that side (added lines have OldLine==0; removed lines have
// NewLine==0).
type DiffLine struct {
	Kind    LineKind
	OldLine int
	NewLine int
	Content string
}

// Hunk is a contiguous @@ block.
type Hunk struct {
	OldStart int
	OldCount int
	NewStart int
	NewCount int
	Lines    []DiffLine
}

// FileDiff is the parsed diff for one changed file.
type FileDiff struct {
	OldPath  string
	NewPath  string
	NewFile  bool
	Renamed  bool
	Deleted  bool
	IsBinary bool
	Hunks    []Hunk
}

// Path returns the file's canonical path: the new path, falling back to the
// old one for deletions.
func (f *FileDiff) Path() string {
	if f.NewPath != "" {
		return f.NewPath
	}
	return f.OldPath
}

// AddedLines returns the new-side line numbers of every added line in the
// diff, in order.
func AddedLines(f *FileDiff) []int {
	var out []int
	for _, h := range f.Hunks {
		for _, l := range h.Lines {
			if l.Kind == LineAdded {
				out = append(out, l.NewLine)
			}
		}
	}
	return out
}

// IsBinaryDiff reports whether a raw diff body describes a binary change.
func IsBinaryDiff(diff string) bool {
	return strings.Contains(diff, "Binary files ") || strings.Contains(diff, "GIT binary patch")
}

// ParseHunks parses the hunks from a single file's unified-diff body (the
// GitLab MR `diff` field). Leading git/---/+++ header lines are tolerated and
// skipped. A binary diff yields no hunks and a nil error.
func ParseHunks(diff string) ([]Hunk, error) {
	if diff == "" || IsBinaryDiff(diff) {
		return nil, nil
	}
	lines := strings.Split(diff, "\n")
	var hunks []Hunk
	var cur *Hunk
	var oldLine, newLine int

	for _, raw := range lines {
		switch {
		case strings.HasPrefix(raw, "@@"):
			h, err := parseHunkHeader(raw)
			if err != nil {
				return nil, err
			}
			hunks = append(hunks, h)
			cur = &hunks[len(hunks)-1]
			oldLine = cur.OldStart
			newLine = cur.NewStart

		case cur == nil:
			// Pre-hunk header noise (diff --git, index, ---, +++). Skip.
			continue

		case strings.HasPrefix(raw, "\\"):
			// "\ No newline at end of file" — not a real line.
			continue

		case strings.HasPrefix(raw, "+"):
			cur.Lines = append(cur.Lines, DiffLine{Kind: LineAdded, NewLine: newLine, Content: raw[1:]})
			newLine++

		case strings.HasPrefix(raw, "-"):
			cur.Lines = append(cur.Lines, DiffLine{Kind: LineRemoved, OldLine: oldLine, Content: raw[1:]})
			oldLine++

		case strings.HasPrefix(raw, " "):
			cur.Lines = append(cur.Lines, DiffLine{Kind: LineContext, OldLine: oldLine, NewLine: newLine, Content: raw[1:]})
			oldLine++
			newLine++

		default:
			// Bare "" is the trailing-newline split artifact; real blank
			// context lines are space-prefixed. Any other unknown line inside
			// a hunk is ignored defensively.
			continue
		}
	}
	return hunks, nil
}

// parseHunkHeader parses "@@ -oldStart,oldCount +newStart,newCount @@ ...".
func parseHunkHeader(line string) (Hunk, error) {
	// Trim to the section between the two @@ markers.
	rest := strings.TrimPrefix(line, "@@")
	end := strings.Index(rest, "@@")
	if end < 0 {
		return Hunk{}, fmt.Errorf("malformed hunk header: %q", line)
	}
	spec := strings.TrimSpace(rest[:end])
	fields := strings.Fields(spec)
	if len(fields) < 2 {
		return Hunk{}, fmt.Errorf("malformed hunk header: %q", line)
	}
	oldStart, oldCount, err := parseRange(fields[0], '-')
	if err != nil {
		return Hunk{}, err
	}
	newStart, newCount, err := parseRange(fields[1], '+')
	if err != nil {
		return Hunk{}, err
	}
	return Hunk{OldStart: oldStart, OldCount: oldCount, NewStart: newStart, NewCount: newCount}, nil
}

// parseRange parses "-start,count" / "+start,count" (count optional, default 1).
func parseRange(field string, sign byte) (start, count int, err error) {
	if len(field) == 0 || field[0] != sign {
		return 0, 0, fmt.Errorf("bad range %q", field)
	}
	body := field[1:]
	count = 1
	if comma := strings.IndexByte(body, ','); comma >= 0 {
		start, err = strconv.Atoi(body[:comma])
		if err != nil {
			return 0, 0, fmt.Errorf("bad range start %q: %w", field, err)
		}
		count, err = strconv.Atoi(body[comma+1:])
		if err != nil {
			return 0, 0, fmt.Errorf("bad range count %q: %w", field, err)
		}
	} else {
		start, err = strconv.Atoi(body)
		if err != nil {
			return 0, 0, fmt.Errorf("bad range %q: %w", field, err)
		}
	}
	return start, count, nil
}
