package service

import (
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/sxwebdev/ai-reviewer/internal/domain"
	"github.com/sxwebdev/ai-reviewer/internal/linear"
)

// linearIdentifierRE deliberately extracts candidates rather than declaring
// them valid. A title may contain another tracker key (for example BTA-4203);
// only an issue returned from one of the configured Linear teams is a match.
var linearIdentifierRE = regexp.MustCompile(`(?i)[a-z][a-z0-9]*-[0-9]+`)

// linearLink is one merge request's resolved Linear task: the issue, plus where
// its status sits relative to In Review in that issue's own team.
//
// The stage is stored rather than recomputed because it depends on the *team's*
// workflow, which the rules below have no access to — and because an
// unresolvable ordering has to travel with the link instead of being guessed at
// each call site.
type linearLink struct {
	issue linear.Issue
	stage linear.Stage
}

type linearDigestState struct {
	// enabled is "the board total arrived" — the one fact allowed to decide that
	// Linear contributed nothing at all.
	enabled       bool
	inReviewCount int
	// linksKnown and gateKnown are the two halves that can degrade
	// independently, and they are tracked separately because they produce
	// different behaviour and therefore need different words in the digest: with
	// no links Linear stops narrowing anything, while with no gate the links are
	// present and only the column *order* is missing. One shared "Linear is
	// degraded" line for both told an operator neither.
	linksKnown bool
	gateKnown  bool
	// gateDegradedLinks is true when at least one merge request was graded against
	// a board whose order was missing. gateKnown says a board failed;
	// this says a merge request noticed. Only the second licenses the digest to
	// claim that merge requests were treated as ready for review.
	gateDegradedLinks bool
	linksByMR         map[string]linearLink
}

// linkFor returns the merge request's Linear task. The bool answers "was a task
// matched", not "could the board be read": a link whose stage is
// linear.StageUnknown is still a link, and every rule below fails open on that
// stage rather than on a missing one.
func (s linearDigestState) linkFor(snapshot domain.MergeRequestSnapshot) (linearLink, bool) {
	link, ok := s.linksByMR[snapshotKey(snapshot.Project.ID, snapshot.MR.IID)]
	return link, ok
}

type linearIssueMatch struct {
	issue linear.Issue
	found bool
	// candidates are every valid issue the title and branch named, chosen first,
	// in match order. The readiness gate needs them all, not just the winner: see
	// linearStage.
	candidates []linear.Issue
	conflicts  []string
	matchField string
}

// matchLinearIssue prefers a valid identifier from the MR title, then falls
// back to the source branch. Casing never matters. Unknown-looking candidates
// are ignored rather than treated as a missing Linear task.
func matchLinearIssue(mr domain.MergeRequest, issues map[string]linear.Issue) linearIssueMatch {
	title := validLinearIssues(linearIdentifiers(mr.Title), issues)
	branch := validLinearIssues(linearIdentifiers(mr.SourceBranch), issues)

	var chosen linear.Issue
	field := ""
	switch {
	case len(title) > 0:
		chosen, field = title[0], "title"
	case len(branch) > 0:
		chosen, field = branch[0], "source_branch"
	default:
		return linearIssueMatch{}
	}

	seen := map[string]bool{strings.ToUpper(chosen.Identifier): true}
	candidates := []linear.Issue{chosen}
	var conflicts []string
	for _, candidate := range append(title, branch...) {
		identifier := strings.ToUpper(candidate.Identifier)
		if seen[identifier] {
			continue
		}
		seen[identifier] = true
		candidates = append(candidates, candidate)
		conflicts = append(conflicts, candidate.Identifier)
	}
	return linearIssueMatch{
		issue: chosen, found: true, candidates: candidates,
		conflicts: conflicts, matchField: field,
	}
}

