package review

import (
	"testing"

	"github.com/sxwebdev/ai-reviewer/internal/gitlab"
)

var testRefs = gitlab.DiffRefs{BaseSHA: "base", HeadSHA: "head", StartSHA: "start"}

// fileDiff builds a FileDiff from a raw hunk body for tests.
func fileDiff(t *testing.T, oldPath, newPath, diff string) *FileDiff {
	t.Helper()
	hunks, err := ParseHunks(diff)
	if err != nil {
		t.Fatal(err)
	}
	return &FileDiff{OldPath: oldPath, NewPath: newPath, Hunks: hunks}
}

const mapDiff = `@@ -1,3 +1,5 @@
 package main
-import "fmt"
+import (
+	"fmt"
+)
 func main() {}
`

func TestMapAddedLineSetsNewOnly(t *testing.T) {
	fd := fileDiff(t, "main.go", "main.go", mapDiff)
	pos, out := MapPosition(fd, testRefs, LineIntent{FilePath: "main.go", Line: 2, LineKind: "new"})
	if out.Kind != MapExact {
		t.Fatalf("want exact, got %v (%s)", out.Kind, out.Reason)
	}
	if pos.NewLine == nil || *pos.NewLine != 2 {
		t.Errorf("NewLine = %v, want 2", pos.NewLine)
	}
	if pos.OldLine != nil {
		t.Errorf("OldLine must be nil for an added line, got %v", *pos.OldLine)
	}
	if pos.PositionType != "text" || pos.HeadSHA != "head" || pos.NewPath != "main.go" {
		t.Errorf("base position wrong: %+v", pos)
	}
}

func TestMapRemovedLineSetsOldOnly(t *testing.T) {
	fd := fileDiff(t, "main.go", "main.go", mapDiff)
	// The removed line `import "fmt"` is old line 2.
	pos, out := MapPosition(fd, testRefs, LineIntent{FilePath: "main.go", Line: 2, LineKind: "old"})
	if out.Kind != MapExact {
		t.Fatalf("want exact, got %v", out.Kind)
	}
	if pos.OldLine == nil || *pos.OldLine != 2 {
		t.Errorf("OldLine = %v, want 2", pos.OldLine)
	}
	if pos.NewLine != nil {
		t.Errorf("NewLine must be nil for a removed line, got %v", *pos.NewLine)
	}
}

func TestMapContextLineSetsBoth(t *testing.T) {
	fd := fileDiff(t, "main.go", "main.go", mapDiff)
	// Context line `func main() {}` is new line 5, old line 3.
	pos, out := MapPosition(fd, testRefs, LineIntent{FilePath: "main.go", Line: 5, LineKind: "context"})
	if out.Kind != MapExact {
		t.Fatalf("want exact, got %v", out.Kind)
	}
	if pos.OldLine == nil || pos.NewLine == nil {
		t.Fatalf("context must set both: old=%v new=%v", pos.OldLine, pos.NewLine)
	}
	if *pos.OldLine != 3 || *pos.NewLine != 5 {
		t.Errorf("context lines: old=%d new=%d, want 3/5", *pos.OldLine, *pos.NewLine)
	}
}

func TestMapSnapsToNearbyChangedLine(t *testing.T) {
	fd := fileDiff(t, "main.go", "main.go", mapDiff)
	// Line 6 is not in the diff but within the snap ceiling of added line 4.
	pos, out := MapPosition(fd, testRefs, LineIntent{FilePath: "main.go", Line: 6, LineKind: "new"})
	if out.Kind != MapSnapped {
		t.Fatalf("want snapped, got %v (%s)", out.Kind, out.Reason)
	}
	if pos.NewLine == nil || *pos.NewLine != 4 {
		t.Errorf("snapped NewLine = %v, want nearest added line 4", pos.NewLine)
	}
}

func TestMapFarLineIsOverview(t *testing.T) {
	fd := fileDiff(t, "main.go", "main.go", mapDiff)
	// Line 999 is far beyond any changed line (> snap ceiling) → overview, not a
	// confident inline comment on an unrelated line.
	pos, out := MapPosition(fd, testRefs, LineIntent{FilePath: "main.go", Line: 999, LineKind: "new"})
	if out.Kind != MapOverview || pos != nil {
		t.Errorf("far line should be overview, got kind=%v pos=%v", out.Kind, pos)
	}
}

func TestMapContextByOldLineNumber(t *testing.T) {
	fd := fileDiff(t, "main.go", "main.go", mapDiff)
	// The context line `func main() {}` is old-line 3 / new-line 5. A context
	// finding that reports the OLD number must still map to that context line
	// (both sides set), not fall through to an unrelated added line.
	pos, out := MapPosition(fd, testRefs, LineIntent{FilePath: "main.go", Line: 3, LineKind: "context"})
	if out.Kind != MapExact {
		t.Fatalf("want exact, got %v", out.Kind)
	}
	if pos.OldLine == nil || pos.NewLine == nil || *pos.OldLine != 3 || *pos.NewLine != 5 {
		t.Errorf("context by old line: old=%v new=%v, want 3/5", pos.OldLine, pos.NewLine)
	}
}

func TestMapOverviewWhenNoHunks(t *testing.T) {
	fd := &FileDiff{OldPath: "x", NewPath: "x"}
	pos, out := MapPosition(fd, testRefs, LineIntent{FilePath: "x", Line: 1, LineKind: "new"})
	if out.Kind != MapOverview || pos != nil {
		t.Errorf("empty diff should be overview, got kind=%v pos=%v", out.Kind, pos)
	}
}

func TestMapFileKindIsOverview(t *testing.T) {
	fd := fileDiff(t, "main.go", "main.go", mapDiff)
	pos, out := MapPosition(fd, testRefs, LineIntent{FilePath: "main.go", LineKind: "file"})
	if out.Kind != MapOverview || pos != nil {
		t.Errorf("file kind should be overview, got %v", out.Kind)
	}
}

func TestMapNilFileDiff(t *testing.T) {
	pos, out := MapPosition(nil, testRefs, LineIntent{FilePath: "x", Line: 1, LineKind: "new"})
	if out.Kind != MapOverview || pos != nil {
		t.Errorf("nil file should be overview")
	}
}

func TestMapNewFileUsesNewPathForBoth(t *testing.T) {
	fd := fileDiff(t, "", "added.go", "@@ -0,0 +1,2 @@\n+line1\n+line2\n")
	fd.NewFile = true
	pos, out := MapPosition(fd, testRefs, LineIntent{FilePath: "added.go", Line: 1, LineKind: "new"})
	if out.Kind != MapExact {
		t.Fatalf("want exact, got %v", out.Kind)
	}
	if pos.OldPath != "added.go" || pos.NewPath != "added.go" {
		t.Errorf("new file should use new path for both: old=%q new=%q", pos.OldPath, pos.NewPath)
	}
}

func TestFindFileDiff(t *testing.T) {
	files := []*FileDiff{
		{OldPath: "a.go", NewPath: "a.go"},
		{OldPath: "old.go", NewPath: "new.go"},
	}
	if FindFileDiff(files, "new.go") == nil {
		t.Error("should find by new path")
	}
	if FindFileDiff(files, "old.go") == nil {
		t.Error("should find by old path")
	}
	if FindFileDiff(files, "missing.go") != nil {
		t.Error("should not find missing")
	}
}
