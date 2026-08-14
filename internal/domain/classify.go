package domain

import "strings"

// Reason explains a NeedsAIReview decision in one word. It is used as the
// `reason` label of ai_reviews_skipped_total, so the set below is closed and
// bounded on purpose: a free-form string here would let an unbounded label
// value blow up Prometheus cardinality.
type Reason string

const (
	// ReasonNeedsReview is the only affirmative reason: review is due.
	ReasonNeedsReview Reason = "needs_review"

	// ReasonDisabled — the team has ai_review turned off.
	ReasonDisabled Reason = "disabled"
	// ReasonNotOpen — the MR is merged or closed; reviewing it helps nobody.
	ReasonNotOpen Reason = "not_open"
	// ReasonDraft — the MR is a draft/WIP; the author is not asking for review yet.
	ReasonDraft Reason = "draft"
	// ReasonNoHeadSHA — no head SHA in the snapshot, so the "already reviewed"
	// check cannot be made. Reviewing anyway would re-review the same MR on
	// every scan, because there would be nothing to record as reviewed.
	ReasonNoHeadSHA Reason = "no_head_sha"
	// ReasonUpToDate — this exact head SHA has already been reviewed.
	ReasonUpToDate Reason = "up_to_date"
	// ReasonHeadMoved — the author pushed between the moment the review was
	// queued and the moment the worker picked it up, so the job's head SHA is no
	// longer the MR's head.
	//
	// NeedsAIReview never returns it: it is not a property of the snapshot but of
	// the gap between a queued job and the live merge request, and only the
	// review worker can see it. It lives here because it is a skip reason like
	// any other and shares the ai_reviews_skipped_total label set. Retargeting
	// the live head instead would break the queue's uniqueness key, which is the
	// queued SHA — two jobs, two keys, one duplicated LLM run.
	ReasonHeadMoved Reason = "head_moved"
)

func (r Reason) String() string { return string(r) }

// SkipReasons is the closed set of reasons a review was skipped, in the order
// NeedsAIReview evaluates them (ReasonHeadMoved last, since the review worker
// rather than NeedsAIReview reports it). Metric label values can be
// pre-registered from it so a reason that never fires still shows up as 0
// rather than as a gap.
var SkipReasons = []Reason{
	ReasonDisabled,
	ReasonNotOpen,
	ReasonDraft,
	ReasonNoHeadSHA,
	ReasonUpToDate,
	ReasonHeadMoved,
}

// NeedsAIReview reports whether an AI review is due for this snapshot, and why.
//
// teamEnabled is the team's ai_review switch; lastReviewedSHA is the head SHA
// of the most recent successful review recorded for this MR (empty when there
// is none). Both are passed in rather than read from the snapshot so the
// function stays a pure decision over config + persisted state + GitLab state.
//
// The checks run cheapest-and-most-general first, and the returned Reason is
// always the FIRST rule that fired — that ordering is part of the metric
// contract, otherwise the same MR would move between reason buckets depending
// on evaluation order. Retry backoff for repeatedly failing reviews (plan §6.5)
// is deliberately not modelled here: it is a property of persisted attempt
// history, not of the MR, and lives in the scheduler.
func NeedsAIReview(s MergeRequestSnapshot, teamEnabled bool, lastReviewedSHA string) (bool, Reason) {
	if !teamEnabled {
		return false, ReasonDisabled
	}
	if !s.MR.IsOpen() {
		return false, ReasonNotOpen
	}
	if s.MR.Draft {
		return false, ReasonDraft
	}
	if s.HeadSHA == "" {
		return false, ReasonNoHeadSHA
	}
	if s.HeadSHA == lastReviewedSHA {
		return false, ReasonUpToDate
	}
	// A different SHA than the one we reviewed means the author pushed: review
	// again. This is also what resets the poison-MR failure counter (§6.5).
	return true, ReasonNeedsReview
}