// linearStage grades a match, and refuses to grade an ambiguous one.
//
// "First valid identifier in the title wins" is a reasonable rule for *naming*
// the card in a digest row, and it was harmless while Linear could only ever
// stop notifications that an approval had already stopped. The readiness gate
// removed that precondition, which handed the heuristic the power to silence
// every reviewer on a merge request: a title like
// "CHAIN-1 superseded by CHAIN-2 work" picks CHAIN-1, and if that card is
// canceled while CHAIN-2 is properly In Review, nobody is asked to review and the
// author is told to move a card that cannot be moved.
//
// So when a merge request names several valid issues, the gate only applies where
// they agree. Disagreement is StageUnknown, i.e. today's behaviour, which is the
// one answer that cannot hide work. The winner still owns the identifier and URL
// the digest renders — that part of the heuristic is unchanged.
func linearStage(match linearIssueMatch, stageOf func(linear.Issue) linear.Stage) linear.Stage {
	stage := stageOf(match.issue)
	for _, candidate := range match.candidates {
		if stageOf(candidate) != stage {
			return linear.StageUnknown
		}
	}
	return stage
}

func validLinearIssues(candidates []string, issues map[string]linear.Issue) []linear.Issue {
	out := make([]linear.Issue, 0, len(candidates))
	seen := make(map[string]bool, len(candidates))
	for _, candidate := range candidates {
		key := strings.ToUpper(candidate)
		issue, ok := issues[key]
		if !ok || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, issue)
	}
	return out
}

func linearIdentifiers(text string) []string {
	matches := linearIdentifierRE.FindAllStringIndex(text, -1)
	out := make([]string, 0, len(matches))
	seen := make(map[string]bool, len(matches))
	for _, match := range matches {
		if match[0] > 0 && linearIdentifierChar(text[match[0]-1]) {
			continue
		}
		if match[1] < len(text) && linearIdentifierChar(text[match[1]]) {
			continue
		}
		identifier := strings.ToUpper(text[match[0]:match[1]])
		if seen[identifier] {
			continue
		}
		seen[identifier] = true
		out = append(out, identifier)
	}
	return out
}

func linearIdentifierChar(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}

func linearIssueNumbers(snapshots []domain.MergeRequestSnapshot) []int {
	seen := make(map[int]bool)
	for _, snapshot := range snapshots {
		for _, text := range []string{snapshot.MR.Title, snapshot.MR.SourceBranch} {
			for _, identifier := range linearIdentifiers(text) {
				separator := strings.LastIndexByte(identifier, '-')
				n, err := strconv.Atoi(identifier[separator+1:])
				if err == nil && n > 0 {
					seen[n] = true
				}
			}
		}
	}
	numbers := make([]int, 0, len(seen))
	for n := range seen {
		numbers = append(numbers, n)
	}
	sort.Ints(numbers)
	return numbers
}

func hasRequestedChanges(snapshot domain.MergeRequestSnapshot) bool {
	for _, reviewer := range snapshot.Reviewers {
		if reviewer.State == domain.ReviewStateRequestedChanges {
			return true
		}
	}
	return false
}

// needsReviewerAction applies both Linear gates on top of GitLab's per-reviewer
// classifier: readiness before the review and completion after it.
//
// Missing tasks, an unreadable board and unknown approvals all fail open — the
// board may narrow who is asked, never on the strength of a question that was
// not answered.
func needsReviewerAction(snapshot domain.MergeRequestSnapshot, reviewer domain.Reviewer, link linearLink, linked bool) bool {
	if !domain.NeedsHumanReview(snapshot, reviewer) {
		return false
	}
	// Readiness, and it outranks everything below — including a standing
	// REQUESTED_CHANGES verdict, which is the deliberate reversal of the original
	// rule ("any status + no approvals → ordinary GitLab classification"). A card
	// the author has not moved to In Review is work that was never offered, so
	// asking three reviewers to look at it nudges everyone except the one person
	// who can fix it.
	//
	// The merge request is not lost by this: needsLinearStart is true for exactly
	// this set — same open/non-draft guard, same stage — so every suppression here
	// produces an author row. That pairing is the digest's partition rule, and it
	// is the precondition for being allowed to silence anyone on board state at
	// all. Change one without the other and the merge request disappears.
	if linked && link.stage == linear.StageBeforeReview {
		return false
	}
	// Completion. Unknown approvals must not read as zero approvals: that is a
	// question GitLab refused to answer, and the answer which keeps notifying
	// reviewers is the one that cannot hide work.
	if !linked || !snapshot.ApprovalsKnown || len(snapshot.ApprovedBy) == 0 || hasRequestedChanges(snapshot) {
		return true
	}
	return false
}

