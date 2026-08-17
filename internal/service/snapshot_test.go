package service

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/sxwebdev/ai-reviewer/internal/domain"
	"github.com/sxwebdev/ai-reviewer/internal/gitlab"
)

func TestParseTime(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want time.Time
	}{
		{"rfc3339 with fractional seconds", "2026-08-13T09:00:00.000Z", time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)},
		{"rfc3339", "2026-08-13T09:00:00Z", time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)},
		{"gitlab numeric offset", "2026-08-13T12:00:00.000+0300", time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)},
		{"empty", "", time.Time{}},
		{"garbage stays unknown rather than the epoch", "not a date", time.Time{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := parseTime(c.in)
			if !got.Equal(c.want) {
				t.Errorf("parseTime(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestLastPushAtTakesTheNewestVersion(t *testing.T) {
	t.Parallel()
	// Deliberately out of order: the answer must not depend on GitLab's
	// ordering of the versions list.
	got := lastPushAt([]gitlab.MergeRequestVersion{
		{CreatedAt: "2026-08-11T09:00:00.000Z"},
		{CreatedAt: "2026-08-13T09:00:00.000Z"},
		{CreatedAt: "2026-08-12T09:00:00.000Z"},
	})
	want := time.Date(2026, 8, 13, 9, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("lastPushAt = %v, want %v", got, want)
	}
	if !lastPushAt(nil).IsZero() {
		t.Error("lastPushAt(nil) must be the zero time, i.e. unknown")
	}
}

// TestMapMergeabilityKnownFlag pins the rule that "false" and "not reported"
// must not look alike: reporting a conflict we are not sure about sends an
// author chasing a problem that does not exist.
func TestMapMergeabilityKnownFlag(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		hasConflicts  bool
		mergeStatus   string
		detailed      string
		wantKnown     bool
		wantConflict  bool
		wantClassKnwn bool
	}{
		{"mergeable", false, "can_be_merged", "mergeable", true, false, true},
		{"conflict reported by both fields", true, "cannot_be_merged", "conflict", true, true, true},
		{"detailed says conflict on its own", false, "cannot_be_merged", "conflict", true, true, true},
		{"unchecked is unknown", false, "unchecked", "unchecked", true, false, false},
		{"checking is unknown", false, "checking", "checking", true, false, false},
		{"recheck without a detailed status is unknown", true, "cannot_be_merged_recheck", "", false, false, false},
		{"legacy-only unchecked is unknown", true, "unchecked", "", true, false, false},
		{"legacy-only can_be_merged trusts has_conflicts", false, "can_be_merged", "", true, false, true},
		{"nothing reported at all", false, "", "", false, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			m := mapMergeability(&gitlab.MergeRequest{
				HasConflicts:        c.hasConflicts,
				MergeStatus:         c.mergeStatus,
				DetailedMergeStatus: c.detailed,
			})
			if m.Known != c.wantKnown {
				t.Errorf("Mergeability.Known = %v, want %v", m.Known, c.wantKnown)
			}
			// The mapping only matters through the classifier, so assert there.
			conflict, known := domain.HasMergeConflicts(domain.MergeRequestSnapshot{Mergeability: m})
			if known != c.wantClassKnwn || conflict != c.wantConflict {
				t.Errorf("HasMergeConflicts = (%v, known=%v), want (%v, known=%v)",
					conflict, known, c.wantConflict, c.wantClassKnwn)
			}
		})
	}
}

func TestSnapshotPipelineKnownOnlyWhenReported(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	proj := testProject()
	mr := testMR(testMRIID, withHeadPipeline(&gitlab.Pipeline{
		ID: 55, SHA: testHeadSHA, Status: "failed", WebURL: "https://pipelines/55",
	}))
	seedGitLab(h.fake, proj, mr)

	loaded, err := h.svc.newSnapshotLoader(depthDigest).load(t.Context(), testTeam, proj, testMRIID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !loaded.Snapshot.Pipeline.Known {
		t.Fatal("head_pipeline was returned, so Pipeline.Known must be true")
	}
	pl, failed, known := domain.FailedPipeline(loaded.Snapshot)
	if !known || !failed {
		t.Fatalf("FailedPipeline = (failed=%v, known=%v), want both true", failed, known)
	}
	if pl.WebURL != "https://pipelines/55" {
		t.Errorf("pipeline web url = %q, want the head pipeline's", pl.WebURL)
	}
	if n := h.gl.pipelineCalls[testMRIID]; n != 0 {
		t.Errorf("/pipelines was called %d times; the fallback must not fire when head_pipeline is present", n)
	}
}

// TestSnapshotPipelineFallbackFiresOncePerMR covers the §17 bullet: the
// /pipelines fallback is used exactly for the MRs whose head_pipeline is
// missing, and exactly once — the snapshot cache is what bounds it.
func TestSnapshotPipelineFallbackFiresOncePerMR(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	proj := testProject()
	visible := testMR(101, withHeadPipeline(&gitlab.Pipeline{ID: 1, SHA: testHeadSHA, Status: "success"}))
	hidden := testMR(102) // head_pipeline absent: no permission to view pipelines
	seedGitLab(h.fake, proj, visible, hidden)
	setPipelines(h.fake, proj, 102, []gitlab.Pipeline{
		{ID: 9, SHA: "older00", Status: "success", WebURL: "https://pipelines/9"},
		{ID: 8, SHA: testHeadSHA, Status: "failed", WebURL: "https://pipelines/8"},
	})

	loader := h.svc.newSnapshotLoader(depthDigest)
	for range 3 {
		for _, iid := range []int64{101, 102} {
			if _, err := loader.load(t.Context(), testTeam, proj, iid); err != nil {
				t.Fatalf("load %d: %v", iid, err)
			}
		}
	}

	if n := h.gl.pipelineCalls[101]; n != 0 {
		t.Errorf("/pipelines called %d times for the MR with a visible head pipeline, want 0", n)
	}
	if n := h.gl.pipelineCalls[102]; n != 1 {
		t.Errorf("/pipelines called %d times for the MR without one, want exactly 1", n)
	}

	loaded, err := loader.load(t.Context(), testTeam, proj, 102)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// The list is newest-first but the head's pipeline is second: picking the
	// matching SHA rather than the newest entry is what makes the fallback
	// useful.
	pl, failed, known := domain.FailedPipeline(loaded.Snapshot)
	if !known || !failed || pl.ID != 8 {
		t.Errorf("FailedPipeline = (%d, failed=%v, known=%v), want pipeline 8 failed and known", pl.ID, failed, known)
	}
}

func TestSnapshotPipelineFallbackStaysUnknownWhenThereAreNoPipelines(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	proj := testProject()
	seedGitLab(h.fake, proj, testMR(testMRIID))

	loaded, err := h.svc.newSnapshotLoader(depthDigest).load(t.Context(), testTeam, proj, testMRIID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.Snapshot.Pipeline.Known {
		t.Error("with no pipelines at all the pipeline must stay unknown, not be reported as absent")
	}
}

// TestSnapshotReviewStatesFromGraphQL walks the review-state matrix through the
// snapshot: the service must map GitLab's enum with domain.ParseReviewState so
// an unrecognised value degrades into the REST fallback.
func TestSnapshotReviewStatesFromGraphQL(t *testing.T) {
	t.Parallel()
	reviewer := gitlab.User{ID: 42, Username: "reviewer"}

	cases := []struct {
		name      string
		state     gitlab.ReviewState
		wantState domain.ReviewState
		wantNeeds bool
	}{
		{"unreviewed", gitlab.ReviewStateUnreviewed, domain.ReviewStateUnreviewed, true},
		{"reviewed", gitlab.ReviewStateReviewed, domain.ReviewStateReviewed, false},
		{"approved", gitlab.ReviewStateApproved, domain.ReviewStateApproved, false},
		// The two states this build used to lose to the REST fallback. Both mean the
		// reviewer still owes a verdict, and both were measured on a live project.
		{"unapproved", gitlab.ReviewStateUnapproved, domain.ReviewStateUnapproved, true},
		{"review started", gitlab.ReviewStateReviewStarted, domain.ReviewStateReviewStarted, true},
		// The reviewer left no note here, so the verdict cannot be dated and the
		// author is the one nudged — ClassifyAuthorActions reports the MR under them.
		{"requested changes, undatable", gitlab.ReviewStateRequestedChanges, domain.ReviewStateRequestedChanges, false},
		// An enum value this build does not know must NOT read as "reviewed"; it
		// falls through to the REST heuristic, which nudges an untouched reviewer.
		{"unknown enum degrades to the REST heuristic", gitlab.ReviewState("SOMETHING_NEW"), domain.ReviewStateUnknown, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			proj := testProject()
			mr := testMR(testMRIID, withReviewers(reviewer))
			seedGitLab(h.fake, proj, mr)
			h.gql.States = map[int64][]gitlab.ReviewerState{
				testMRIID: {{Username: "reviewer", State: c.state}},
			}

			loader := h.svc.newSnapshotLoader(depthDigest)
			loader.primeReviewStates(t.Context(), proj.PathWithNamespace, []int64{testMRIID})
			loaded, err := loader.load(t.Context(), testTeam, proj, testMRIID)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if len(loaded.Snapshot.Reviewers) != 1 {
				t.Fatalf("got %d reviewers, want 1", len(loaded.Snapshot.Reviewers))
			}
			got := loaded.Snapshot.Reviewers[0]
			if got.State != c.wantState {
				t.Errorf("reviewer state = %q, want %q", got.State, c.wantState)
			}
			if needs := domain.NeedsHumanReview(loaded.Snapshot, got); needs != c.wantNeeds {
				t.Errorf("NeedsHumanReview = %v, want %v", needs, c.wantNeeds)
			}
		})
	}
}

// TestSnapshotGraphQLFallbackWarnsOncePerRun covers two §17 requirements at
// once: an unsupported GraphQL degrades to the REST heuristic, and the warning
// is emitted once per run rather than once per merge request.
func TestSnapshotGraphQLFallbackWarnsOncePerRun(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	proj := testProject()
	reviewer := gitlab.User{ID: 42, Username: "reviewer"}
	seedGitLab(h.fake, proj, testMR(101, withReviewers(reviewer)), testMR(102, withReviewers(reviewer)))
	h.gql.Err = errors.New("boom: " + gitlab.ErrGraphQLUnsupported.Error())
	h.gql.Err = gitlab.ErrGraphQLUnsupported

	loader := h.svc.newSnapshotLoader(depthDigest)
	loader.primeReviewStates(t.Context(), proj.PathWithNamespace, []int64{101, 102})
	// A second prime for the same project in the same run must not re-query.
	loader.primeReviewStates(t.Context(), proj.PathWithNamespace, []int64{101, 102})
	if got := h.gql.CallCount(); got != 1 {
		t.Errorf("GraphQL called %d times, want 1 per project per run", got)
	}
	if !loader.gqlWarned {
		t.Error("the degradation must be logged, once")
	}

	loaded, err := loader.load(t.Context(), testTeam, proj, 101)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	r := loaded.Snapshot.Reviewers[0]
	if r.State != domain.ReviewStateUnknown {
		t.Errorf("reviewer state = %q, want the unknown state that selects the REST fallback", r.State)
	}
	if !domain.NeedsHumanReview(loaded.Snapshot, r) {
		t.Error("REST fallback: a reviewer who neither approved nor commented still owes a review")
	}
}

func TestSnapshotRESTFallbackUsesApprovalsAndNotes(t *testing.T) {
	t.Parallel()
	reviewer := gitlab.User{ID: 42, Username: "reviewer"}

	t.Run("approved", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		proj := testProject()
		seedGitLab(h.fake, proj, testMR(testMRIID, withReviewers(reviewer)))
		setApprovals(h.fake, proj, testMRIID, &gitlab.Approvals{
			ApprovedBy: []gitlab.ApprovedBy{{User: reviewer}},
		})

		loaded, err := h.svc.newSnapshotLoader(depthDigest).load(t.Context(), testTeam, proj, testMRIID)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if domain.NeedsHumanReview(loaded.Snapshot, loaded.Snapshot.Reviewers[0]) {
			t.Error("an approver does not owe another review")
		}
	})

	t.Run("commented after the last push", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		proj := testProject()
		seedGitLab(h.fake, proj, testMR(testMRIID, withReviewers(reviewer)))
		setDiscussions(h.fake, proj, testMRIID, []gitlab.Discussion{{
			ID: "d1",
			Notes: []gitlab.Note{{
				ID: 5, Author: reviewer, Body: "looks fine",
				CreatedAt: "2026-08-13T10:00:00.000Z", // after the 09:00 push
			}},
		}})

		loaded, err := h.svc.newSnapshotLoader(depthDigest).load(t.Context(), testTeam, proj, testMRIID)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		snap := loaded.Snapshot
		if domain.NeedsHumanReview(snap, snap.Reviewers[0]) {
			t.Error("a reviewer who commented after the push has acted")
		}
		want := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
		if got := snap.Reviewers[0].LastActivityAt; !got.Equal(want) {
			t.Errorf("LastActivityAt = %v, want %v", got, want)
		}
	})

	t.Run("system notes never clear a reviewer", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		proj := testProject()
		seedGitLab(h.fake, proj, testMR(testMRIID, withReviewers(reviewer)))
		setDiscussions(h.fake, proj, testMRIID, []gitlab.Discussion{{
			ID: "d1",
			Notes: []gitlab.Note{{
				ID: 5, Author: reviewer, System: true, Body: "added 3 commits",
				CreatedAt: "2026-08-13T10:00:00.000Z",
			}},
		}})

		loaded, err := h.svc.newSnapshotLoader(depthDigest).load(t.Context(), testTeam, proj, testMRIID)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		snap := loaded.Snapshot
		// The REST fallback re-scans the discussions itself and skips system notes
		// outright, so it cannot read GitLab's own event log as "the reviewer
		// engaged" — regardless of what LastActivityAt ends up holding.
		if !domain.NeedsHumanReview(snap, snap.Reviewers[0]) {
			t.Error("a system note must not clear a reviewer")
		}
	})

	t.Run("an ordinary note outranks a newer system note", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		proj := testProject()
		seedGitLab(h.fake, proj, testMR(testMRIID, withReviewers(reviewer)))
		// The push is last, so the system note is *usable* by the fallback's clamp:
		// the ordinary note wins on precedence alone, not because the other was
		// discarded for being too new.
		setVersions(h.fake, proj, testMRIID, []gitlab.MergeRequestVersion{
			{ID: 1, HeadCommitSHA: testHeadSHA, CreatedAt: "2026-08-13T12:00:00.000Z"},
		})
		setDiscussions(h.fake, proj, testMRIID, []gitlab.Discussion{{
			ID: "d1",
			Notes: []gitlab.Note{
				{ID: 5, Author: reviewer, Body: "please fix", CreatedAt: "2026-08-13T10:00:00.000Z"},
				{ID: 6, Author: reviewer, System: true, Body: "requested changes", CreatedAt: "2026-08-13T11:00:00.000Z"},
			},
		}})

		loaded, err := h.svc.newSnapshotLoader(depthDigest).load(t.Context(), testTeam, proj, testMRIID)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		// The system-note fallback exists only for reviewers who wrote nothing at
		// all: a real comment is the better evidence and must win even when a
		// system note is newer.
		want := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
		if got := loaded.Snapshot.Reviewers[0].LastActivityAt; !got.Equal(want) {
			t.Errorf("LastActivityAt = %v, want the ordinary note's %v", got, want)
		}
	})
}

