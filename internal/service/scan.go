package service

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/sxwebdev/ai-reviewer/internal/domain"
	"github.com/sxwebdev/ai-reviewer/internal/gitlab"
	"github.com/sxwebdev/ai-reviewer/internal/metrics"
	"github.com/sxwebdev/ai-reviewer/internal/store/repos/repo_review"
)

// ScanResult is what one scan_repo pass learned about one repository.
type ScanResult struct {
	// Candidates are the MRs that need an AI review now, with the §6.5 backoff
	// already applied.
	Candidates []ReviewRequest
	// StalePublish are reviews stuck in status='reviewed' past the threshold:
	// their publication never finished, so it is enqueued again. The expensive
	// part is not repeated (§6.3).
	StalePublish []uuid.UUID
	// Snapshots is the merge requests this pass actually inspected — the ones
	// that passed the cheap filter and were worth a detail call (§9.1). It is NOT
	// every open MR: `ai_reviewer_merge_requests_scanned` counts those, and the
	// digest builds its own snapshots, because the two runs see different
	// moments and a scan's copy would be stale by the time a digest fired.
	Snapshots []domain.MergeRequestSnapshot
	// Failed marks a repository that could not be fully inspected: some MRs are
	// missing from Snapshots. It is a partial result, not an error — the digest
	// reports "⚠️ Partial data" rather than pretending it saw everything.
	Failed bool
}

// backoffLadder is the §6.5 wait after N consecutive failed reviews of one head
// SHA: index 1 after one failure, 2 after two, 3 after three. Four failures and
// the MR is not enqueued at all until a new head SHA resets the count, because
// a deterministically-broken MR would otherwise cost 288 full LLM runs a day.
//
// The thresholds are fixed on purpose and are not configurable.
var backoffLadder = [...]time.Duration{0, 15 * time.Minute, time.Hour, 6 * time.Hour}

// Bounds on the §6.3 publication sweep.
const (
	// maxPublishSweeps is how many times one review may be re-enqueued for
	// publication before it is retired as unpublishable. Each sweep hands the
	// review a fresh publish_review job worth ten attempts, and the sweep runs no
	// more often than the scan interval, so 20 is on the order of two hours of
	// retrying — long enough to ride out a GitLab outage, short enough that a
	// deleted merge request stops costing publish slots the same afternoon.
	maxPublishSweeps = 20
	// maxStalePublishPerPass keeps one repository's backlog from filling the
	// publish queue in a single pass. The remainder is picked up next scan.
	maxStalePublishPerPass = 100
)

// ScanRepository inspects one repository: it lists the open MRs, classifies
// them, and decides which need an AI review.
//
// Errors and partial results are deliberately different things. Failing to list
// the repository at all is an error — the job should retry, and a repository
// silently reporting "no open MRs" would be worse than a retry. Failing to
// inspect individual MRs sets Failed and returns everything else, because one
// unreadable MR must not cost a team its whole digest.
//
// It emits ai_reviewer_scans_total / _duration_seconds because a repository is
// the unit that actually does the scanning work: the `scan` job above it only
// reads config and fans out. The four per-team gauges are NOT set here — they
// describe a whole team, and setting them per repository would leave every team
// reporting only its last repository's counts. BuildDigest, which sees the
// whole team in one pass, owns them.
func (s *Service) ScanRepository(ctx context.Context, team domain.Team, repository string) (*ScanResult, error) {
	start := s.now()

	proj, err := s.gl.GetProject(ctx, projectKey(0, repository))
	if err != nil {
		metrics.ObserveScan(metrics.ResultError, s.now().Sub(start))
		return nil, fmt.Errorf("get project %s: %w", repository, err)
	}
	open, err := s.gl.ListOpenMRs(ctx, projectKey(proj.ID, proj.PathWithNamespace))
	if err != nil {
		metrics.ObserveScan(metrics.ResultError, s.now().Sub(start))
		return nil, fmt.Errorf("list open merge requests of %s: %w", proj.PathWithNamespace, err)
	}

	// The cheap filter runs on the LIST payload alone (§9.1: the detail endpoint
	// is for "кандидат прошёл дешёвый отсев"). state, draft and the head SHA are
	// all it needs and all three are in the list response, so a repository whose
	// merge requests have not moved costs exactly one GitLab request per pass
	// instead of one plus four per open MR — at 40 repositories × 20 MRs every
	// five minutes, the difference is ~920k requests a day against the per-user
	// rate limit named in §20.6.
	var interesting []gitlab.MergeRequest
	for _, mr := range open {
		ok, err := s.mayNeedReview(ctx, team, proj, mr)
		if err != nil {
			res := &ScanResult{Failed: true}
			s.log.Warnw("review candidacy could not be decided",
				"project", proj.PathWithNamespace, "iid", mr.IID, "err", err)
			return s.finishScan(ctx, team, proj, res, len(open), start)
		}
		if ok {
			interesting = append(interesting, mr)
		}
	}

	loader := s.newSnapshotLoader(depthReview)
	// One GraphQL call per repository per run buys exact reviewer review states
	// for every MR at once; without it every reviewer degrades to the REST
	// heuristic, which is correct but coarser.
	iids := make([]int64, 0, len(interesting))
	for _, mr := range interesting {
		iids = append(iids, mr.IID)
	}
	loader.primeReviewStates(ctx, proj.PathWithNamespace, iids)

	res := &ScanResult{}
	for _, mr := range interesting {
		loaded, err := loader.load(ctx, team.Name, proj, mr.IID)
		if err != nil {
			// Partial data: report it and keep going. The alternative — failing
			// the repository — would hide every other MR in it.
			res.Failed = true
			s.log.Warnw("merge request could not be inspected",
				"project", proj.PathWithNamespace, "iid", mr.IID, "err", err)
			continue
		}
		res.Snapshots = append(res.Snapshots, loaded.Snapshot)

		// The decision is retaken on the detail payload: diff_refs.head_sha is
		// the SHA everything content-addressed keys off, and it lags mr.sha for
		// a moment right after a push. The cheap filter is deliberately
		// conservative in that window — it lets the MR through and this call
		// says "up to date" — so the narrowing can never lose a review.
		candidate, err := s.reviewCandidate(ctx, team, proj, loaded.Snapshot)
		if err != nil {
			res.Failed = true
			s.log.Warnw("review candidacy could not be decided",
				"project", proj.PathWithNamespace, "iid", mr.IID, "err", err)
			continue
		}
		if candidate != nil {
			res.Candidates = append(res.Candidates, *candidate)
		}
	}

	return s.finishScan(ctx, team, proj, res, len(open), start)
}

