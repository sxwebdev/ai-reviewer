package server

import (
	"testing"

	"github.com/sxwebdev/ai-reviewer/internal/state"
)

// TestMRVMHeadChanged covers the detail-page signal that drives the "new commits
// since this review" banner and the re-review button relabel.
func TestMRVMHeadChanged(t *testing.T) {
	tests := []struct {
		name string
		vm   mrVM
		want bool
	}{
		{"no review yet", mrVM{MR: &state.MergeRequest{HeadSHA: "abc"}}, false},
		{"head still current", mrVM{MR: &state.MergeRequest{HeadSHA: "abc"}, Review: &state.Review{HeadSHA: "abc"}}, false},
		{"head advanced", mrVM{MR: &state.MergeRequest{HeadSHA: "def"}, Review: &state.Review{HeadSHA: "abc"}}, true},
		{"empty MR head", mrVM{MR: &state.MergeRequest{HeadSHA: ""}, Review: &state.Review{HeadSHA: "abc"}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.vm.HeadChanged(); got != tt.want {
				t.Errorf("HeadChanged() = %v, want %v", got, tt.want)
			}
		})
	}
}
