package service

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/sxwebdev/ai-reviewer/internal/domain"
	"github.com/sxwebdev/ai-reviewer/internal/gitlab"
)

// loadedMR is one fully-inspected merge request: the domain snapshot the
// classifiers work on, plus the raw GitLab discussions.
//
// The raw discussions are kept because domain.Discussion deliberately drops
// note positions (the classifiers do not need them) while two consumers do:
// the prompt context renders "file:line" for inline notes, and the marker
// scanner reads fingerprints out of note bodies. Re-fetching them would break
// the one-load-per-MR budget of §9.4.
type loadedMR struct {
	Snapshot    domain.MergeRequestSnapshot
	Discussions []gitlab.Discussion
	// Refs are the MR's diff_refs, the three SHAs an inline position is
	// anchored to. Only the review path needs them, and only the detail
	// endpoint returns them.
	Refs gitlab.DiffRefs
}

// snapshotDepth selects how much of §9.1's per-MR request budget a run pays.
//
// The table there puts /versions and /approvals under "digest", and the review
// path genuinely does not read what they produce: NeedsAIReview looks at state,
// draft and the head SHA, and the prompt uses the title, description, branches
// and pipeline status. Loading them anyway doubles the per-MR cost of a scan for
// two fields nobody reads.
type snapshotDepth int

const (
	// depthReview loads the MR detail and its discussions. LastPushAt stays zero
	// and ApprovedBy stays empty with ApprovalsKnown false, which every consumer
	// reads as "unknown".
	depthReview snapshotDepth = iota
	// depthDigest additionally loads /versions (the "waiting 18h" clock and the
	// REQUESTED_CHANGES freshness rule) and /approvals, which is the sole input to
	// both Linear completion rules and also sharpens the REST reviewer fallback.
	depthDigest
)

// snapshotLoader loads merge-request snapshots at most once per (project, iid)
// for the lifetime of one run (plan §9.4). It is created per entry point, not
// per Service, because a run is exactly the scope over which "the same MR" may
// be assumed unchanged — and because the depth is a property of the entry point,
// so one cache never mixes a light snapshot with a full one.
//
// It is not safe for concurrent use; every caller drives it from one goroutine.
type snapshotLoader struct {
	svc   *Service
	depth snapshotDepth

	cache map[string]*loadedMR
	// states holds GraphQL reviewer review states per project full path. It is
	// primed once per project per run; a project absent from the map means the
	// query was never made or degraded, which leaves every reviewer in
	// ReviewStateUnknown and routes them through the REST heuristic.
	states map[string]map[int64][]gitlab.ReviewerState
	// gqlWarned keeps the GraphQL degradation warning to one line per run
	// rather than one per merge request.
	gqlWarned bool
	// approvalsWarned does the same for the approvals endpoint, which some
	// instances refuse.
	approvalsWarned bool
}

func (s *Service) newSnapshotLoader(depth snapshotDepth) *snapshotLoader {
	return &snapshotLoader{
		svc:    s,
		depth:  depth,
		cache:  map[string]*loadedMR{},
		states: map[string]map[int64][]gitlab.ReviewerState{},
	}
}

func snapshotKey(projectID, iid int64) string {
	return strconv.FormatInt(projectID, 10) + ":" + strconv.FormatInt(iid, 10)
}

// primeReviewStates fetches reviewer review states for a whole project in one
// GraphQL call (§9.2). Failure is never fatal: every error from the GraphQL
// client wraps gitlab.ErrGraphQLUnsupported, so one errors.Is decides whether
// to fall back, and the warning is emitted once per run.
func (l *snapshotLoader) primeReviewStates(ctx context.Context, fullPath string, iids []int64) {
	if l.svc.gql == nil || fullPath == "" || len(iids) == 0 {
		return
	}
	if _, done := l.states[fullPath]; done {
		return
	}
	states, err := l.svc.gql.ReviewStates(ctx, fullPath, iids)
	if err != nil {
		if !l.gqlWarned {
			l.gqlWarned = true
			level := l.svc.log.Warnw
			if !errors.Is(err, gitlab.ErrGraphQLUnsupported) {
				level = l.svc.log.Errorw
			}
			level("graphql review states unavailable; falling back to the REST heuristic",
				"project", fullPath, "err", err)
		}
		// Mark the project as attempted so a second repository pass in the same
		// run does not pay for the same failure again.
		l.states[fullPath] = nil
		return
	}
	l.states[fullPath] = states
}