// mayNeedReview is the cheap filter: everything domain.NeedsAIReview can decide
// from a list entry, plus the one database lookup that answers "already
// reviewed".
//
// It is allowed to say yes to an MR the full snapshot then rejects, and never
// the other way round. The list's `sha` is the source-branch head; diff_refs's
// head_sha — which is what a review is recorded under — can lag it by seconds
// after a push. In that window the list SHA has no review row, so this returns
// true and the detail call settles it. The reverse cannot happen: a matching
// list SHA implies diff_refs agreed with it when the review was recorded.
func (s *Service) mayNeedReview(ctx context.Context, team domain.Team, proj *gitlab.Project, mr gitlab.MergeRequest) (bool, error) {
	if mr.SHA == "" {
		// No SHA in the list payload is not the same as no SHA at all: the detail
		// endpoint may still carry diff_refs. Let the full snapshot decide, and
		// let it own the skip metric.
		return true, nil
	}
	reviewed, err := s.alreadyReviewed(ctx, proj.ID, mr.IID, mr.SHA)
	if err != nil {
		return false, err
	}
	snap := domain.MergeRequestSnapshot{
		MR:      domain.MergeRequest{IID: mr.IID, State: mr.State, Draft: mr.IsDraft()},
		HeadSHA: mr.SHA,
	}
	ok, reason := domain.NeedsAIReview(snap, team.AIReview, reviewed)
	if !ok {
		metrics.ReviewSkipped(team.Name, reason.String())
	}
	return ok, nil
}

// finishScan appends the §6.3 publication sweep and emits the pass's metrics.
// It is the single exit of ScanRepository so no return path can skip either.
func (s *Service) finishScan(ctx context.Context, team domain.Team, proj *gitlab.Project, res *ScanResult, open int, start time.Time) (*ScanResult, error) {
	s.sweepStalePublications(ctx, proj, res)

	// Every open merge request was classified, even the ones no detail call was
	// spent on — the gauge means "seen", not "fetched".
	metrics.MergeRequestsScanned(team.Name, open)
	result := metrics.ResultOK
	if res.Failed {
		result = metrics.ResultPartial
	}
	metrics.ObserveScan(result, s.now().Sub(start))

	s.log.Infow("repository scanned",
		"team", team.Name, "project", proj.PathWithNamespace,
		"open", open, "inspected", len(res.Snapshots),
		"candidates", len(res.Candidates), "stale_publish", len(res.StalePublish),
		"partial", res.Failed)
	return res, nil
}

