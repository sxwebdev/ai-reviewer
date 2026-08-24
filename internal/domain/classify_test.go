package domain

import (
	"reflect"
	"slices"
	"testing"
	"time"
)

var (
	pushedAt = time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	beforeMR = pushedAt.Add(-2 * time.Hour)
	afterMR  = pushedAt.Add(2 * time.Hour)

	reviewerUser = User{ID: 42, Username: "reviewer"}
	otherUser    = User{ID: 99, Username: "someone-else"}
)

// openSnap is a plain open, non-draft MR with nothing else going on: every
// classifier below starts from it and changes exactly the field under test.
func openSnap() MergeRequestSnapshot {
	return MergeRequestSnapshot{
		Team:    "payments",
		Project: Project{ID: 7, FullPath: "backend/payments"},
		MR: MergeRequest{
			ID:        1001,
			IID:       481,
			ProjectID: 7,
			State:     "opened",
			Title:     "Add payment retries",
			Author:    User{ID: 1, Username: "author"},
			WebURL:    "https://gitlab.example.com/backend/payments/-/merge_requests/481",
		},
		HeadSHA:    "aaa111",
		LastPushAt: pushedAt,
	}
}

// thread builds a resolvable discussion thread rooted at a note by author.
func thread(id string, resolved bool, notes ...Note) Discussion {
	root := Note{ID: 1, Author: otherUser, Resolvable: true, Resolved: resolved, CreatedAt: afterMR}
	return Discussion{ID: id, Notes: append([]Note{root}, notes...)}
}

// ---------------------------------------------------------------- AI review

func TestNeedsAIReview(t *testing.T) {
	draft := openSnap()
	draft.MR.Draft = true

	merged := openSnap()
	merged.MR.State = "merged"

	closed := openSnap()
	closed.MR.State = "closed"

	noSHA := openSnap()
	noSHA.HeadSHA = ""

	disabledDraft := openSnap()
	disabledDraft.MR.Draft = true

	cases := []struct {
		name            string
		snap            MergeRequestSnapshot
		teamEnabled     bool
		lastReviewedSHA string
		want            bool
		wantReason      Reason
	}{
		{"no review recorded yet", openSnap(), true, "", true, ReasonNeedsReview},
		{"same head sha already reviewed", openSnap(), true, "aaa111", false, ReasonUpToDate},
		{"new push re-reviews", openSnap(), true, "old000", true, ReasonNeedsReview},
		{"draft", draft, true, "", false, ReasonDraft},
		{"merged", merged, true, "", false, ReasonNotOpen},
		{"closed", closed, true, "", false, ReasonNotOpen},
		{"team switched off", openSnap(), false, "", false, ReasonDisabled},
		{"no head sha", noSHA, true, "", false, ReasonNoHeadSHA},
		// Ordering is part of the metric contract: the first rule that fires wins,
		// so a disabled team never shows up under "draft".
		{"disabled beats draft", disabledDraft, false, "", false, ReasonDisabled},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, reason := NeedsAIReview(c.snap, c.teamEnabled, c.lastReviewedSHA)
			if got != c.want || reason != c.wantReason {
				t.Errorf("NeedsAIReview = (%v, %q), want (%v, %q)", got, reason, c.want, c.wantReason)
			}
			// The metric label is the raw reason string, unadorned.
			if reason.String() != string(reason) {
				t.Errorf("Reason.String() = %q, want %q", reason.String(), string(reason))
			}
			// Every negative reason must be a declared metric label value.
			if !got && !slices.Contains(SkipReasons, reason) {
				t.Errorf("reason %q is not in SkipReasons %v", reason, SkipReasons)
			}
			if got && reason != ReasonNeedsReview {
				t.Errorf("affirmative decision must report %q, got %q", ReasonNeedsReview, reason)
			}
		})
	}
}

// ------------------------------------------------------------- human review

