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

type linearDigestState struct {
	enabled       bool
	inReviewCount int
	issuesByMR    map[string]linear.Issue
}

func (s linearDigestState) issueFor(snapshot domain.MergeRequestSnapshot) (linear.Issue, bool) {
	issue, ok := s.issuesByMR[snapshotKey(snapshot.Project.ID, snapshot.MR.IID)]
	return issue, ok
}

type linearIssueMatch struct {
	issue      linear.Issue
	found      bool
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
	var conflicts []string
	for _, candidate := range append(title, branch...) {
		identifier := strings.ToUpper(candidate.Identifier)
		if seen[identifier] {
			continue
		}
		seen[identifier] = true
		conflicts = append(conflicts, candidate.Identifier)
	}
	return linearIssueMatch{issue: chosen, found: true, conflicts: conflicts, matchField: field}
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

// needsReviewerAction applies the Linear completion gate on top of GitLab's
// per-reviewer classifier. One approval completes a linked task for review
// purposes, except that any REQUESTED_CHANGES verdict keeps the ordinary GitLab
// flow active. Missing Linear tasks and zero approvals always fail open.
func needsReviewerAction(snapshot domain.MergeRequestSnapshot, reviewer domain.Reviewer, linked bool) bool {
	if !domain.NeedsHumanReview(snapshot, reviewer) {
		return false
	}
	if !linked || len(snapshot.ApprovedBy) == 0 || hasRequestedChanges(snapshot) {
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
func needsLinearMove(snapshot domain.MergeRequestSnapshot, issue linear.Issue, linked bool) bool {
	if !snapshot.MR.IsOpen() || snapshot.MR.Draft {
		return false
	}
	return linked && len(snapshot.ApprovedBy) > 0 && !hasRequestedChanges(snapshot) &&
		strings.EqualFold(strings.TrimSpace(issue.State.Name), linear.InReviewState)
}
