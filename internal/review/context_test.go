package review

import (
	"fmt"
	"strings"
	"testing"
)

func genFile(lines int) string {
	var b strings.Builder
	for i := 1; i <= lines; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	return b.String()
}

func TestBuildFileContextWholeFile(t *testing.T) {
	fd := fileDiff(t, "main.go", "main.go", mapDiff)
	fc := BuildFileContext(fd, genFile(10), 500, 60)
	if fc == nil {
		t.Fatal("nil FileContext")
	}
	if fc.Truncated {
		t.Error("small file should not be truncated")
	}
	if len(fc.Segments) != 1 || fc.Segments[0].StartLine != 1 || len(fc.Segments[0].Lines) != 10 {
		t.Errorf("want one whole-file segment of 10 lines, got %+v", fc.Segments)
	}
}

func TestBuildFileContextWindowed(t *testing.T) {
	// One hunk at new lines 1..5 with a 3-line window over a 100-line file.
	fd := fileDiff(t, "main.go", "main.go", mapDiff)
	fc := BuildFileContext(fd, genFile(100), 50, 3)
	if fc == nil {
		t.Fatal("nil FileContext")
	}
	if !fc.Truncated {
		t.Error("windowed context should be marked truncated")
	}
	if len(fc.Segments) != 1 {
		t.Fatalf("want 1 merged segment, got %d", len(fc.Segments))
	}
	seg := fc.Segments[0]
	if seg.StartLine != 1 || seg.StartLine+len(seg.Lines)-1 != 8 {
		t.Errorf("window should span lines 1..8, got %d..%d", seg.StartLine, seg.StartLine+len(seg.Lines)-1)
	}
}

func TestHunkWindowsMerge(t *testing.T) {
	hunks := []Hunk{
		{NewStart: 10, NewCount: 2},
		{NewStart: 15, NewCount: 1}, // overlaps the first window
		{NewStart: 90, NewCount: 5},
	}
	spans := hunkWindows(hunks, 4, 100)
	want := [][2]int{{6, 19}, {86, 98}}
	if len(spans) != len(want) {
		t.Fatalf("want %v, got %v", want, spans)
	}
	for i := range want {
		if spans[i] != want[i] {
			t.Errorf("span %d: want %v, got %v", i, want[i], spans[i])
		}
	}
}

func TestBuildFileContextEmptyContent(t *testing.T) {
	fd := fileDiff(t, "main.go", "main.go", mapDiff)
	if fc := BuildFileContext(fd, "", 500, 60); fc != nil {
		t.Errorf("empty content should yield nil, got %+v", fc)
	}
}

func TestRenderFileContextNumbersAndGaps(t *testing.T) {
	fc := FileContext{Path: "a.go", Truncated: true, Segments: []FileSegment{
		{StartLine: 1, Lines: []string{"one"}},
		{StartLine: 40, Lines: []string{"forty"}},
	}}
	got := RenderFileContext(fc)
	if !strings.Contains(got, "     1 | one") || !strings.Contains(got, "    40 | forty") {
		t.Errorf("line numbers wrong:\n%s", got)
	}
	if !strings.Contains(got, "⋮") {
		t.Error("segment gap marker missing")
	}
}