func TestNeedsHumanReviewGraphQLStates(t *testing.T) {
	// The reviewer has neither approved nor commented, so the REST fallback
	// would answer "needs review" for every state below. Anything that comes back
	// false therefore proves the GraphQL state is what decided.
	cases := []struct {
		state ReviewState
		want  bool
	}{
		{ReviewStateUnreviewed, true},
		// Withdrawing an approval asks for a fresh verdict; starting a review is
		// not finishing one. Both used to fall through to the REST fallback, which
		// answers "done" for a reviewer who commented after the last push — so a
		// reviewer who un-approved could silently leave the digest.
		{ReviewStateUnapproved, true},
		{ReviewStateReviewStarted, true},
		// LastActivityAt is zero here: the verdict cannot be dated, so the author is
		// nudged instead (ClassifyAuthorActions lists the MR under them).
		// TestNeedsHumanReviewRequestedChangesIsPushAware covers the timestamped
		// cases.
		{ReviewStateRequestedChanges, false},
		{ReviewStateReviewed, false},
		{ReviewStateApproved, false},
	}
	for _, c := range cases {
		t.Run(string(c.state), func(t *testing.T) {
			got := NeedsHumanReview(openSnap(), Reviewer{User: reviewerUser, State: c.state})
			if got != c.want {
				t.Errorf("NeedsHumanReview(state=%s) = %v, want %v", c.state, got, c.want)
			}
		})
	}
}

func TestNeedsHumanReviewRequestedChangesIsPushAware(t *testing.T) {
	// A reviewer who requested changes has acted: the ball is with the author
	// until new commits land, so they must not appear under "Reviews needed".
	cases := []struct {
		name           string
		lastActivityAt time.Time
		want           bool
	}{
		{"no push since the reviewer's last activity", afterMR, false},
		// Exact ties resolve as "no new push": the comparison is strict.
		{"reviewer engaged exactly at the push instant", pushedAt, false},
		{"author pushed after the reviewer requested changes", beforeMR, true},
		// An undatable verdict goes to the author, not back to the reviewer. This
		// is only safe because ClassifyAuthorActions now reports the same state —
		// TestAuthorActionsIncludeRequestedChanges is the other half, and the MR
		// would fall out of the digest entirely if either side stopped.
		{"engagement time unknown", time.Time{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := Reviewer{User: reviewerUser, State: ReviewStateRequestedChanges, LastActivityAt: c.lastActivityAt}
			if got := NeedsHumanReview(openSnap(), r); got != c.want {
				t.Errorf("NeedsHumanReview(REQUESTED_CHANGES, activity=%v) = %v, want %v",
					c.lastActivityAt, got, c.want)
			}
		})
	}
}

func TestNeedsHumanReviewIgnoresDraftAndClosedMRs(t *testing.T) {
	cases := []struct {
		name  string
		state string
		draft bool
		want  bool
	}{
		{"open", "opened", false, true},
		{"locked discussion is still open", "locked", false, true},
		{"draft", "opened", true, false},
		{"merged", "merged", false, false},
		{"closed", "closed", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := openSnap()
			s.MR.State = c.state
			s.MR.Draft = c.draft
			// UNREVIEWED would be "needs review" on an open MR, so a false here
			// can only come from the draft/closed gate.
			got := NeedsHumanReview(s, Reviewer{User: reviewerUser, State: ReviewStateUnreviewed})
			if got != c.want {
				t.Errorf("NeedsHumanReview = %v, want %v", got, c.want)
			}
		})
	}
}

func TestNeedsHumanReviewRESTFallback(t *testing.T) {
	note := func(author User, system bool, at time.Time) Discussion {
		return Discussion{ID: "d", Notes: []Note{{ID: 5, Author: author, System: system, CreatedAt: at}}}
	}

	cases := []struct {
		name       string
		approvedBy []User
		discussion []Discussion
		want       bool
	}{
		{"no approval and no notes", nil, nil, true},
		{"approved", []User{reviewerUser}, nil, false},
		{"non-system note after the last push", nil, []Discussion{note(reviewerUser, false, afterMR)}, false},
		// The author pushed after the reviewer's comment: it is stale, re-review.
		{"note predates the last push", nil, []Discussion{note(reviewerUser, false, beforeMR)}, true},
		{"system note does not count", nil, []Discussion{note(reviewerUser, true, afterMR)}, true},
		{"someone else's note does not count", nil, []Discussion{note(otherUser, false, afterMR)}, true},
		{"approval matched by username when ids are absent",
			[]User{{Username: "REVIEWER"}}, nil, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := openSnap()
			s.ApprovedBy = c.approvedBy
			s.Discussions = c.discussion
			// Unknown state = GraphQL disabled or failed.
			got := NeedsHumanReview(s, Reviewer{User: reviewerUser, State: ReviewStateUnknown})
			if got != c.want {
				t.Errorf("NeedsHumanReview (REST fallback) = %v, want %v", got, c.want)
			}
		})
	}
}