// needsLinearMove tells the MR author that review is complete but the linked
// task is still parked in In Review. REQUESTED_CHANGES always wins over an
// approval, so the author sees the normal changes-requested action instead.
//
// The draft/open guard is the same one domain.NeedsHumanReview and
// domain.changesRequestedBy open with, and it has to be repeated here because
// this is the one author action that does not come from ClassifyAuthorActions:
// an approval on a draft is an early look, not a finished review, so telling the
// author to advance the board asks them to move a card for work they have not
// finished marking as ready.
func needsLinearMove(snapshot domain.MergeRequestSnapshot, link linearLink, linked bool) bool {
	if !snapshot.MR.IsOpen() || snapshot.MR.Draft {
		return false
	}
	// The state *name*, not the stage: "move it forward" is only true of a card
	// sitting exactly on In Review, whereas StageReviewOrLater also covers Done —
	// where there is nothing left to advance.
	//
	// ApprovalsKnown for the same reason needsReviewerAction needs it, pointed the
	// other way: with the endpoint unreadable this must not claim a review
	// finished. That is the half of the outage nothing used to report — the author
	// was never asked to advance the board either.
	//
	// See domain.MergeRequestSnapshot.ApprovalsKnown for why unreadable happens.
	return linked && snapshot.ApprovalsKnown && len(snapshot.ApprovedBy) > 0 && !hasRequestedChanges(snapshot) &&
		strings.EqualFold(strings.TrimSpace(link.issue.State.Name), linear.InReviewState)
}

// needsReviewerTag applies the Linear readiness gate on top of
// domain.NoReviewersAssigned: a card the author has not offered for review is
// not an MR they forgot to tag, it is an MR that is not ready to be tagged.
//
// Readiness outranks it for the same reason it outranks a standing
// REQUESTED_CHANGES verdict in needsReviewerAction — the author's one useful
// action is moving the card, and a row that asks for two things at once buries
// the one that unblocks the other.
//
// The merge request cannot be lost by this suppression, and the argument is the
// same partition rule needsLinearStart carries: the guard suppressed on here is
// exactly needsLinearStart's condition (linked, before In Review, and the same
// open/non-draft guard NoReviewersAssigned opens with), so every suppression
// leaves an author row that says "move CHAIN-184 to In Review". There is no
// input for which this returns false, actions.Any() is otherwise false and
// startLinear is false — which would be an author row with nothing on it.
func needsReviewerTag(snapshot domain.MergeRequestSnapshot, link linearLink, linked bool) bool {
	if !domain.NoReviewersAssigned(snapshot) {
		return false
	}
	// Written as the same early return needsReviewerAction's readiness gate uses,
	// so the two guards can be read against each other — that they stay identical
	// is the whole argument above.
	if linked && link.stage == linear.StageBeforeReview {
		return false
	}
	return true
}

// needsLinearStart tells the MR author that the linked card is still parked
// before In Review, which is why the digest asked nobody to review it.
//
// The guard is deliberately identical to the one needsReviewerAction inherits
// from domain.NeedsHumanReview (open, not draft), which is what makes this row a
// superset of every readiness suppression rather than a matching set. The
// superset direction is the safe one: an extra author row costs a line, a
// missing one costs the merge request.
//
// Drafts are excluded because a draft whose card is In Progress is simply
// consistent — the author is working, the board says so, and nothing is being
// hidden from anyone since a draft never asks for review in the first place.
func needsLinearStart(snapshot domain.MergeRequestSnapshot, link linearLink, linked bool) bool {
	if !snapshot.MR.IsOpen() || snapshot.MR.Draft {
		return false
	}
	return linked && link.stage == linear.StageBeforeReview
}
