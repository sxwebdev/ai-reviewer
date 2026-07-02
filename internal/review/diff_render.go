package review

import (
	"fmt"
	"strconv"
	"strings"
)

// RenderAnnotatedDiff renders a parsed FileDiff with explicit old/new line
// numbers on every line so the model can cite exact positions instead of
// counting offsets from @@ headers. Format per line:
//
//	old   new M | content
//
// where old/new are the line numbers on each side ("-" when the line does not
// exist on that side) and M is the change marker (+, - or space).
func RenderAnnotatedDiff(f *FileDiff) string {
	var b strings.Builder
	for _, h := range f.Hunks {
		fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n", h.OldStart, h.OldCount, h.NewStart, h.NewCount)
		for _, l := range h.Lines {
			b.WriteString(renderDiffLine(l))
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func renderDiffLine(l DiffLine) string {
	old, new_, marker := "-", "-", " "
	switch l.Kind {
	case LineAdded:
		new_ = strconv.Itoa(l.NewLine)
		marker = "+"
	case LineRemoved:
		old = strconv.Itoa(l.OldLine)
		marker = "-"
	case LineContext:
		old = strconv.Itoa(l.OldLine)
		new_ = strconv.Itoa(l.NewLine)
	}
	return fmt.Sprintf("%6s %6s %s | %s", old, new_, marker, l.Content)
}
