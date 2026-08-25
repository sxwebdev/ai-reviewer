package service

import (
	"slices"
	"testing"
	"time"

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
	tests := []struct {
		name   string
		linked bool
		state  string
		stage  linear.Stage
		// approved means an approval is recorded *and* readable. approvedButBlind
		// is the outage: GitLab holds an approval this service cannot see.
		approved         bool
		approvedButBlind bool
		requestedChanges bool

		wantReviewerAction bool
		wantMoveLinear     bool
		wantStartLinear    bool
	}{
		{name: "unlinked standard flow", wantReviewerAction: true},
		{
			name: "in review without approval", linked: true, state: linear.InReviewState,
			stage: linear.StageReviewOrLater, wantReviewerAction: true,
		},
		{
			name: "later status without approval fails open on completion", linked: true, state: "Done",
			stage: linear.StageReviewOrLater, wantReviewerAction: true,
		},
		{
			name: "in review with approval nudges author", linked: true, state: linear.InReviewState,
			stage: linear.StageReviewOrLater, approved: true, wantMoveLinear: true,
		},
		{
			name: "later status with approval is complete", linked: true, state: "Done",
			stage: linear.StageReviewOrLater, approved: true,
		},
		{name: "unlinked approval keeps standard reviewer flow", approved: true, wantReviewerAction: true},
		{
			name: "requested changes overrides in review approval", linked: true, state: linear.InReviewState,
			stage: linear.StageReviewOrLater, approved: true, requestedChanges: true, wantReviewerAction: true,
		},

		// The readiness gate. It outranks approvals and the verdict alike, and
		// every one of these rows also asserts the author row that keeps the merge
		// request in the digest.
		{
			name: "before review parks the mr with its author", linked: true, state: "In Progress",
			stage: linear.StageBeforeReview, wantStartLinear: true,
		},
		{
			name: "before review outranks an approval", linked: true, state: "In Progress",
			stage: linear.StageBeforeReview, approved: true, wantStartLinear: true,
		},
		{
			name: "before review outranks requested changes", linked: true, state: "In Progress",
			stage: linear.StageBeforeReview, requestedChanges: true, wantStartLinear: true,
		},
		{
			name: "unknown stage fails open", linked: true, state: "Blocked",
			stage: linear.StageUnknown, wantReviewerAction: true,
		},
		{
			// A stage without a link is not a fact about anything: nothing was
			// matched, so the board has no say.
			name: "stage without a link is ignored", state: "In Progress",
			stage: linear.StageBeforeReview, wantReviewerAction: true,
		},

		// Unknown approvals may not read as zero approvals, in either direction:
		// reviewers stay notified and the author is not told the review finished.
		{
			name: "blind approvals keep notifying reviewers", linked: true, state: linear.InReviewState,
			stage: linear.StageReviewOrLater, approvedButBlind: true, wantReviewerAction: true,
		},
		{
			// Named for what it pins: at Done, needsLinearMove is already false on the
			// state name alone, so this row says nothing about ApprovalsKnown — the row
			// above is the one that does. It pins that a later column plus an
			// unreadable approval still owes a reviewer.
			name: "blind approvals keep notifying reviewers past review", linked: true, state: "Done",
			stage: linear.StageReviewOrLater, approvedButBlind: true, wantReviewerAction: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			snapshot := domain.MergeRequestSnapshot{
				MR:             domain.MergeRequest{State: "opened"},
				ApprovalsKnown: !tt.approvedButBlind,
			}
			if tt.approved || tt.approvedButBlind {
				snapshot.ApprovedBy = []domain.User{{ID: 9, Username: "approver"}}
			}
			if tt.requestedChanges {
				snapshot.Reviewers = []domain.Reviewer{{
					User: domain.User{ID: 2, Username: "blocker"}, State: domain.ReviewStateRequestedChanges,
				}}
			}
			link := linearLink{
				issue: linear.Issue{Identifier: "CHAIN-184", State: linear.WorkflowState{Name: tt.state}},
				stage: tt.stage,
			}
			if got := needsReviewerAction(snapshot, reviewer, link, tt.linked); got != tt.wantReviewerAction {
				t.Errorf("needsReviewerAction = %v, want %v", got, tt.wantReviewerAction)
			}
			if got := needsLinearMove(snapshot, link, tt.linked); got != tt.wantMoveLinear {
				t.Errorf("needsLinearMove = %v, want %v", got, tt.wantMoveLinear)
			}
			if got := needsLinearStart(snapshot, link, tt.linked); got != tt.wantStartLinear {
				t.Errorf("needsLinearStart = %v, want %v", got, tt.wantStartLinear)
			}
		})
	}
}

