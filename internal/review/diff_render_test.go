package review

import (
	"strings"
	"testing"
)

func TestRenderAnnotatedDiffNumbers(t *testing.T) {
	fd := fileDiff(t, "main.go", "main.go", mapDiff)
	got := RenderAnnotatedDiff(fd)

	wantLines := []string{
		"@@ -1,3 +1,5 @@",
		`     1      1   | package main`,
		`     2      - - | import "fmt"`,
		`     -      2 + | import (`,
		"     -      3 + | \t\"fmt\"",
		`     -      4 + | )`,
		`     3      5   | func main() {}`,
	}
	gotLines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(gotLines) != len(wantLines) {
		t.Fatalf("line count: got %d want %d\n%s", len(gotLines), len(wantLines), got)
	}
	for i, want := range wantLines {
		if gotLines[i] != want {
			t.Errorf("line %d:\n got %q\nwant %q", i, gotLines[i], want)
		}
	}
}

func TestRenderAnnotatedDiffEmpty(t *testing.T) {
	if got := RenderAnnotatedDiff(&FileDiff{OldPath: "bin", NewPath: "bin", IsBinary: true}); got != "" {
		t.Errorf("binary/no-hunk diff should render empty, got %q", got)
	}
}
