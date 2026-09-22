package linear_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/sxwebdev/ai-reviewer/internal/linear"
)

func board() []linear.WorkflowState {
	return []linear.WorkflowState{
		{ID: "triage", Name: "Triage", Type: "triage", Position: 0},
		{ID: "backlog", Name: "Backlog", Type: "backlog", Position: 10},
		{ID: "todo", Name: "Todo", Type: "unstarted", Position: 20},
		{ID: "progress", Name: "In Progress", Type: "started", Position: 30},
		{ID: "review", Name: linear.InReviewState, Type: "started", Position: 40},
		{ID: "qa", Name: "QA", Type: "started", Position: 50},
		{ID: "done", Name: "Done", Type: "completed", Position: 60},
		{ID: "canceled", Name: "Canceled", Type: "canceled", Position: 70},
	}
}

func TestWorkflowStageOrdersAgainstInReview(t *testing.T) {
	t.Parallel()
	wf, err := linear.NewWorkflow(board())
	if err != nil {
		t.Fatalf("NewWorkflow: %v", err)
	}
	want := map[string]linear.Stage{
		"Triage":      linear.StageBeforeReview,
		"Backlog":     linear.StageBeforeReview,
		"Todo":        linear.StageBeforeReview,
		"In Progress": linear.StageBeforeReview,
		// In Review itself, and everything the team put after it.
		linear.InReviewState: linear.StageReviewOrLater,
		"QA":                 linear.StageReviewOrLater,
		"Done":               linear.StageReviewOrLater,
		// Canceled sorts last on a Linear board but is not progress past review:
		// the merge request belongs to its author, who either moves the card or
		// closes the merge request.
		"Canceled": linear.StageBeforeReview,
	}
	for _, st := range board() {
		if got := wf.Stage(st); got != want[st.Name] {
			t.Errorf("Stage(%q) = %v, want %v", st.Name, got, want[st.Name])
		}
	}
}

// Only the state *id* is read from the caller's copy. This is the guard against
// the whole class of defect the previous design had: a card's own copy of its
// state arrives from a different query, and `Position float64` cannot tell a
// `null` or an absent key from a legitimate 0 — so comparing that 0 graded every
// column after In Review as "never offered for review" and silenced every
// reviewer on the team.
func TestWorkflowStageReadsOnlyTheStateID(t *testing.T) {
	t.Parallel()
	wf, err := linear.NewWorkflow(board())
	if err != nil {
		t.Fatalf("NewWorkflow: %v", err)
	}
	// Everything except the id is wrong or missing, exactly as a `null` position
	// and an omitted type would decode.
	stripped := linear.WorkflowState{ID: "qa"}
	if got := wf.Stage(stripped); got != linear.StageReviewOrLater {
		t.Errorf("Stage(id-only QA) = %v, want review-or-later — the board owns the order", got)
	}
	// And a lie in those fields cannot move the answer either.
	lying := linear.WorkflowState{ID: "qa", Name: "Backlog", Type: "backlog", Position: -99}
	if got := wf.Stage(lying); got != linear.StageReviewOrLater {
		t.Errorf("Stage(mislabelled QA) = %v, want review-or-later", got)
	}
}

// The gate may narrow who is asked to review only when the comparison actually
// happened. Each of these is a column whose order cannot be established, and each
// has to come back unknown so the caller keeps notifying reviewers.
func TestWorkflowStageFailsOpenWhenTheOrderIsNotEstablished(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		extra linear.WorkflowState
		probe linear.WorkflowState
	}{
		{
			// A seventh type in a future Linear release must not read as "before
			// review" — that would stop notifying reviewers everywhere at once.
			name:  "unknown type on the board",
			extra: linear.WorkflowState{ID: "paused", Name: "Paused", Type: "hibernating", Position: 35},
			probe: linear.WorkflowState{ID: "paused"},
		},
		{
			name:  "empty type on the board",
			extra: linear.WorkflowState{ID: "blocked", Name: "Blocked", Type: "", Position: 35},
			probe: linear.WorkflowState{ID: "blocked"},
		},
		{
			// Linear reshuffles these floats; two columns sharing one have no order.
			name:  "position ties with in review",
			extra: linear.WorkflowState{ID: "ready", Name: "Ready", Type: "started", Position: 40},
			probe: linear.WorkflowState{ID: "ready"},
		},
		{
			// A column the board never reported: a page truncated below it, or one
			// created since the board was read.
			name:  "column absent from the board",
			probe: linear.WorkflowState{ID: "invented", Name: "Invented", Type: "started", Position: 45},
		},
		{
			// A board state with no id cannot be addressed, so a card on it is
			// unresolvable rather than matching everything.
			name:  "board state without an id",
			extra: linear.WorkflowState{ID: "", Name: "Nameless", Type: "started", Position: 35},
			probe: linear.WorkflowState{ID: ""},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			states := board()
			if tt.extra.Name != "" {
				states = append(states, tt.extra)
			}
			wf, err := linear.NewWorkflow(states)
			if err != nil {
				t.Fatalf("NewWorkflow: %v", err)
			}
			if got := wf.Stage(tt.probe); got != linear.StageUnknown {
				t.Errorf("Stage = %v, want unknown", got)
			}
			// The rest of the board must still be ordered: one unorderable column
			// may not cost the others their gate.
			if got := wf.Stage(linear.WorkflowState{ID: "progress"}); got != linear.StageBeforeReview {
				t.Errorf("Stage(In Progress) = %v, want before review", got)
			}
		})
	}
}