func TestNeedsHumanReviewFallbackCountsNotesOutsideThreads(t *testing.T) {
	// A standalone comment is not an unresolved *thread*, but it is still proof
	// the reviewer engaged after the push.
	s := openSnap()
	s.Discussions = []Discussion{{
		ID:             "d1",
		IndividualNote: true,
		Notes:          []Note{{ID: 9, Author: reviewerUser, CreatedAt: afterMR}},
	}}
	if NeedsHumanReview(s, Reviewer{User: reviewerUser, State: ReviewStateUnknown}) {
		t.Error("a non-system standalone comment after the push must clear the reviewer")
	}
}

func TestParseReviewState(t *testing.T) {
	cases := []struct {
		in   string
		want ReviewState
	}{
		{"UNREVIEWED", ReviewStateUnreviewed},
		{"REVIEWED", ReviewStateReviewed},
		{"REQUESTED_CHANGES", ReviewStateRequestedChanges},
		{"APPROVED", ReviewStateApproved},
		// Both are live values on gitlab.com — 7 of 57 reviewer states on one
		// project — and both used to land in the REST fallback because this
		// function did not know them while gitlab.ReviewState did.
		{"UNAPPROVED", ReviewStateUnapproved},
		{"REVIEW_STARTED", ReviewStateReviewStarted},
		{"", ReviewStateUnknown},
		// A value we genuinely do not know must degrade to the REST fallback, never
		// to "already reviewed".
		{"SOMETHING_NEW", ReviewStateUnknown},
		{"approved", ReviewStateUnknown},
	}
	for _, c := range cases {
		if got := ParseReviewState(c.in); got != c.want {
			t.Errorf("ParseReviewState(%q) = %q, want %q", c.in, got, c.want)
		}
		if got := ParseReviewState(c.in).Known(); got != (c.want != ReviewStateUnknown) {
			t.Errorf("ParseReviewState(%q).Known() = %v", c.in, got)
		}
	}
}

// -------------------------------------------------------------- discussions

func TestUnresolvedThreads(t *testing.T) {
	cases := []struct {
		name        string
		discussions []Discussion
		want        int
	}{
		{"unresolved resolvable thread", []Discussion{thread("d1", false)}, 1},
		{"resolved thread", []Discussion{thread("d1", true)}, 0},
		{
			"individual note is not a thread",
			[]Discussion{{ID: "d1", IndividualNote: true, Notes: []Note{{Resolvable: true}}}},
			0,
		},
		{
			"thread rooted in a non-resolvable note",
			[]Discussion{{ID: "d1", Notes: []Note{{Resolvable: false}}}},
			0,
		},
		{"discussion with no notes", []Discussion{{ID: "d1"}}, 0},
		{
			"a later resolved note closes the thread",
			[]Discussion{thread("d1", false, Note{ID: 2, Resolvable: true, Resolved: true})},
			0,
		},
		{
			"three threads, one resolved",
			[]Discussion{thread("d1", false), thread("d2", true), thread("d3", false)},
			2,
		},
		{
			"threads mixed with plain comments",
			[]Discussion{
				thread("d1", false),
				{ID: "d2", IndividualNote: true, Notes: []Note{{Resolvable: true}}},
			},
			1,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := openSnap()
			s.Discussions = c.discussions
			if got := UnresolvedThreads(s); got != c.want {
				t.Errorf("UnresolvedThreads = %d, want %d", got, c.want)
			}
		})
	}
}

// ----------------------------------------------------------------- conflicts