// TestSnapshotDatesAnUncommentedRequestedChangesVerdict walks the progression a
// reviewer who clicks "Request changes" and writes nothing goes through.
//
// It used to dead-end. LastActivityAt counted non-system notes only, so such a
// reviewer had none at all, and NeedsHumanReview answers false for an undated
// verdict — which is correct while the ball is with the author but never stops
// being true. The author's own push could not hand the MR back, so the reviewer
// was never nudged again and the author read "changes requested by X" forever:
// the stalled-MR-nobody-chases outcome the pairing was meant to prevent. The
// partition itself held (the author kept the MR), which is why no existing test
// caught it — both sides of the partition were tested, the *progression* was not.
//
// Every stage runs on the GraphQL path (reviewState REQUESTED_CHANGES), which is
// the one where LastActivityAt decides anything: the REST fallback re-scans the
// discussions and skips system notes outright, so it cannot show these bugs.
func TestSnapshotDatesAnUncommentedRequestedChangesVerdict(t *testing.T) {
	t.Parallel()
	reviewer := gitlab.User{ID: 42, Username: "reviewer"}
	// GitLab records the click as a system note authored by the reviewer; the
	// verdict itself (mergeRequestInteraction.reviewState) carries no timestamp.
	verdictNote := gitlab.Note{
		ID: 7, Author: reviewer, System: true, Body: "requested changes",
		CreatedAt: "2026-08-13T09:30:00.000Z",
	}
	verdict := time.Date(2026, 8, 13, 9, 30, 0, 0, time.UTC)
	// The push the reviewer looked at, and the author's answer to the verdict.
	firstPush := gitlab.MergeRequestVersion{ID: 1, HeadCommitSHA: testHeadSHA, CreatedAt: "2026-08-13T09:00:00.000Z"}
	fixPush := gitlab.MergeRequestVersion{ID: 2, HeadCommitSHA: "bbbb222", CreatedAt: "2026-08-13T10:00:00.000Z"}

	// load rebuilds the snapshot from scratch: the loader cache is per-run, and
	// each stage below is a separate digest run over the same merge request. Every
	// note is installed as its own individual_note discussion, which is how GitLab
	// returns system notes.
	load := func(t *testing.T, versions []gitlab.MergeRequestVersion, notes ...gitlab.Note) domain.MergeRequestSnapshot {
		t.Helper()
		h := newHarness(t)
		proj := testProject()
		seedGitLab(h.fake, proj, testMR(testMRIID, withReviewers(reviewer)))
		setVersions(h.fake, proj, testMRIID, versions)
		var ds []gitlab.Discussion
		for i, n := range notes {
			ds = append(ds, gitlab.Discussion{
				ID: "d" + strconv.Itoa(i), IndividualNote: true, Notes: []gitlab.Note{n},
			})
		}
		setDiscussions(h.fake, proj, testMRIID, ds)
		h.gql.States = map[int64][]gitlab.ReviewerState{
			testMRIID: {{Username: "reviewer", State: gitlab.ReviewStateRequestedChanges}},
		}

		loader := h.svc.newSnapshotLoader(depthDigest)
		loader.primeReviewStates(t.Context(), proj.PathWithNamespace, []int64{testMRIID})
		loaded, err := loader.load(t.Context(), testTeam, proj, testMRIID)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if len(loaded.Snapshot.Reviewers) != 1 {
			t.Fatalf("got %d reviewers, want 1", len(loaded.Snapshot.Reviewers))
		}
		return loaded.Snapshot
	}

	// claim asserts the partition and returns which side owns the MR: for a
	// REQUESTED_CHANGES reviewer exactly one section must list it. Both would
	// double-report it in the digest; neither would lose it silently.
	claim := func(t *testing.T, snap domain.MergeRequestSnapshot) (reviewerOwes bool) {
		t.Helper()
		r := snap.Reviewers[0]
		reviewerOwes = domain.NeedsHumanReview(snap, r)
		authorOwes := len(domain.ClassifyAuthorActions(snap).ChangesRequestedBy) > 0
		if reviewerOwes == authorOwes {
			t.Fatalf("partition broken: reviewer owes = %v, author owes = %v; want exactly one",
				reviewerOwes, authorOwes)
		}
		return reviewerOwes
	}

	t.Run("an unanswered verdict stays undated and sits with the author", func(t *testing.T) {
		t.Parallel()
		snap := load(t, []gitlab.MergeRequestVersion{firstPush}, verdictNote)
		// The system note is newer than the last push, so it is not used: a system
		// note may only ever prove a verdict *predates* a push. Dating the verdict
		// from it would be indistinguishable from "the reviewer acted after the
		// push", which is what lets a stray event evict a reviewer who is owed a
		// re-look.
		if got := snap.Reviewers[0].LastActivityAt; !got.IsZero() {
			t.Errorf("LastActivityAt = %v, want the zero time: nothing predating the push says the reviewer engaged", got)
		}
		if claim(t, snap) {
			t.Error("the author has not answered the verdict yet, so it is theirs to act on")
		}
	})

	t.Run("the author's push dates the verdict and hands the MR back", func(t *testing.T) {
		t.Parallel()
		snap := load(t, []gitlab.MergeRequestVersion{firstPush, fixPush}, verdictNote)
		if got := snap.Reviewers[0].LastActivityAt; !got.Equal(verdict) {
			t.Errorf("LastActivityAt = %v, want the verdict's system note at %v", got, verdict)
		}
		if !claim(t, snap) {
			t.Error("the author pushed after the verdict, so the reviewer owes the next look")
		}
	})

	// The regression the first version of this fix introduced: any event GitLab
	// credits to the reviewer dated the "verdict", so one stray act after the
	// author's push parked the MR back in the author's list as "changes requested
	// by R" — where it stayed until the next push, which the author has no reason
	// to make. Work silently lost, and worse than the old behaviour, which at least
	// kept the reviewer in "Reviews needed".
	//
	// The bodies below are real GitLab system notes. None of them is parsed: the
	// rule is positional (newer than the last push ⇒ unusable), so it holds for
	// wording this list does not contain.
	t.Run("a later system note cannot take the MR back from the reviewer", func(t *testing.T) {
		t.Parallel()
		for _, body := range []string{
			"added ~123 label",
			"assigned to @someone-else",
			"requested review from @colleague",
			"added 3 commits",
			"marked this merge request as ready",
			"mentioned in merge request !999",
			"changed milestone to %sprint-42",
		} {
			t.Run(body, func(t *testing.T) {
				t.Parallel()
				stray := gitlab.Note{
					ID: 8, Author: reviewer, System: true, Body: body,
					CreatedAt: "2026-08-13T11:00:00.000Z", // after the author's 10:00 fix
				}
				snap := load(t, []gitlab.MergeRequestVersion{firstPush, fixPush}, verdictNote, stray)
				if got := snap.Reviewers[0].LastActivityAt; !got.Equal(verdict) {
					t.Errorf("LastActivityAt = %v, want the verdict at %v: a later system note is not a re-review",
						got, verdict)
				}
				if !claim(t, snap) {
					t.Errorf("%q after the push must not clear the reviewer — the re-look would be lost", body)
				}
			})
		}
	})
}