// The partition rule, asserted as a property rather than as a list of rows: a
// merge request the readiness gate takes away from its reviewers must always be
// handed to its author. Getting this wrong does not produce a wrong line, it
// produces a merge request that appears in the digest under nobody — which is the
// failure this repository has already shipped once.
//
// "Because of the readiness gate" is detected by re-asking with the stage
// unresolved, which is the fail-open answer: if the reviewer owed an action then
// and not now, the gate is what removed it.
func TestReadinessSuppressionAlwaysHandsTheMRToItsAuthor(t *testing.T) {
	t.Parallel()
	stages := []linear.Stage{linear.StageUnknown, linear.StageBeforeReview, linear.StageReviewOrLater}
	reviewStates := []domain.ReviewState{
		domain.ReviewStateUnreviewed, domain.ReviewStateReviewed, domain.ReviewStateApproved,
		domain.ReviewStateUnapproved, domain.ReviewStateReviewStarted, domain.ReviewStateRequestedChanges,
		domain.ReviewStateUnknown,
	}
	// Counted, and required to be non-zero at the end. The baseline probe below asks
	// the function under test what it does with StageUnknown, so any mutation that
	// makes Unknown behave like BeforeReview empties every iteration of its
	// assertion — deleting the gate outright then leaves this test green. A property
	// test that cannot tell "the property holds" from "the property was never
	// exercised" is not a test.
	suppressed := 0
	for _, state := range []string{"opened", "merged", "closed"} {
		for _, draft := range []bool{false, true} {
			for _, stage := range stages {
				for _, rs := range reviewStates {
					for _, approved := range []bool{false, true} {
						for _, known := range []bool{false, true} {
							// A second reviewer whose verdict stands independently of the one
							// being probed. Without it hasRequestedChanges(snapshot) can only
							// be true when the *probed* reviewer requested changes, so the
							// term escapes the cross product entirely.
							for _, blocked := range []bool{false, true} {
								reviewer := domain.Reviewer{
									User: domain.User{ID: 1, Username: "r"}, State: rs,
									// Dated, so the push-aware REQUESTED_CHANGES branch of
									// NeedsHumanReview can actually fire; left at zero it
									// short-circuits and that dimension is dead.
									LastActivityAt: time.Unix(100, 0),
								}
								snap := domain.MergeRequestSnapshot{
									MR:             domain.MergeRequest{State: state, Draft: draft},
									Reviewers:      []domain.Reviewer{reviewer},
									ApprovalsKnown: known,
									LastPushAt:     time.Unix(200, 0),
								}
								if blocked {
									snap.Reviewers = append(snap.Reviewers, domain.Reviewer{
										User:           domain.User{ID: 2, Username: "blocker"},
										State:          domain.ReviewStateRequestedChanges,
										LastActivityAt: time.Unix(100, 0),
									})
								}
								if approved {
									snap.ApprovedBy = []domain.User{{ID: 9}}
								}
								link := linearLink{issue: linear.Issue{Identifier: "CHAIN-1"}, stage: stage}
								open := linearLink{issue: link.issue, stage: linear.StageUnknown}

								if !needsReviewerAction(snap, reviewer, open, true) ||
									needsReviewerAction(snap, reviewer, link, true) {
									continue
								}
								suppressed++
								if !needsLinearStart(snap, link, true) {
									t.Fatalf("readiness gate dropped the mr with no author row: state=%s draft=%v stage=%v review=%v approved=%v approvals_known=%v blocked=%v",
										state, draft, stage, rs, approved, known, blocked)
								}
							}
						}
					}
				}
			}
		}
	}
	if suppressed == 0 {
		t.Fatal("the readiness gate never suppressed anything, so this test asserted nothing")
	}
}

// "First valid identifier in the title wins" is fine for *naming* a card, and it
// was harmless while Linear could only stop notifications an approval had already
// stopped. The readiness gate removed that precondition, so an unlucky title could
// silence every reviewer on a merge request and hand its author an impossible
// instruction. The gate now applies only where the candidates agree.
func TestLinearStageRefusesToGradeAnAmbiguousMatch(t *testing.T) {
	t.Parallel()
	canceled := linear.Issue{Identifier: "CHAIN-1"}
	inReview := linear.Issue{Identifier: "CHAIN-2"}
	stages := map[string]linear.Stage{
		"CHAIN-1": linear.StageBeforeReview,
		"CHAIN-2": linear.StageReviewOrLater,
	}
	stageOf := func(issue linear.Issue) linear.Stage { return stages[issue.Identifier] }

	// The title names a superseded card first, the branch names the live one.
	ambiguous := linearIssueMatch{
		issue: canceled, found: true,
		candidates: []linear.Issue{canceled, inReview},
	}
	if got := linearStage(ambiguous, stageOf); got != linear.StageUnknown {
		t.Errorf("linearStage = %v, want unknown — two cards, two verdicts, no gate", got)
	}

	// Agreement is still graded, so the ordinary "two tickets, both In Progress"
	// title keeps its gate.
	other := linear.Issue{Identifier: "CHAIN-3"}
	stages["CHAIN-3"] = linear.StageBeforeReview
	agreeing := linearIssueMatch{
		issue: canceled, found: true,
		candidates: []linear.Issue{canceled, other},
	}
	if got := linearStage(agreeing, stageOf); got != linear.StageBeforeReview {
		t.Errorf("linearStage = %v, want before review", got)
	}

	// And the unambiguous case is untouched.
	single := linearIssueMatch{issue: inReview, found: true, candidates: []linear.Issue{inReview}}
	if got := linearStage(single, stageOf); got != linear.StageReviewOrLater {
		t.Errorf("linearStage = %v, want review-or-later", got)
	}
}