func TestHasMergeConflicts(t *testing.T) {
	cases := []struct {
		name         string
		mergeability Mergeability
		wantConflict bool
		wantKnown    bool
	}{
		{"conflict confirmed by both fields",
			Mergeability{HasConflicts: true, DetailedStatus: "conflict", Known: true}, true, true},
		{"has_conflicts without a detailed status",
			Mergeability{HasConflicts: true, Known: true}, true, true},
		{"detailed status alone confirms",
			Mergeability{DetailedStatus: "conflict", Known: true}, true, true},
		{"mergeable",
			Mergeability{DetailedStatus: "mergeable", Known: true}, false, true},
		// Blocked for policy reasons, not conflicting: the author does not need
		// to rebase.
		{"blocked on discussions is not a conflict",
			Mergeability{DetailedStatus: "discussions_not_resolved", Known: true}, false, true},
		{"unchecked", Mergeability{DetailedStatus: "unchecked", Known: true}, false, false},
		{"checking", Mergeability{DetailedStatus: "checking", Known: true}, false, false},
		{"preparing", Mergeability{DetailedStatus: "preparing", Known: true}, false, false},
		// Mergeability is still being computed, so has_conflicts is not
		// trustworthy yet — report unknown rather than a conflict.
		{"has_conflicts while still checking",
			Mergeability{HasConflicts: true, DetailedStatus: "Checking", Known: true}, false, false},
		{"mergeability not reported at all", Mergeability{}, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := openSnap()
			s.Mergeability = c.mergeability
			conflict, known := HasMergeConflicts(s)
			if conflict != c.wantConflict || known != c.wantKnown {
				t.Errorf("HasMergeConflicts = (%v, %v), want (%v, %v)",
					conflict, known, c.wantConflict, c.wantKnown)
			}
		})
	}
}

// ------------------------------------------------------------------ pipeline

func TestFailedPipelineStatuses(t *testing.T) {
	cases := []struct {
		status     string
		wantFailed bool
		wantKnown  bool
	}{
		{"failed", true, true},
		{"FAILED", true, true}, // casing must not change the verdict
		{"success", false, true},
		// Deliberate non-failures: nothing here says "the author must fix
		// something". Easy to confuse with "failed" — hence the explicit rows.
		{"canceled", false, true},
		{"canceling", false, true},
		{"skipped", false, true},
		{"manual", false, true},
		// Not a result yet.
		{"running", false, false},
		{"pending", false, false},
		{"created", false, false},
		{"preparing", false, false},
		{"scheduled", false, false},
		{"waiting_for_resource", false, false},
		{"waiting_for_callback", false, false},
		// Unknown to this build: refuse to interpret it.
		{"brand_new_status", false, false},
		{"", false, false},
	}
	for _, c := range cases {
		t.Run(c.status, func(t *testing.T) {
			s := openSnap()
			s.Pipeline = Pipeline{ID: 5, SHA: s.HeadSHA, Status: c.status, WebURL: "https://ci/5", Known: true}
			p, failed, known := FailedPipeline(s)
			if failed != c.wantFailed || known != c.wantKnown {
				t.Errorf("FailedPipeline(%q) = (failed=%v, known=%v), want (%v, %v)",
					c.status, failed, known, c.wantFailed, c.wantKnown)
			}
			if !known && p != (Pipeline{}) {
				t.Errorf("an unknown verdict must return the zero Pipeline, got %+v", p)
			}
			if known && p.ID != 5 {
				t.Errorf("a known verdict must return the pipeline, got %+v", p)
			}
		})
	}
}

func TestFailedPipelineFreshness(t *testing.T) {
	cases := []struct {
		name      string
		headSHA   string
		pipeline  Pipeline
		wantKnown bool
	}{
		{
			"failed pipeline for the head commit",
			"aaa111",
			Pipeline{ID: 5, SHA: "aaa111", Status: "failed", WebURL: "https://ci/5", Known: true},
			true,
		},
		{
			// The author already pushed a fix; showing the old failure would
			// send them chasing a problem that no longer exists.
			"failed pipeline from a previous push is stale",
			"aaa111",
			Pipeline{ID: 4, SHA: "old000", Status: "failed", WebURL: "https://ci/4", Known: true},
			false,
		},
		{
			// head_pipeline is only exposed when the token can view pipelines.
			"no head pipeline in the response",
			"aaa111",
			Pipeline{},
			false,
		},
		{
			"no head sha to compare against",
			"",
			Pipeline{ID: 5, SHA: "aaa111", Status: "failed", Known: true},
			false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := openSnap()
			s.HeadSHA = c.headSHA
			s.Pipeline = c.pipeline
			p, failed, known := FailedPipeline(s)
			if known != c.wantKnown {
				t.Fatalf("FailedPipeline known = %v, want %v", known, c.wantKnown)
			}
			if !known {
				if failed || p != (Pipeline{}) {
					t.Errorf("unknown verdict leaked data: failed=%v pipeline=%+v", failed, p)
				}
				return
			}
			if !failed {
				t.Error("a current failed pipeline must report failed=true")
			}
			// The digest links straight to the failing pipeline, not to the MR.
			if p.WebURL != "https://ci/5" {
				t.Errorf("web_url must survive for the digest link, got %q", p.WebURL)
			}
		})
	}
}