// load inspects one merge request and returns its snapshot, serving a cached
// copy on a second call for the same MR within the run.
//
// Detail, discussions and — at depthDigest — diff versions are fatal on error:
// without them the classifiers would silently answer "nothing to do" for an MR
// that does need attention. Approvals and the head-pipeline fallback degrade
// instead — both only ever remove information, and both are endpoints an
// instance may refuse.
func (l *snapshotLoader) load(ctx context.Context, team string, proj *gitlab.Project, iid int64) (*loadedMR, error) {
	key := snapshotKey(proj.ID, iid)
	if got, ok := l.cache[key]; ok {
		return got, nil
	}

	pk := projectKey(proj.ID, proj.PathWithNamespace)

	var (
		detail      *gitlab.MergeRequest
		discussions []gitlab.Discussion
		versions    []gitlab.MergeRequestVersion
		approvals   *gitlab.Approvals
		approvalErr error
	)
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() (err error) {
		detail, err = l.svc.gl.GetMR(gctx, pk, iid)
		return err
	})
	g.Go(func() (err error) {
		discussions, err = l.svc.gl.ListMRDiscussions(gctx, pk, iid)
		return err
	})
	if l.depth == depthDigest {
		g.Go(func() (err error) {
			versions, err = l.svc.gl.ListMRVersions(gctx, pk, iid)
			return err
		})
		g.Go(func() error {
			// Errors are captured, not returned: losing approvals must not cost the
			// whole snapshot, and the digest is still worth building without them.
			//
			// What it may *not* do is look like "nobody approved". The comment here
			// used to say approvals "only sharpen the REST fallback", which was true
			// until the Linear rules made ApprovedBy the sole input to "is this
			// review finished" — after which a 403 from an under-privileged service
			// account disabled the completion gate on every merge request, silently
			// and for as long as the permission was missing. ApprovalsKnown is how
			// the failure stays visible to the classifiers; see
			// domain.MergeRequestSnapshot.ApprovalsKnown.
			approvals, approvalErr = l.svc.gl.GetMRApprovals(gctx, pk, iid)
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	if approvalErr != nil {
		if !l.approvalsWarned {
			l.approvalsWarned = true
			// Once per run, since where the endpoint is refused this fires for every
			// merge request.
			// Both consequences are named — the first alone reads like cosmetic noise —
			// and the Linear half is worded as a conditional, because a deployment
			// without linear_team_ids cannot reach it at all.
			l.svc.log.Warnw("merge request approvals unavailable; reviewers keep being nudged after approving, and where Linear is configured no author is asked to advance their card",
				"project", proj.PathWithNamespace, "err", approvalErr)
		}
	}
	if approvals == nil {
		approvals = &gitlab.Approvals{}
	}

	snap := domain.MergeRequestSnapshot{
		Team:         team,
		Project:      mapProject(proj),
		MR:           mapMergeRequest(detail),
		HeadSHA:      HeadSHA(detail),
		Discussions:  mapDiscussions(discussions),
		Mergeability: mapMergeability(detail),
		Pipeline:     mapPipeline(detail.HeadPipeline),
		LastPushAt:   lastPushAt(versions),
	}
	// depthReview never asks for approvals, so "unknown" is also its honest
	// answer — the field is not a claim that the request failed, it is a claim
	// that ApprovedBy may be trusted as complete.
	snap.ApprovalsKnown = l.depth == depthDigest && approvalErr == nil
	for _, u := range approvals.Approvers() {
		snap.ApprovedBy = append(snap.ApprovedBy, mapUser(u))
	}
	snap.Reviewers = l.buildReviewers(proj.PathWithNamespace, detail, snap)

	// head_pipeline is exposed "only if the current user can view pipelines for
	// this project", so its absence is a permissions signal rather than "no
	// pipeline". Fall back to the pipeline list for exactly those MRs, exactly
	// once — the snapshot cache is what bounds it to once per MR per run.
	if !snap.Pipeline.Known {
		snap.Pipeline = l.pipelineFallback(ctx, pk, iid, snap.HeadSHA)
	}

	out := &loadedMR{Snapshot: snap, Discussions: discussions, Refs: detail.DiffRefs}
	l.cache[key] = out
	return out, nil
}

// pipelineFallback fetches the MR's pipeline list and picks the one belonging
// to the head SHA. When none matches, the newest is returned anyway: the domain
// classifier compares SHAs itself and reports a stale pipeline as unknown, so
// handing it the real newest pipeline is both honest and self-correcting.
func (l *snapshotLoader) pipelineFallback(ctx context.Context, pk string, iid int64, head string) domain.Pipeline {
	pipelines, err := l.svc.gl.ListMRPipelines(ctx, pk, iid)
	if err != nil {
		l.svc.log.Debugw("merge request pipelines unavailable", "iid", iid, "err", err)
		return domain.Pipeline{}
	}
	if len(pipelines) == 0 {
		return domain.Pipeline{}
	}
	chosen := &pipelines[0]
	for i := range pipelines {
		if pipelines[i].SHA != "" && pipelines[i].SHA == head {
			chosen = &pipelines[i]
			break
		}
	}
	return mapPipeline(chosen)
}

// buildReviewers pairs each assigned reviewer with their GraphQL review state
// and the time they last engaged with the MR.
func (l *snapshotLoader) buildReviewers(fullPath string, detail *gitlab.MergeRequest, snap domain.MergeRequestSnapshot) []domain.Reviewer {
	if len(detail.Reviewers) == 0 {
		return nil
	}
	byUsername := map[string]gitlab.ReviewerState{}
	for _, rs := range l.states[fullPath][detail.IID] {
		byUsername[strings.ToLower(strings.TrimSpace(rs.Username))] = rs
	}

	out := make([]domain.Reviewer, 0, len(detail.Reviewers))
	for _, u := range detail.Reviewers {
		r := domain.Reviewer{User: mapUser(u)}
		// ParseReviewState rather than a cast: an enum value this build does
		// not know must degrade into the REST fallback, never read as
		// "already reviewed".
		if rs, ok := byUsername[strings.ToLower(strings.TrimSpace(u.Username))]; ok {
			r.State = domain.ParseReviewState(string(rs.State))
		}
		// snap.LastPushAt is already filled by the caller; it bounds which system
		// notes may date a verdict — see lastActivityAt.
		r.LastActivityAt = lastActivityAt(snap.Discussions, r.User, snap.LastPushAt)
		out = append(out, r)
	}
	return out
}

// lastActivityAt is when a user last engaged with the MR: their most recent
// ordinary note, or — only when they wrote none at all — their most recent
// system note that predates lastPushAt.
//
// The system-note fallback exists because a reviewer can deliver a verdict
// without typing anything. GitLab records a click on "Request changes" as a
// *system* note ("requested changes"), exactly as it records approvals and
// un-approvals, and mergeRequestInteraction.reviewState carries no timestamp of
// its own. Counting ordinary notes only left that reviewer at the zero time
// forever, which NeedsHumanReview reads as "the verdict cannot be dated" and
// hands to the author — permanently. The author's push could never give the MR
// back, so the reviewer was never nudged again and the author read "changes
// requested by X" for the rest of the MR's life.
//
// The lastPushAt clamp is the whole safety of that fallback, and it is not an
// optimisation: **a system note may only ever establish that a verdict predates
// the last push, never that the reviewer acted after it.** GitLab credits a user
// with every event it records for them — "added ~label", "assigned to @x",
// "requested review from @y", "added 3 commits", "marked this merge request as
// ready", "mentioned in merge request !999" — so an unclamped fallback let one
// unrelated click after the author's push read as a re-review. That parks the MR
// in the *author's* list as "changes requested by R" and leaves it there until
// the next push, which the author has no reason to make, having already pushed
// the fix: the re-look is lost for the life of the MR, which is strictly worse
// than the zero-time bug this fallback fixes. Clamped, the states come out right
// without any assumption about note bodies:
//
//   - verdict newer than the last push, nothing since → unusable → zero → the
//     author keeps the MR (the documented undatable residual);
//   - author pushes after the verdict → the note now predates the push, is used,
//     and the push is later still → the reviewer owes the next look;
//   - a stray event after that push → newer than the push → ignored → the
//     reviewer still owes it.
//
// Matching the body prefix instead ("requested changes" / "approved" /
// "unapproved") would be semantically sharper, and is rejected twice over: this
// repository has never verified GitLab's wording against a live instance, and a
// *second* uncommented verdict would still date itself after the push and evict
// the reviewer. The positional rule needs neither the vocabulary nor the luck.
//
// Ordinary notes are not clamped and still win outright, even when a system note
// is newer: a comment is a human act, and it is the evidence the rule was written
// for. A lastPushAt of zero (no diff versions were read) discards every system
// note, which is observationally identical to the old behaviour — NeedsHumanReview
// cannot conclude anything from an unknown push time either.
func lastActivityAt(discussions []domain.Discussion, u domain.User, lastPushAt time.Time) time.Time {
	var ordinary, system time.Time
	for _, d := range discussions {
		for _, n := range d.Notes {
			if !sameUser(n.Author, u) {
				continue
			}
			if n.System {
				// Strictly after the push: a note at the push instant is kept, the
				// same tie-break the push-aware classifier uses.
				if n.CreatedAt.After(lastPushAt) {
					continue
				}
				if n.CreatedAt.After(system) {
					system = n.CreatedAt
				}
				continue
			}
			if n.CreatedAt.After(ordinary) {
				ordinary = n.CreatedAt
			}
		}
	}
	if ordinary.IsZero() {
		return system
	}
	return ordinary
}

// sameUser matches two GitLab accounts: by id when both carry one, otherwise
// case-insensitively by username. It mirrors the rule internal/domain uses for
// the same comparison.
func sameUser(a, b domain.User) bool {
	if a.ID != 0 && b.ID != 0 {
		return a.ID == b.ID
	}
	return a.Username != "" && strings.EqualFold(a.Username, b.Username)
}

// HeadSHA is the one SHA everything content-addressed in a review keys off:
// review dedupe, job uniqueness, worktree checkout and pipeline freshness.
// diff_refs wins over the MR's sha because positions are anchored to diff_refs;
// the two can disagree for a moment right after a push.
//
// It is exported because the queue's uniqueness key is (project_id, mr_iid,
// head_sha), so every producer of a review job must derive the SHA the same way.
// A CLI `review <ref>` that read mr.SHA while the scanner read diff_refs.head_sha
// would insert a second job for work already queued — inside exactly the window
// this function documents.
func HeadSHA(mr *gitlab.MergeRequest) string {
	if mr.DiffRefs.HeadSHA != "" {
		return mr.DiffRefs.HeadSHA
	}
	return mr.SHA
}

func mapProject(p *gitlab.Project) domain.Project {
	return domain.Project{
		ID:            p.ID,
		FullPath:      p.PathWithNamespace,
		DefaultBranch: p.DefaultBranch,
		WebURL:        p.WebURL,
	}
}

func mapMergeRequest(mr *gitlab.MergeRequest) domain.MergeRequest {
	return domain.MergeRequest{
		ID:           mr.ID,
		IID:          mr.IID,
		ProjectID:    mr.ProjectID,
		Title:        mr.Title,
		Description:  mr.Description,
		State:        mr.State,
		Draft:        mr.IsDraft(),
		WebURL:       mr.WebURL,
		Author:       mapUser(mr.Author),
		SourceBranch: mr.SourceBranch,
		TargetBranch: mr.TargetBranch,
		CreatedAt:    parseTime(mr.CreatedAt),
		UpdatedAt:    parseTime(mr.UpdatedAt),
	}
}

func mapUser(u gitlab.User) domain.User {
	return domain.User{ID: u.ID, Username: u.Username, Name: u.Name, Email: u.Email}
}

func mapDiscussions(in []gitlab.Discussion) []domain.Discussion {
	if len(in) == 0 {
		return nil
	}
	out := make([]domain.Discussion, 0, len(in))
	for _, d := range in {
		md := domain.Discussion{ID: d.ID, IndividualNote: d.IndividualNote}
		for _, n := range d.Notes {
			md.Notes = append(md.Notes, domain.Note{
				ID:         n.ID,
				Body:       n.Body,
				Author:     mapUser(n.Author),
				System:     n.System,
				Resolvable: n.Resolvable,
				Resolved:   n.Resolved,
				CreatedAt:  parseTime(n.CreatedAt),
			})
		}
		out = append(out, md)
	}
	return out
}

// legacyRecheck is the one legacy merge_status value that means "mergeability
// is being recomputed" without having a counterpart in detailed_merge_status.
// The other two ("unchecked", "checking") are spelled the same in both fields,
// so the domain classifier already treats them as pending.
const legacyRecheck = "cannot_be_merged_recheck"

// mapMergeability sets Known only when GitLab actually reported mergeability.
// "false" and "not reported" must not look alike: telling an author about a
// conflict we are not sure about sends them chasing a problem that is not there.
func mapMergeability(mr *gitlab.MergeRequest) domain.Mergeability {
	detailed := strings.ToLower(strings.TrimSpace(mr.DetailedMergeStatus))
	legacy := strings.ToLower(strings.TrimSpace(mr.MergeStatus))

	status := detailed
	if status == "" {
		// Older GitLab has no detailed_merge_status. Its legacy vocabulary
		// overlaps enough for the domain classifier to read directly.
		status = legacy
	}
	if status == "" || status == legacyRecheck {
		return domain.Mergeability{}
	}
	return domain.Mergeability{
		HasConflicts:   mr.HasConflicts,
		DetailedStatus: status,
		Known:          true,
	}
}

// mapPipeline reports Known only for a pipeline GitLab actually returned. A nil
// head_pipeline means "cannot see pipelines here", never "no pipeline".
func mapPipeline(p *gitlab.Pipeline) domain.Pipeline {
	if p == nil {
		return domain.Pipeline{}
	}
	return domain.Pipeline{
		ID:     p.ID,
		SHA:    p.SHA,
		Status: p.Status,
		WebURL: p.WebURL,
		Known:  true,
	}
}

// lastPushAt is the created_at of the newest diff version — the MR's last push
// time, and the clock the digest's "waiting 18h" and the REQUESTED_CHANGES
// freshness rule are measured against (§20.3). The list is scanned rather than
// indexed at [0] so the answer does not depend on GitLab's ordering.
func lastPushAt(versions []gitlab.MergeRequestVersion) time.Time {
	var latest time.Time
	for _, v := range versions {
		if t := parseTime(v.CreatedAt); t.After(latest) {
			latest = t
		}
	}
	return latest
}

// parseTime converts a GitLab timestamp. GitLab emits RFC3339 with fractional
// seconds; an unparseable or absent value yields the zero time, which every
// consumer treats as "unknown" rather than "the epoch".
func parseTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000-0700"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
