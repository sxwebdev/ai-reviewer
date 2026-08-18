package service

import (
	"slices"
	"testing"

	"github.com/sxwebdev/ai-reviewer/internal/domain"
	"github.com/sxwebdev/ai-reviewer/internal/linear"
)

func TestMatchLinearIssuePrefersValidTitleThenBranchCaseInsensitively(t *testing.T) {
	t.Parallel()
	issues := map[string]linear.Issue{
		"CHAIN-184": {Identifier: "CHAIN-184"},
		"CHAIN-203": {Identifier: "CHAIN-203"},
	}
	tests := []struct {
		name          string
		title         string
		branch        string
		want          string
		wantField     string
		wantConflicts []string
	}{
		{name: "title", title: "feat: [chain-184] wrapper", branch: "feature/no-key", want: "CHAIN-184", wantField: "title"},
		{name: "branch fallback", title: "Scheduler wrapper", branch: "feature/ChAiN-203_scheduler", want: "CHAIN-203", wantField: "source_branch"},
		{name: "unknown title key falls back", title: "BTA-4203 wrapper", branch: "chain-184-wrapper", want: "CHAIN-184", wantField: "source_branch"},
		{name: "valid title wins conflict", title: "CHAIN-184 wrapper", branch: "feature/chain-203", want: "CHAIN-184", wantField: "title", wantConflicts: []string{"CHAIN-203"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := matchLinearIssue(domain.MergeRequest{Title: tt.title, SourceBranch: tt.branch}, issues)
			if !got.found || got.issue.Identifier != tt.want || got.matchField != tt.wantField || !slices.Equal(got.conflicts, tt.wantConflicts) {
				t.Errorf("match = %+v, want identifier=%q field=%q conflicts=%v", got, tt.want, tt.wantField, tt.wantConflicts)
			}
		})
	}
}

func TestLinearReviewPolicy(t *testing.T) {
	t.Parallel()
	reviewer := domain.Reviewer{User: domain.User{ID: 1, Username: "reviewer"}, State: domain.ReviewStateUnreviewed}
	issue := func(state string) linear.Issue {
		return linear.Issue{Identifier: "CHAIN-184", State: linear.WorkflowState{Name: state}}
	}
	tests := []struct {
		name               string
		linked             bool
		state              string
		approved           bool
		requestedChanges   bool
		wantReviewerAction bool
		wantMoveLinear     bool
	}{
		{name: "unlinked standard flow", wantReviewerAction: true},
		{name: "in review without approval", linked: true, state: linear.InReviewState, wantReviewerAction: true},
		{name: "other status without approval fails open", linked: true, state: "QA", wantReviewerAction: true},
		{name: "in review with approval nudges author", linked: true, state: linear.InReviewState, approved: true, wantMoveLinear: true},
		{name: "other status with approval is complete", linked: true, state: "QA", approved: true},
		{name: "unlinked approval keeps standard reviewer flow", approved: true, wantReviewerAction: true},
		{name: "requested changes overrides in review approval", linked: true, state: linear.InReviewState, approved: true, requestedChanges: true, wantReviewerAction: true},
		{name: "requested changes overrides QA approval", linked: true, state: "QA", approved: true, requestedChanges: true, wantReviewerAction: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			snapshot := domain.MergeRequestSnapshot{
				MR: domain.MergeRequest{State: "opened"},
			}
			if tt.approved {
				snapshot.ApprovedBy = []domain.User{{ID: 9, Username: "approver"}}
			}
			if tt.requestedChanges {
				snapshot.Reviewers = []domain.Reviewer{{
					User: domain.User{ID: 2, Username: "blocker"}, State: domain.ReviewStateRequestedChanges,
				}}
			}
			linkedIssue := issue(tt.state)
			if got := needsReviewerAction(snapshot, reviewer, tt.linked); got != tt.wantReviewerAction {
				t.Errorf("needsReviewerAction = %v, want %v", got, tt.wantReviewerAction)
			}
			if got := needsLinearMove(snapshot, linkedIssue, tt.linked); got != tt.wantMoveLinear {
				t.Errorf("needsLinearMove = %v, want %v", got, tt.wantMoveLinear)
			}
		})
	}
}

// A draft or closed MR carries no author action, and the Linear nudge is an
// author action like any other — it just does not come from
// ClassifyAuthorActions, so it needs the guard restated.
func TestNeedsLinearMoveSkipsDraftAndClosedMergeRequests(t *testing.T) {
	t.Parallel()
	issue := linear.Issue{Identifier: "CHAIN-184", State: linear.WorkflowState{Name: linear.InReviewState}}
	tests := []struct {
		name string
		mr   domain.MergeRequest
		want bool
	}{
		{name: "open", mr: domain.MergeRequest{State: "opened"}, want: true},
		{name: "draft", mr: domain.MergeRequest{State: "opened", Draft: true}},
		{name: "merged", mr: domain.MergeRequest{State: "merged"}},
		{name: "closed", mr: domain.MergeRequest{State: "closed"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			snapshot := domain.MergeRequestSnapshot{
				MR:         tt.mr,
				ApprovedBy: []domain.User{{ID: 9, Username: "approver"}},
			}
			if got := needsLinearMove(snapshot, issue, true); got != tt.want {
				t.Errorf("needsLinearMove = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNeedsReviewerActionKeepsGitLabTerminalVerdict(t *testing.T) {
	t.Parallel()
	snapshot := domain.MergeRequestSnapshot{MR: domain.MergeRequest{State: "opened"}}
	reviewer := domain.Reviewer{State: domain.ReviewStateApproved}
	if needsReviewerAction(snapshot, reviewer, true) {
		t.Error("an APPROVED reviewer was reopened by the Linear policy")
	}
}

func TestLinearIdentifiersRequireBoundariesNormalizeAndDedupe(t *testing.T) {
	t.Parallel()
	text := "CHAIN-1/chain-2 CHAIN-3abc 1CHAIN-4 [ChAiN-5] chain-1"
	if got := linearIdentifiers(text); !slices.Equal(got, []string{"CHAIN-1", "CHAIN-2", "CHAIN-5"}) {
		t.Errorf("identifiers = %v, want [CHAIN-1 CHAIN-2 CHAIN-5]", got)
	}
}

func TestLinearIssueNumbersAreUniqueAndSorted(t *testing.T) {
	t.Parallel()
	snapshots := []domain.MergeRequestSnapshot{
		{MR: domain.MergeRequest{Title: "CHAIN-203/chain-184", SourceBranch: "feature/CHAIN-203"}},
		{MR: domain.MergeRequest{Title: "No task", SourceBranch: "fix/chain-9-something"}},
	}
	if got := linearIssueNumbers(snapshots); !slices.Equal(got, []int{9, 184, 203}) {
		t.Errorf("numbers = %v, want [9 184 203]", got)
	}
}