// ------------------------------------------------------------ author actions

func TestClassifyAuthorActions(t *testing.T) {
	conflicting := Mergeability{HasConflicts: true, DetailedStatus: "conflict", Known: true}
	failing := Pipeline{ID: 5, SHA: "aaa111", Status: "failed", WebURL: "https://ci/5", Known: true}
	stale := Pipeline{ID: 4, SHA: "old000", Status: "failed", WebURL: "https://ci/4", Known: true}

	cases := []struct {
		name         string
		discussions  []Discussion
		mergeability Mergeability
		pipeline     Pipeline
		want         AuthorActions
	}{
		{"nothing to do", nil, Mergeability{DetailedStatus: "mergeable", Known: true}, Pipeline{}, AuthorActions{}},
		{
			"unresolved threads only",
			[]Discussion{thread("d1", false), thread("d2", false)},
			Mergeability{DetailedStatus: "mergeable", Known: true}, Pipeline{},
			AuthorActions{UnresolvedThreads: 2},
		},
		{
			"conflicts only", nil, conflicting, Pipeline{},
			AuthorActions{HasConflicts: true},
		},
		{
			// Explicitly listed in the plan: a clean MR with a failed pipeline
			// still belongs in Author actions.
			"failed pipeline only", nil, Mergeability{DetailedStatus: "mergeable", Known: true}, failing,
			AuthorActions{PipelineFailed: true, Pipeline: failing},
		},
		{
			"all three",
			[]Discussion{thread("d1", false)}, conflicting, failing,
			AuthorActions{UnresolvedThreads: 1, HasConflicts: true, PipelineFailed: true, Pipeline: failing},
		},
		{
			// Unknown answers are never reported as problems.
			"unknown mergeability and a stale failure",
			nil, Mergeability{DetailedStatus: "checking", Known: true}, stale,
			AuthorActions{},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := openSnap()
			s.Discussions = c.discussions
			s.Mergeability = c.mergeability
			s.Pipeline = c.pipeline
			// Every case here is about a merge request somebody was asked to look
			// at; the untagged case is a rule of its own, in TestNoReviewersAssigned.
			// Without this the whole table would carry NoReviewers: true and say
			// nothing about the flags it exists to check.
			s.Reviewers = []Reviewer{{User: reviewerUser, State: ReviewStateReviewed}}

			got := ClassifyAuthorActions(s)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("ClassifyAuthorActions = %+v, want %+v", got, c.want)
			}
			wantAny := !reflect.DeepEqual(c.want, AuthorActions{})
			if got.Any() != wantAny {
				t.Errorf("Any() = %v, want %v", got.Any(), wantAny)
			}
			if NeedsAuthorAction(s) != wantAny {
				t.Errorf("NeedsAuthorAction = %v, want %v", NeedsAuthorAction(s), wantAny)
			}
		})
	}
}

// TestAuthorActionsIncludeRequestedChanges is the half of the digest that was
// missing entirely.
//
// ClassifyAuthorActions never looked at s.Reviewers, so "a reviewer asked for
// changes" reached the author only when the reviewer also happened to leave a
// resolvable thread. A verdict with no comment, or one whose threads were all
// resolved, put the MR in neither section — NeedsHumanReview said the reviewer
// was done and nothing said the author owed anything.
func TestAuthorActionsIncludeRequestedChanges(t *testing.T) {
	reviewer := func(state ReviewState, activity time.Time) Reviewer {
		return Reviewer{User: reviewerUser, State: state, LastActivityAt: activity}
	}

	cases := []struct {
		name      string
		reviewers []Reviewer
		wantUsers []User
	}{
		{
			// The case that used to vanish: no threads, no conflicts, no pipeline,
			// and an undatable verdict.
			name:      "undatable verdict still reaches the author",
			reviewers: []Reviewer{reviewer(ReviewStateRequestedChanges, time.Time{})},
			wantUsers: []User{reviewerUser},
		},
		{
			name:      "verdict newer than the last push",
			reviewers: []Reviewer{reviewer(ReviewStateRequestedChanges, afterMR)},
			wantUsers: []User{reviewerUser},
		},
		{
			// Answered by a push: the ball is back with the reviewer, who is listed
			// under "Reviews needed". Reporting both would double-count the MR.
			name:      "verdict superseded by a push",
			reviewers: []Reviewer{reviewer(ReviewStateRequestedChanges, beforeMR)},
			wantUsers: nil,
		},
		{
			name:      "other states are not author actions",
			reviewers: []Reviewer{reviewer(ReviewStateUnreviewed, time.Time{}), reviewer(ReviewStateApproved, afterMR)},
			wantUsers: nil,
		},
		{
			name: "two reviewers, one of them superseded",
			reviewers: []Reviewer{
				reviewer(ReviewStateRequestedChanges, afterMR),
				{User: otherUser, State: ReviewStateRequestedChanges, LastActivityAt: beforeMR},
			},
			wantUsers: []User{reviewerUser},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := openSnap()
			s.Reviewers = c.reviewers

			got := ClassifyAuthorActions(s)
			if !reflect.DeepEqual(got.ChangesRequestedBy, c.wantUsers) {
				t.Errorf("ChangesRequestedBy = %+v, want %+v", got.ChangesRequestedBy, c.wantUsers)
			}
			// Any() is what puts the MR in the digest at all, and there is nothing
			// else to report on this snapshot.
			if want := len(c.wantUsers) > 0; got.Any() != want {
				t.Errorf("Any() = %v, want %v: the merge request would be %s the digest",
					got.Any(), want, map[bool]string{true: "missing from", false: "wrongly listed in"}[want])
			}
		})
	}
}