// sweepStalePublications is the second line of defence for publications that
// never finished: attempts exhausted, the job cancelled by hand, the database
// restored from a backup. Only delivery is redone; the LLM run is not.
//
// The claim is bounded and the exhausted rows are retired, because the sweep
// used to have no ceiling of any kind: a review that can never be published (the
// MR was deleted, the token lost its scope) was re-enqueued every scan_interval
// forever through a one- or two-worker queue, starving the publications that
// could still land.
func (s *Service) sweepStalePublications(ctx context.Context, proj *gitlab.Project, res *ScanResult) {
	olderThan := s.now().Add(-s.cfg.StalePublishAfter)

	stale, err := s.st.Review().ClaimStalePublications(ctx, repo_review.ClaimStalePublicationsParams{
		ProjectID:   proj.ID,
		OlderThan:   olderThan,
		MaxAttempts: maxPublishSweeps,
		MaxRows:     maxStalePublishPerPass,
	})
	if err != nil {
		res.Failed = true
		s.log.Warnw("stale published reviews could not be claimed", "project", proj.PathWithNamespace, "err", err)
		return
	}
	res.StalePublish = append(res.StalePublish, stale...)

	// Retiring runs after the claim so a review always gets its full budget of
	// attempts before it is written off.
	abandoned, err := s.st.Review().AbandonExhaustedPublications(ctx, repo_review.AbandonExhaustedPublicationsParams{
		ErrorText:   fmt.Sprintf("publication was retried %d times and never completed", maxPublishSweeps),
		ProjectID:   proj.ID,
		OlderThan:   olderThan,
		MaxAttempts: maxPublishSweeps,
	})
	if err != nil {
		res.Failed = true
		s.log.Warnw("exhausted publications could not be retired", "project", proj.PathWithNamespace, "err", err)
		return
	}
	if len(abandoned) > 0 {
		// Loud, and it names the rows: giving up on publishing a review the team
		// paid for is a fact an operator has to be able to find.
		s.log.Errorw("giving up on publishing reviews whose publication never completed",
			"project", proj.PathWithNamespace, "reviews", abandoned, "attempts", maxPublishSweeps)
	}
}

// reviewCandidate decides whether one MR should be handed to the review queue
// now. It returns nil when the MR does not need a review, or when the backoff
// ladder says not yet.
func (s *Service) reviewCandidate(ctx context.Context, team domain.Team, proj *gitlab.Project, snap domain.MergeRequestSnapshot) (*ReviewRequest, error) {
	reviewed, err := s.alreadyReviewed(ctx, proj.ID, snap.MR.IID, snap.HeadSHA)
	if err != nil {
		return nil, err
	}
	if ok, reason := domain.NeedsAIReview(snap, team.AIReview, reviewed); !ok {
		metrics.ReviewSkipped(team.Name, reason.String())
		return nil, nil
	}

	stats, err := s.st.Review().FailureStats(ctx, repo_review.FailureStatsParams{
		ProjectID: proj.ID, MrIid: snap.MR.IID, HeadSha: snap.HeadSHA,
	})
	if err != nil {
		return nil, fmt.Errorf("failure stats for %d!%d: %w", proj.ID, snap.MR.IID, err)
	}
	due, wait := reviewDue(stats, s.now())
	if !due {
		// A poisoned MR stops costing money but must not become invisible: the
		// counter ai_reviews_failed_total keeps its history, and this line names
		// the MR *and why it is stuck*. Without last_error the operator can see
		// that the ladder gave up but not what to fix. The message goes through
		// the redacting core like every other logged string.
		s.log.Warnw("review held back by the failure backoff",
			"project", proj.PathWithNamespace, "iid", snap.MR.IID, "head_sha", snap.HeadSHA,
			"failures", stats.Failures, "last_failure_at", stats.LastFailureAt,
			"last_error", stats.LastError, "retry_in", wait)
		return nil, nil
	}

	return &ReviewRequest{
		Team:        team.Name,
		ProjectPath: proj.PathWithNamespace,
		ProjectID:   proj.ID,
		MRIID:       snap.MR.IID,
		HeadSHA:     snap.HeadSHA,
	}, nil
}

// reviewDue applies the §6.5 ladder. wait is how long is still left when the
// answer is "not yet"; it is zero for the permanent stop, which only a new head
// SHA can clear — and a new head SHA is a different row set, so the counter
// resets by itself.
func reviewDue(stats repo_review.FailureStats, now time.Time) (due bool, wait time.Duration) {
	n := stats.Failures
	if n <= 0 {
		return true, 0
	}
	if int(n) >= len(backoffLadder) {
		return false, 0
	}
	elapsed := now.Sub(stats.LastFailureAt)
	if need := backoffLadder[n]; elapsed < need {
		return false, need - elapsed
	}
	return true, 0
}