// A draft or closed MR carries no author action// A draft or closed MR carries no author action, and the two Linear nudges are
// author actions like any other — they just do not come from
// ClassifyAuthorActions, so they need the guard restated.
func TestLinearAuthorActionsSkipDraftAndClosedMergeRequests(t *testing.T) {
	t.Parallel()
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
				MR:             tt.mr,
				ApprovedBy:     []domain.User{{ID: 9, Username: "approver"}},
				ApprovalsKnown: true,
			}
			move := linearLink{
				issue: linear.Issue{Identifier: "CHAIN-184", State: linear.WorkflowState{Name: linear.InReviewState}},
				stage: linear.StageReviewOrLater,
			}
			if got := needsLinearMove(snapshot, move, true); got != tt.want {
				t.Errorf("needsLinearMove = %v, want %v", got, tt.want)
			}
			start := linearLink{
				issue: linear.Issue{Identifier: "CHAIN-184", State: linear.WorkflowState{Name: "In Progress"}},
				stage: linear.StageBeforeReview,
			}
			if got := needsLinearStart(snapshot, start, true); got != tt.want {
				t.Errorf("needsLinearStart = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNeedsReviewerActionKeepsGitLabTerminalVerdict(t *testing.T) {
	t.Parallel()
	snapshot := domain.MergeRequestSnapshot{MR: domain.MergeRequest{State: "opened"}}
	reviewer := domain.Reviewer{State: domain.ReviewStateApproved}
	if needsReviewerAction(snapshot, reviewer, linearLink{stage: linear.StageReviewOrLater}, true) {
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

// TestReviewerTagSuppressionAlwaysLeavesTheRowSaying is the same partition
// argument as TestReadinessSuppressionAlwaysHandsTheMRToItsAuthor, applied to
// the one flag the board is allowed to withdraw.
//
// needsReviewerTag drops "add a reviewer" when the card has not been offered for
// review. That is legal only because needsLinearStart is true for exactly that
// set, so the author still gets a row telling them to move the card. If the two
// guards ever drift, digestData renders an author row whose every flag was
// suppressed — a bare link with nothing to do about it.
func TestReviewerTagSuppressionAlwaysLeavesTheRowSaying(t *testing.T) {
	t.Parallel()

	stages := []linear.Stage{linear.StageUnknown, linear.StageBeforeReview, linear.StageReviewOrLater}
	// Counted for the same reason the readiness property counts: a mutation that
	// makes needsReviewerTag always false empties every assertion below, and a
	// property test that cannot tell "held" from "never ran" is not a test.
	suppressed := 0
	for _, state := range []string{"opened", "merged", "closed"} {
		for _, draft := range []bool{false, true} {
			for _, stage := range stages {
				for _, linked := range []bool{false, true} {
					for _, approved := range []bool{false, true} {
						for _, known := range []bool{false, true} {
							for _, tagged := range []bool{false, true} {
								snap := domain.MergeRequestSnapshot{
									MR:             domain.MergeRequest{State: state, Draft: draft},
									ApprovalsKnown: known,
								}
								if tagged {
									snap.Reviewers = []domain.Reviewer{{
										User: domain.User{ID: 1, Username: "r"}, State: domain.ReviewStateUnreviewed,
									}}
								}
								if approved {
									snap.ApprovedBy = []domain.User{{ID: 9}}
								}
								link := linearLink{issue: linear.Issue{Identifier: "CHAIN-1"}, stage: stage}

								raw := domain.NoReviewersAssigned(snap)
								gated := needsReviewerTag(snap, link, linked)
								if gated && !raw {
									t.Fatalf("the gate invented a flag GitLab never reported: "+
										"state=%s draft=%v stage=%v linked=%v", state, draft, stage, linked)
								}
								if !raw || gated {
									continue
								}
								suppressed++
								if !needsLinearStart(snap, link, linked) {
									t.Fatalf("suppressed the only flag on the row and left nothing to say: "+
										"state=%s draft=%v stage=%v linked=%v approved=%v known=%v",
										state, draft, stage, linked, approved, known)
								}
							}
						}
					}
				}
			}
		}
	}
	if suppressed == 0 {
		t.Fatal("no input suppressed the flag; the property was never exercised")
	}
}