func TestSnapshotIsLoadedOncePerMergeRequest(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	proj := testProject()
	seedGitLab(h.fake, proj, testMR(testMRIID))

	loader := h.svc.newSnapshotLoader(depthDigest)
	for range 5 {
		if _, err := loader.load(t.Context(), testTeam, proj, testMRIID); err != nil {
			t.Fatalf("load: %v", err)
		}
	}
	if h.gl.discussionCalls != 1 {
		t.Errorf("discussions fetched %d times, want 1 — one MR is loaded once per run", h.gl.discussionCalls)
	}
}

func TestSnapshotUnresolvedThreads(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	proj := testProject()
	seedGitLab(h.fake, proj, testMR(testMRIID))
	setDiscussions(h.fake, proj, testMRIID, []gitlab.Discussion{
		{ID: "open", Notes: []gitlab.Note{{ID: 1, Resolvable: true}}},
		{ID: "closed", Notes: []gitlab.Note{{ID: 2, Resolvable: true, Resolved: true}}},
		{ID: "comment", IndividualNote: true, Notes: []gitlab.Note{{ID: 3}}},
		{ID: "not-resolvable", Notes: []gitlab.Note{{ID: 4}}},
	})

	loaded, err := h.svc.newSnapshotLoader(depthDigest).load(t.Context(), testTeam, proj, testMRIID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := domain.UnresolvedThreads(loaded.Snapshot); got != 1 {
		t.Errorf("UnresolvedThreads = %d, want 1 (only the open resolvable thread)", got)
	}
}

func TestSnapshotDegradesWhenApprovalsAreRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	proj := testProject()
	seedGitLab(h.fake, proj, testMR(testMRIID, withReviewers(gitlab.User{ID: 42, Username: "reviewer"})))

	// The fake answers approvals from a map; a 403 is simulated by swapping in
	// a client whose approvals call fails.
	failing := &approvalsDenied{countingGitLab: h.gl}
	h.svc.gl = failing

	loaded, err := h.svc.newSnapshotLoader(depthDigest).load(t.Context(), testTeam, proj, testMRIID)
	if err != nil {
		t.Fatalf("load must survive an unavailable approvals endpoint: %v", err)
	}
	if len(loaded.Snapshot.ApprovedBy) != 0 {
		t.Error("no approvals could be read, so none may be reported")
	}
}

type approvalsDenied struct{ *countingGitLab }

func (a *approvalsDenied) GetMRApprovals(_ context.Context, pk string, iid int64) (*gitlab.Approvals, error) {
	return nil, &gitlab.APIError{Status: 403, Path: pk}
}

func TestSnapshotHeadSHAPrefersDiffRefs(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	proj := testProject()
	mr := testMR(testMRIID)
	mr.SHA = "transient"
	mr.DiffRefs.HeadSHA = "anchored"
	seedGitLab(h.fake, proj, mr)

	loaded, err := h.svc.newSnapshotLoader(depthDigest).load(t.Context(), testTeam, proj, testMRIID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.Snapshot.HeadSHA != "anchored" {
		t.Errorf("HeadSHA = %q, want the diff_refs head that positions are anchored to", loaded.Snapshot.HeadSHA)
	}

	mr.DiffRefs.HeadSHA = ""
	loaded2, err := h.svc.newSnapshotLoader(depthDigest).load(t.Context(), testTeam, proj, testMRIID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded2.Snapshot.HeadSHA != "transient" {
		t.Errorf("HeadSHA = %q, want the MR sha when diff_refs carries none", loaded2.Snapshot.HeadSHA)
	}
}