// The zero Workflow is what a caller holds for a team whose board it could not
// read, and it must be usable without a nil check: unknown for everything.
func TestZeroWorkflowIsUnknownForEverything(t *testing.T) {
	t.Parallel()
	var wf linear.Workflow
	for _, st := range board() {
		if got := wf.Stage(st); got != linear.StageUnknown {
			t.Errorf("zero Workflow Stage(%q) = %v, want unknown", st.Name, got)
		}
	}
}

func TestNewWorkflowRequiresAUsableInReviewState(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		states  []linear.WorkflowState
		wantErr string
	}{
		{name: "none", states: []linear.WorkflowState{{ID: "a", Name: "Todo", Type: "unstarted"}}, wantErr: "found 0"},
		{
			name: "two",
			states: []linear.WorkflowState{
				{ID: "a", Name: "In Review", Type: "started", Position: 1},
				{ID: "b", Name: " in REVIEW ", Type: "started", Position: 2},
			},
			wantErr: "found 2",
		},
		{
			// The one column whose type must resolve, because every comparison is
			// relative to it. Unreadable here means the whole board is unorderable,
			// and that has to be an error an operator sees rather than a gate that
			// quietly never fires.
			name:    "in review has an unknown type",
			states:  []linear.WorkflowState{{ID: "a", Name: "In Review", Type: "reviewing"}},
			wantErr: "unknown type",
		},
		{
			// Ordering is by id, so the review column without one is unusable.
			name:    "in review has no id",
			states:  []linear.WorkflowState{{ID: "", Name: "In Review", Type: "started"}},
			wantErr: "no id",
		},
		{name: "empty board", states: nil, wantErr: "found 0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := linear.NewWorkflow(tt.states)
			if err == nil {
				t.Fatal("NewWorkflow accepted the board")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

// A board too large to read in one page reports the same "found 0" as a renamed
// column, and the operator then goes looking for a rename that never happened.
func TestNewWorkflowNamesTruncationWhenTheBoardFillsThePage(t *testing.T) {
	t.Parallel()
	full := make([]linear.WorkflowState, 250)
	for i := range full {
		full[i] = linear.WorkflowState{ID: "s" + strings.Repeat("x", i%3), Name: "Column", Type: "started", Position: float64(i)}
	}
	_, err := linear.NewWorkflow(full)
	if err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("error = %v, want it to mention truncation", err)
	}
	// A short board says nothing about truncation, or every renamed column would
	// send the operator down that path instead.
	if _, err := linear.NewWorkflow(full[:3]); err == nil || strings.Contains(err.Error(), "truncated") {
		t.Errorf("error = %v, want a plain \"found 0\"", err)
	}
}

// A board where In Review is not of type `started` still has to work: nothing
// stops a team from marking it otherwise, and the comparison is relative to
// wherever it actually is. This is also what pins the relative order of the three
// types below `started` — collapsing them leaves every other test green.
func TestNewWorkflowDoesNotAssumeInReviewIsStarted(t *testing.T) {
	t.Parallel()
	states := []linear.WorkflowState{
		{ID: "triage", Name: "Triage", Type: "triage", Position: 1},
		{ID: "backlog", Name: "Backlog", Type: "backlog", Position: 2},
		{ID: "review", Name: linear.InReviewState, Type: "unstarted", Position: 3},
		{ID: "progress", Name: "In Progress", Type: "started", Position: 4},
	}
	wf, err := linear.NewWorkflow(states)
	if err != nil {
		t.Fatalf("NewWorkflow: %v", err)
	}
	want := map[string]linear.Stage{
		"triage":   linear.StageBeforeReview,
		"backlog":  linear.StageBeforeReview,
		"review":   linear.StageReviewOrLater,
		"progress": linear.StageReviewOrLater,
	}
	for id, expected := range want {
		if got := wf.Stage(linear.WorkflowState{ID: id}); got != expected {
			t.Errorf("Stage(%s) = %v, want %v", id, got, expected)
		}
	}
}

// Board order is type first, then position within one type. Position alone
// interleaves a backlog column with a started one, so anything that prints a
// board has to use this.
func TestCompareStatesOrdersByTypeThenPosition(t *testing.T) {
	t.Parallel()
	states := []linear.WorkflowState{
		{ID: "qa", Name: "QA", Type: "started", Position: 1},
		{ID: "paused", Name: "Paused", Type: "hibernating", Position: 0},
		{ID: "done", Name: "Done", Type: "completed", Position: 0},
		{ID: "progress", Name: "In Progress", Type: "started", Position: 0},
		{ID: "backlog", Name: "Backlog", Type: "backlog", Position: 99},
	}
	slices.SortStableFunc(states, linear.CompareStates)
	var got []string
	for _, st := range states {
		got = append(got, st.Name)
	}
	// Backlog sorts first despite the largest position; the unrankable type sorts
	// last so it is visible as a group rather than scattered through real columns.
	want := []string{"Backlog", "In Progress", "QA", "Done", "Paused"}
	if !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

// Label is the one rule for naming a team in a diagnostic, shared by doctor and the
// digest. The empty-key case is why it exists: concatenating name and key by hand
// printed "Chain ()".
func TestTeamLabel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		team linear.Team
		want string
	}{
		{name: "name and key", team: linear.Team{ID: "t1", Name: "Chain", Key: "CHAIN"}, want: "Chain (CHAIN)"},
		{name: "no key", team: linear.Team{ID: "t1", Name: "Chain"}, want: "Chain"},
		{name: "blank key", team: linear.Team{ID: "t1", Name: "Chain", Key: "  "}, want: "Chain"},
		{name: "no name falls back to the id", team: linear.Team{ID: "t1", Key: "CHAIN"}, want: "t1 (CHAIN)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.team.Label(); got != tt.want {
				t.Errorf("Label = %q, want %q", got, tt.want)
			}
		})
	}
}