// claimant returns which side of the digest claims the merge request for
// reviewer r, failing when the two rules do not partition it: both would
// double-report the MR, neither would lose it silently.
func claimant(t *testing.T, s MergeRequestSnapshot, r Reviewer) (reviewerOwes bool) {
	t.Helper()
	reviewerOwes = NeedsHumanReview(s, r)
	authorOwes := len(ClassifyAuthorActions(s).ChangesRequestedBy) > 0
	if reviewerOwes == authorOwes {
		t.Fatalf("partition broken: reviewer owes = %v, author owes = %v; want exactly one",
			reviewerOwes, authorOwes)
	}
	return reviewerOwes
}

// TestAuthorActionsAndReviewsNeededDoNotOverlap pins the pairing the two rules
// rely on: for a REQUESTED_CHANGES reviewer, exactly one side must claim the
// merge request. Both would double-report it; neither would lose it.
func TestAuthorActionsAndReviewsNeededDoNotOverlap(t *testing.T) {
	cases := []struct {
		name             string
		activity         time.Time
		wantReviewerOwes bool
	}{
		{"author pushed after the verdict", beforeMR, true},
		{"verdict at the push instant", pushedAt, false},
		{"verdict newer than the last push", afterMR, false},
		// The residual undated case. The service now dates an uncommented verdict
		// from the reviewer's own system note, so zero means it could not read the
		// discussions at all — not "the reviewer wrote nothing". The author keeps
		// the MR, and TestUncommentedVerdictIsReachableAfterAPush is what stops
		// that from being a permanent dead end.
		{"engagement time unknown", time.Time{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := openSnap()
			r := Reviewer{User: reviewerUser, State: ReviewStateRequestedChanges, LastActivityAt: c.activity}
			s.Reviewers = []Reviewer{r}

			if got := claimant(t, s, r); got != c.wantReviewerOwes {
				t.Errorf("activity=%v: reviewer owes = %v, want %v", c.activity, got, c.wantReviewerOwes)
			}
		})
	}
}

// TestUncommentedVerdictIsReachableAfterAPush walks the progression of a
// REQUESTED_CHANGES verdict from "the author owes a fix" to "the reviewer owes a
// re-look", which is where the pairing used to dead-end.
//
// Both static halves of the partition were tested; the progression between them
// was not. LastActivityAt was fed by non-system notes only, so a reviewer who
// clicked "Request changes" without writing a word stayed at zero forever, the
// push-aware comparison was never reached, and the MR sat under the author for the
// rest of its life — nobody chasing the reviewer.
func TestUncommentedVerdictIsReachableAfterAPush(t *testing.T) {
	verdictAt := afterMR // the verdict lands after the push the reviewer looked at

	// The two ways a verdict is dated by the time it reaches this package. The
	// uncommented one is only datable once the author's push proves the verdict is
	// the older of the two — a system note newer than the push cannot be trusted to
	// mean "the reviewer acted", so service.lastActivityAt reports zero until then.
	cases := []struct {
		name        string
		unanswered  time.Time // LastActivityAt while the verdict stands
		afterAnswer time.Time // LastActivityAt once the author has pushed
	}{
		{"reviewer left a comment", verdictAt, verdictAt},
		{"reviewer wrote nothing", time.Time{}, verdictAt},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Step 1: the verdict is fresher than the last push. The author owes the fix.
			s := openSnap()
			r := Reviewer{User: reviewerUser, State: ReviewStateRequestedChanges, LastActivityAt: c.unanswered}
			s.Reviewers = []Reviewer{r}
			if claimant(t, s, r) {
				t.Fatal("an unanswered verdict belongs to the author, not back to the reviewer")
			}

			// Step 2: the author pushes the fix. Ownership must flip — exactly once,
			// and without the MR passing through a state where nobody claims it
			// (claimant fails on that).
			s.LastPushAt = verdictAt.Add(time.Hour)
			r.LastActivityAt = c.afterAnswer
			s.Reviewers = []Reviewer{r}
			if !claimant(t, s, r) {
				t.Error("after the author pushes, the reviewer owes the next look")
			}
		})
	}
}

