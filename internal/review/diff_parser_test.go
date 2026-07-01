package review

import "testing"

const sampleDiff = `@@ -1,3 +1,5 @@
 package main
-import "fmt"
+import (
+	"fmt"
+)
 func main() {}
`

func TestParseHunksBasic(t *testing.T) {
	hunks, err := ParseHunks(sampleDiff)
	if err != nil {
		t.Fatal(err)
	}
	if len(hunks) != 1 {
		t.Fatalf("want 1 hunk, got %d", len(hunks))
	}
	h := hunks[0]
	if h.OldStart != 1 || h.OldCount != 3 || h.NewStart != 1 || h.NewCount != 5 {
		t.Errorf("header parsed wrong: %+v", h)
	}

	// Verify line kinds and numbering.
	var added, removed, ctx int
	for _, dl := range h.Lines {
		switch dl.Kind {
		case LineAdded:
			added++
			if dl.NewLine == 0 || dl.OldLine != 0 {
				t.Errorf("added line numbering wrong: %+v", dl)
			}
		case LineRemoved:
			removed++
			if dl.OldLine == 0 || dl.NewLine != 0 {
				t.Errorf("removed line numbering wrong: %+v", dl)
			}
		case LineContext:
			ctx++
			if dl.OldLine == 0 || dl.NewLine == 0 {
				t.Errorf("context line numbering wrong: %+v", dl)
			}
		}
	}
	if added != 3 || removed != 1 || ctx != 2 {
		t.Errorf("counts: added=%d removed=%d ctx=%d", added, removed, ctx)
	}

	// The first added line ("import (") is new line 2.
	dl, ok := findLine(&FileDiff{Hunks: hunks}, LineAdded, sideNew, 2)
	if !ok || dl.Content != "import (" {
		t.Errorf("expected added new-line 2 to be 'import (', got %q ok=%v", dl.Content, ok)
	}
}

func TestParseHunksMultipleAndDefaultCount(t *testing.T) {
	diff := `@@ -10 +10 @@
-old
+new
@@ -20,2 +21,3 @@
 keep
+extra1
+extra2
-gone
`
	hunks, err := ParseHunks(diff)
	if err != nil {
		t.Fatal(err)
	}
	if len(hunks) != 2 {
		t.Fatalf("want 2 hunks, got %d", len(hunks))
	}
	// Single-line ranges default count to 1.
	if hunks[0].OldCount != 1 || hunks[0].NewCount != 1 {
		t.Errorf("default count wrong: %+v", hunks[0])
	}
	// Second hunk numbering: 'extra1' is new line 22.
	if dl, ok := findLine(&FileDiff{Hunks: hunks}, LineAdded, sideNew, 22); !ok || dl.Content != "extra1" {
		t.Errorf("extra1 should be new line 22, got %q ok=%v", dl.Content, ok)
	}
	// 'gone' is old line 21.
	if dl, ok := findLine(&FileDiff{Hunks: hunks}, LineRemoved, sideOld, 21); !ok || dl.Content != "gone" {
		t.Errorf("gone should be old line 21, got %q ok=%v", dl.Content, ok)
	}
}

func TestParseHunksSkipsHeaderNoise(t *testing.T) {
	diff := `diff --git a/f.go b/f.go
index 111..222 100644
--- a/f.go
+++ b/f.go
@@ -1 +1 @@
-a
+b
`
	hunks, err := ParseHunks(diff)
	if err != nil {
		t.Fatal(err)
	}
	if len(hunks) != 1 || len(hunks[0].Lines) != 2 {
		t.Fatalf("noise not skipped: %+v", hunks)
	}
}

func TestParseHunksNoNewlineMarker(t *testing.T) {
	diff := "@@ -1 +1 @@\n-a\n+b\n\\ No newline at end of file\n"
	hunks, err := ParseHunks(diff)
	if err != nil {
		t.Fatal(err)
	}
	if len(hunks[0].Lines) != 2 {
		t.Errorf("no-newline marker should not be a line: %+v", hunks[0].Lines)
	}
}

func TestIsBinaryDiff(t *testing.T) {
	if !IsBinaryDiff("Binary files a/x.png and b/x.png differ") {
		t.Error("should detect binary")
	}
	if h, _ := ParseHunks("Binary files differ"); h != nil {
		t.Error("binary diff should yield no hunks")
	}
}

func TestParseHunksMalformedHeader(t *testing.T) {
	if _, err := ParseHunks("@@ this is broken @@\n+x\n"); err == nil {
		t.Error("expected error on malformed header")
	}
}