// NeedsHumanReview reports whether reviewer r still owes an action on this MR.
//
// The authoritative source is the GraphQL review state: REVIEWED and APPROVED
// mean done, UNREVIEWED means the reviewer has not looked yet.
//
// REQUESTED_CHANGES is push-aware. A reviewer who requested changes has already
// acted, and the ball sits with the author until new commits land; listing them
// under "Reviews needed" would nudge exactly the wrong person, and would do so
// for the single most common state in an active MR. So they owe an action only
// once the author has pushed since their last engagement.
//
// There is intentionally no equivalent "REVIEWED but a newer push landed" rule:
// REVIEWED is a terminal verdict the reviewer chose, and re-opening it on every
// push would nag reviewers of long-running MRs forever. GitLab owns resetting
// reviewState when new commits arrive.
func NeedsHumanReview(s MergeRequestSnapshot, r Reviewer) bool {
	// Drafts and merged/closed MRs never ask anyone for a review: the digest is
	// a list of actions people can usefully take right now.
	if !s.MR.IsOpen() || s.MR.Draft {
		return false
	}

	switch r.State {
	case ReviewStateReviewed, ReviewStateApproved:
		return false
	case ReviewStateUnreviewed:
		return true
	case ReviewStateRequestedChanges:
		// No engagement timestamp: we cannot tell whether the verdict predates
		// the push, and a missed nudge (a stalled MR nobody chases) costs more
		// than a spurious one (a line in a digest). The state itself already
		// says this reviewer is engaged with the MR, so asking again is cheap.
		if r.LastActivityAt.IsZero() {
			return true
		}
		return s.LastPushAt.After(r.LastActivityAt)
	}

	// ReviewStateUnknown: graphql_enabled=false, the query failed, or GitLab
	// returned an enum we do not know. Fall back to the REST heuristic (§8).
	return needsHumanReviewREST(s, r.User)
}

// needsHumanReviewREST is the fallback for instances where the GraphQL review
// state is unavailable: the reviewer is considered done if they approved, or if
// they left a non-system note after the last push. System notes are GitLab's
// own event log ("added 3 commits", "assigned to @x") and are not a human act.
//
// A note *before* LastPushAt does not count: the author has pushed since, so
// the reviewer has to look at the new code. That is the re-review-after-push
// rule. When LastPushAt is the zero time (no diff versions were fetched) every
// note is "after" it, which is the conservative direction: we would rather stop
// asking a reviewer who has demonstrably engaged than nag them on no evidence.
func needsHumanReviewREST(s MergeRequestSnapshot, u User) bool {
	for _, a := range s.ApprovedBy {
		if sameUser(a, u) {
			return false
		}
	}
	for _, d := range s.Discussions {
		for _, n := range d.Notes {
			if n.System || !sameUser(n.Author, u) {
				continue
			}
			if n.CreatedAt.After(s.LastPushAt) {
				return false
			}
		}
	}
	return true
}

// sameUser matches two GitLab accounts. IDs win when both sides have one;
// otherwise we fall back to the username, which is unique per instance and
// case-insensitive in GitLab. Names and emails are never used for identity —
// they are display data and are routinely empty or duplicated.
func sameUser(a, b User) bool {
	if a.ID != 0 && b.ID != 0 {
		return a.ID == b.ID
	}
	return a.Username != "" && strings.EqualFold(a.Username, b.Username)
}

// UnresolvedThreads counts the discussions that still need the author's
// attention. See Discussion.IsUnresolvedThread for the rule.
func UnresolvedThreads(s MergeRequestSnapshot) int {
	n := 0
	for _, d := range s.Discussions {
		if d.IsUnresolvedThread() {
			n++
		}
	}
	return n
}

// Merge status values that mean "GitLab has not finished computing
// mergeability yet". They appear in both merge_status and
// detailed_merge_status, and are the reason Mergeability has a Known bit.
var mergeabilityPending = map[string]bool{
	"unchecked": true,
	"checking":  true,
	"preparing": true,
}

// detailedStatusConflict is the detailed_merge_status value that confirms a
// conflict. Only this one confirms it: other blocked statuses
// (discussions_not_resolved, ci_must_pass, not_approved, …) are policy gates,
// not conflicts, and reporting them as conflicts would send authors to rebase
// for no reason.
const detailedStatusConflict = "conflict"

// HasMergeConflicts reports whether the MR conflicts with its target branch and
// whether that is known at all.
//
// Conflicts come from the mergeability fields only — never from pipeline output
// or comment text. When GitLab has not finished computing mergeability
// (unchecked/checking/preparing) or did not report it, the answer is "unknown"
// and the digest stays silent: an unchecked MR is the normal state right after
// a push, and announcing a conflict there would be wrong most of the time.
func HasMergeConflicts(s MergeRequestSnapshot) (conflict bool, known bool) {
	m := s.Mergeability
	if !m.Known {
		return false, false
	}
	status := normalizeStatus(m.DetailedStatus)
	if mergeabilityPending[status] {
		return false, false
	}
	return m.HasConflicts || status == detailedStatusConflict, true
}