// TestNoReviewersAssigned pins the rule that puts an untagged merge request into
// the digest at all. Before it, such an MR was in neither section: no reviewer
// means NeedsHumanReview asks nobody, and none of the other author actions
// describe an MR whose only problem is that nothing has happened to it.
func TestNoReviewersAssigned(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		mutate  func(*MergeRequestSnapshot)
		want    bool
		wantWhy string
	}{
		{
			"nobody was asked and nobody approved",
			func(*MergeRequestSnapshot) {},
			true,
			"the whole point of the rule",
		},
		{
			"a reviewer is assigned",
			func(s *MergeRequestSnapshot) {
				s.Reviewers = []Reviewer{{User: reviewerUser, State: ReviewStateUnreviewed}}
			},
			false,
			"somebody was asked; whether they answered is NeedsHumanReview's question",
		},
		{
			// The reviewer is done, but they were still tagged: this MR is not the
			// one nobody was handed.
			"a reviewer is assigned and has reviewed",
			func(s *MergeRequestSnapshot) {
				s.Reviewers = []Reviewer{{User: reviewerUser, State: ReviewStateReviewed}}
			},
			false,
			"an answered review is not an untagged merge request",
		},
		{
			"approved without ever being assigned",
			func(s *MergeRequestSnapshot) {
				s.ApprovedBy = []User{reviewerUser}
				s.ApprovalsKnown = true
			},
			false,
			"it was reviewed anyway; asking for a reviewer asks for a second review",
		},
		{
			// The unreadable-/approvals case: ApprovedBy is empty because the
			// question could not be asked. This rule reads that as "no approval" on
			// purpose — an extra line is visibly wrong to the author, silence is not.
			"approvals unreadable",
			func(s *MergeRequestSnapshot) { s.ApprovalsKnown = false },
			true,
			"fails open into saying something",
		},
		{
			"draft",
			func(s *MergeRequestSnapshot) { s.MR.Draft = true },
			false,
			"a draft with no reviewer is an author still working",
		},
		{
			"merged",
			func(s *MergeRequestSnapshot) { s.MR.State = "merged" },
			false,
			"the digest lists actions somebody can still take",
		},
		{
			"closed",
			func(s *MergeRequestSnapshot) { s.MR.State = "closed" },
			false,
			"the digest lists actions somebody can still take",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := openSnap()
			c.mutate(&s)
			if got := NoReviewersAssigned(s); got != c.want {
				t.Errorf("NoReviewersAssigned = %v, want %v (%s)", got, c.want, c.wantWhy)
			}
			// The flag has to reach the digest through the aggregate, which is the
			// only thing digestData reads.
			if got := ClassifyAuthorActions(s).NoReviewers; got != c.want {
				t.Errorf("ClassifyAuthorActions().NoReviewers = %v, want %v", got, c.want)
			}
		})
	}
}

// TestNoReviewersAloneIsAnAuthorAction: the flag is worthless if it does not put
// the merge request in the digest by itself — an untagged MR has, by definition,
// nothing else wrong with it yet.
func TestNoReviewersAloneIsAnAuthorAction(t *testing.T) {
	t.Parallel()
	s := openSnap()
	s.Mergeability = Mergeability{DetailedStatus: "mergeable", Known: true}
	if !NeedsAuthorAction(s) {
		t.Error("an untagged merge request must belong to its author's section on its own")
	}
}