// pipelineResults are the pipeline statuses that are a final verdict. Only
// "failed" is a failure; the rest are conclusive non-failures.
//
// canceled/canceling/skipped/manual are explicitly NOT failures: a canceled or
// skipped pipeline is usually a deliberate act (a newer push superseded it, a
// rule skipped it), and "manual" means the pipeline is waiting for someone to
// press a button. None of them mean "the author has something to fix", which is
// the only question the digest asks.
var pipelineResults = map[string]bool{
	"failed":    true,
	"success":   true,
	"canceled":  true,
	"canceling": true,
	"skipped":   true,
	"manual":    true,
}

// pipelineStatusFailed is the single status that counts as a failure.
const pipelineStatusFailed = "failed"

// FailedPipeline reports the MR's head pipeline when it has a conclusive
// verdict for the current head commit.
//
//   - failed is true only for status == "failed".
//   - known is true only when we hold a *current* and *conclusive* pipeline:
//     present, SHA equal to the snapshot's head SHA, and in a terminal status.
//     running/pending/created/preparing/scheduled/waiting_for_* are not a
//     result yet, and an unrecognised status is treated the same way — we do
//     not report a verdict we cannot interpret.
//   - A pipeline whose SHA differs from the head SHA is stale: it belongs to a
//     previous push. Reporting it would tell the author to fix a failure they
//     have already fixed, which is exactly the noise that kills a digest's
//     credibility. Stale therefore means known=false, not failed=false.
//
// The returned Pipeline is the zero value whenever known is false, so a caller
// that forgets to check known cannot render a link to a stale or unrelated
// pipeline. When known is true it carries WebURL, which the digest links to so
// the author lands on the failing pipeline rather than on the MR.
//
// Note that Pipeline.Known (the field) and the returned known differ: the field
// only says head_pipeline was present in the response, and it is what drives
// the one-shot GET /merge_requests/:iid/pipelines fallback for MRs where the
// field is missing because of pipeline visibility permissions.
func FailedPipeline(s MergeRequestSnapshot) (p Pipeline, failed bool, known bool) {
	pl := s.Pipeline
	if !pl.Known {
		return Pipeline{}, false, false
	}
	// An empty head SHA gives us nothing to compare against, so freshness is
	// unprovable — treat it like a stale pipeline rather than trusting it.
	if s.HeadSHA == "" || pl.SHA != s.HeadSHA {
		return Pipeline{}, false, false
	}
	status := normalizeStatus(pl.Status)
	if !pipelineResults[status] {
		return Pipeline{}, false, false
	}
	return pl, status == pipelineStatusFailed, true
}

// normalizeStatus makes status comparisons robust to whitespace and casing.
// GitLab sends lower-case values today; this keeps a future change from
// silently turning every status into "unrecognised".
func normalizeStatus(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// AuthorActions is what the digest needs from an MR to render its
// "Author actions" entry. Field order mirrors the fixed line order of the
// digest (threads → conflicts → pipeline, plan §13.3) so the rendering stays
// identical from day to day.
type AuthorActions struct {
	UnresolvedThreads int
	HasConflicts      bool
	PipelineFailed    bool
	Pipeline          Pipeline // set only when PipelineFailed; carries WebURL for the link
}

// Any reports whether the MR belongs in the digest's "Author actions" section:
// at least one of unresolved threads, merge conflicts, failed pipeline.
func (a AuthorActions) Any() bool {
	return a.UnresolvedThreads > 0 || a.HasConflicts || a.PipelineFailed
}

// ClassifyAuthorActions runs the three author-facing classifiers over one
// snapshot. Unknown answers (mergeability not computed, pipeline stale or
// missing) are reported as "no action" — the digest only ever states things it
// is sure of.
func ClassifyAuthorActions(s MergeRequestSnapshot) AuthorActions {
	a := AuthorActions{UnresolvedThreads: UnresolvedThreads(s)}
	if conflict, known := HasMergeConflicts(s); known && conflict {
		a.HasConflicts = true
	}
	if pl, failed, known := FailedPipeline(s); known && failed {
		a.PipelineFailed = true
		a.Pipeline = pl
	}
	return a
}

// NeedsAuthorAction is the shorthand predicate for "this MR belongs in
// Author actions".
func NeedsAuthorAction(s MergeRequestSnapshot) bool { return ClassifyAuthorActions(s).Any() }
