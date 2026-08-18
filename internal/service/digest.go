package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sxwebdev/ai-reviewer/internal/dbtypes"
	"github.com/sxwebdev/ai-reviewer/internal/domain"
	"github.com/sxwebdev/ai-reviewer/internal/linear"
	"github.com/sxwebdev/ai-reviewer/internal/match"
	"github.com/sxwebdev/ai-reviewer/internal/metrics"
	"github.com/sxwebdev/ai-reviewer/internal/models"
	"github.com/sxwebdev/ai-reviewer/internal/security"
	"github.com/sxwebdev/ai-reviewer/internal/slack"
	"github.com/sxwebdev/ai-reviewer/internal/store"
	"github.com/sxwebdev/ai-reviewer/internal/store/repos/repo_digestmessage"
	"github.com/sxwebdev/ai-reviewer/internal/store/repos/repo_digestrun"
)

// maxPreviewBytes bounds a dry-run payload preview in the log.
const maxPreviewBytes = 16 << 10

// DigestOutcome reports what one BuildDigest call produced.
type DigestOutcome struct {
	RunID uuid.UUID
	// Messages are the persisted parts that still need delivering, in order —
	// one slack_send job each. It is empty for a dry run: the rows exist and
	// can be inspected, but no delivery job may be created for them (§13.5).
	Messages         []uuid.UUID
	Status           string // built | dry_run | partial | failed
	MRCount          int
	LinearIssueCount int
}

// BuildDigest assembles one team's digest and persists it. It never calls
// chat.postMessage: building a digest costs dozens of GitLab requests and a
// Slack directory load, while delivering one part costs one POST that may need
// retrying — so the two are separate jobs with separate retry economics (§6.3).
//
// Re-running the same (team, run_date, slot, attempt) is idempotent: the
// existing run is returned untouched. A previous attempt that failed before it
// could build anything is reused rather than duplicated, because the unique
// index on those four columns is what stops two replicas doubling a slot.
func (s *Service) BuildDigest(ctx context.Context, team domain.Team, slot string, runDate time.Time, attempt int) (*DigestOutcome, error) {
	day := truncateDay(runDate)

	existing, err := s.digestRunForSlot(ctx, team.Name, slot, day, attempt)
	if err != nil {
		return nil, err
	}
	var reuseID uuid.UUID
	if existing != nil {
		if existing.Status != DigestFailed {
			// Already built for this slot and attempt — hand back what is there
			// rather than assembling a second copy.
			return s.outcomeForRun(ctx, existing)
		}
		// A failed attempt left a row but no messages; the unique index means
		// we must reuse it rather than insert a twin.
		reuseID = existing.ID
	}

	snapshots, failedRepos := s.gatherTeam(ctx, team)
	linearState, linearErr := s.gatherLinear(ctx, team, snapshots)

	gitLabUnavailable := failedRepos > 0 && len(snapshots) == 0 && len(team.Repositories) > 0
	linearConfigured := len(team.LinearTeamIDs) > 0
	// Two different questions. linearDegraded asks whether anything failed, and
	// drives the partial status and the warning; linearState.enabled asks whether
	// the board total actually arrived, and is the only one allowed to decide
	// that Linear contributed nothing at all.
	linearDegraded := linearConfigured && linearErr != nil

	// Every metric this build owns is published here, before the early return
	// below: a run that learned nothing is still a run, and a source failure that
	// aborts the digest is the one an operator most wants counted. This is also
	// the only place that sees a whole team at once, so it is the only place that
	// can set the per-team gauges honestly — overwritten every pass, including
	// when every count is zero, because a team that drops to zero must report
	// zero rather than keep its last non-zero value forever.
	metrics.SetTeamState(team.Name, teamState(snapshots, linearState))
	if linearState.enabled {
		metrics.SetLinearIssuesInReview(team.Name, linearState.inReviewCount)
	} else {
		// Unknown, so the series goes absent rather than stale or falsely zero —
		// see metrics.ClearLinearIssuesInReview. Covers both the outage and the
		// team that no longer declares linear_team_ids.
		metrics.ClearLinearIssuesInReview(team.Name)
	}
	if failedRepos > 0 {
		metrics.DigestSourceError(team.Name, metrics.SourceGitLab)
	}
	if linearDegraded {
		metrics.DigestSourceError(team.Name, metrics.SourceLinear)
	}

	if gitLabUnavailable && (!linearConfigured || !linearState.enabled) {
		// Nothing was learned at all, so there is no digest to build — as
		// opposed to a partial one, which is a normal result.
		err := fmt.Errorf("digest for team %q: all configured sources failed (GitLab repositories: %d, Linear configured: %t)", team.Name, failedRepos, linearConfigured)
		s.recordDigestFailure(ctx, team, slot, day, attempt, reuseID, err)
		metrics.DigestRun(team.Name, metrics.ResultError)
		return nil, err
	}

	data, mrCount := s.digestData(ctx, team, snapshots, failedRepos, linearState)
	if linearDegraded {
		// The two warnings are not interchangeable. "could not be inspected"
		// explains an absent count; with the count present it would read as a
		// contradiction of the line right above it, and hide which half is stale.
		warning := "Partial data: Linear could not be inspected."
		if linearState.enabled {
			warning = "Partial data: Linear issue links could not be resolved; the In Review count is current."
		}
		data.Warnings = append(data.Warnings, warning)
		s.log.Warnw("Linear could not be fully inspected for the digest",
			"team", team.Name, "count_known", linearState.enabled, "err", linearErr)
	}
	messages := slack.BuildDigest(data)

	failedSources := failedRepos
	if linearDegraded {
		failedSources++
	}
	status, messageStatus := digestStatuses(s.cfg.SlackSendEnabled, failedSources)
	if !s.cfg.SlackSendEnabled {
		s.logDigestPreview(team, messages)
	}

	runID, ids, err := s.persistDigest(ctx, persistDigestInput{
		reuseID:          reuseID,
		team:             team,
		slot:             slot,
		runDate:          day,
		attempt:          attempt,
		status:           status,
		messageStatus:    messageStatus,
		mrCount:          mrCount,
		linearIssueCount: linearState.inReviewCount,
		messages:         messages,
	})
	if err != nil {
		metrics.DigestRun(team.Name, metrics.ResultError)
		return nil, err
	}

	result := metrics.ResultOK
	if status == DigestPartial {
		result = metrics.ResultPartial
	}
	metrics.DigestRun(team.Name, result)
	s.log.Infow("digest built",
		"team", team.Name, "slot", slot, "run_date", day.Format(time.DateOnly), "attempt", attempt,
		"status", status, "parts", len(messages), "mrs", mrCount,
		"linear_issues", linearState.inReviewCount, "failed_repos", failedRepos, "linear_degraded", linearDegraded)

	out := &DigestOutcome{RunID: runID, Status: status, MRCount: mrCount, LinearIssueCount: linearState.inReviewCount}
	if s.cfg.SlackSendEnabled {
		out.Messages = ids
	}
	return out, nil
}

// digestStatuses maps the run's circumstances onto the two status columns.
// A dry run outranks a partial one: "nothing was sent" is the fact an operator
// must see first, and the partial-data warning is still rendered in the message
// itself.
func digestStatuses(sendEnabled bool, failedRepos int) (run, message string) {
	switch {
	case !sendEnabled:
		return DigestDryRun, MessageDryRun
	case failedRepos > 0:
		return DigestPartial, MessagePending
	default:
		return DigestBuilt, MessagePending
	}
}

// digestRunForSlot returns the run already recorded for exactly this slot and
// attempt, or nil.
//
// It addresses the attempt explicitly rather than asking for the slot's latest
// one: a `digest --force` earlier in the day owns attempt 1, and answering "yes,
// the 09:00 slot already ran" would make the scheduled attempt 0 collide with
// digest_runs_slot_uniq instead of being recognised as a run that has not
// happened yet.
func (s *Service) digestRunForSlot(ctx context.Context, team, slot string, day time.Time, attempt int) (*models.DigestRun, error) {
	run, err := s.st.DigestRun().GetBySlotAttempt(ctx, repo_digestrun.GetBySlotAttemptParams{
		Team: team, RunDate: day, Slot: slot, Attempt: int32(attempt),
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("lookup digest run: %w", err)
	}
	return run, nil
}

// outcomeForRun describes a run that already exists, so a retried job reports
// the same thing the first one did.
func (s *Service) outcomeForRun(ctx context.Context, run *models.DigestRun) (*DigestOutcome, error) {
	msgs, err := s.st.DigestMessage().ListByRun(ctx, run.ID)
	if err != nil {
		return nil, fmt.Errorf("list digest messages: %w", err)
	}
	out := &DigestOutcome{
		RunID: run.ID, Status: run.Status, MRCount: int(run.MrCount),
		LinearIssueCount: int(run.LinearIssueCount),
	}
	if run.Status != DigestDryRun {
		for _, m := range msgs {
			out.Messages = append(out.Messages, m.ID)
		}
	}
	s.log.Infow("digest already built for this slot; reusing it",
		"team", run.Team, "slot", run.Slot, "attempt", run.Attempt, "status", run.Status)
	return out, nil
}

// gatherTeam inspects every repository of a team and returns the snapshots plus
// how many repositories could not be inspected. A repository failure is never
// fatal: one 500 must not cost the other nine their digest (§17).
func (s *Service) gatherTeam(ctx context.Context, team domain.Team) ([]domain.MergeRequestSnapshot, int) {
	loader := s.newSnapshotLoader(depthDigest)
	var (
		snapshots []domain.MergeRequestSnapshot
		failed    int
	)
	for _, repository := range team.Repositories {
		snaps, err := s.inspectRepository(ctx, loader, team, repository)
		if err != nil {
			failed++
			s.log.Warnw("repository could not be inspected for the digest",
				"team", team.Name, "repository", repository, "err", err)
			continue
		}
		snapshots = append(snapshots, snaps...)
	}
	return snapshots, failed
}

func (s *Service) gatherLinear(
	ctx context.Context,
	team domain.Team,
	snapshots []domain.MergeRequestSnapshot,
) (linearDigestState, error) {
	if len(team.LinearTeamIDs) == 0 {
		return linearDigestState{}, nil
	}
	if s.linear == nil {
		return linearDigestState{}, errors.New("linear client is not configured")
	}
	inReview, err := s.linear.ListIssuesInReview(ctx, team.LinearTeamIDs)
	if err != nil {
		return linearDigestState{}, err
	}

	// The board total is known from here on, and every later failure degrades
	// only the per-MR *links*. Both are returned together for that reason: an
	// earlier version discarded the whole state when the batch lookup failed, so
	// a Linear board the service had just counted at 8 was rendered as no count
	// at all, persisted as linear_issue_count = 0 and left the gauge stale —
	// which is precisely the false zero §7 forbids. The reviewer gate needs no
	// protection here: an empty issuesByMR already fails it open.
	state := linearDigestState{
		enabled:       true,
		inReviewCount: len(inReview),
		issuesByMR:    make(map[string]linear.Issue),
	}

	issues := make(map[string]linear.Issue, len(inReview))
	for _, issue := range inReview {
		issues[strings.ToUpper(issue.Identifier)] = issue
	}
	numbers := linearIssueNumbers(snapshots)
	if len(numbers) > 0 {
		linked, err := s.linear.ListIssuesByNumbers(ctx, team.LinearTeamIDs, numbers)
		if err != nil {
			return state, err
		}
		for _, issue := range linked {
			issues[strings.ToUpper(issue.Identifier)] = issue
		}
	}

	for _, snapshot := range snapshots {
		match := matchLinearIssue(snapshot.MR, issues)
		if !match.found {
			continue
		}
		state.issuesByMR[snapshotKey(snapshot.Project.ID, snapshot.MR.IID)] = match.issue
		if len(match.conflicts) > 0 {
			s.log.Warnw("merge request references multiple Linear issues; using the first valid match",
				"project", snapshot.Project.FullPath, "iid", snapshot.MR.IID,
				"selected", match.issue.Identifier, "selected_from", match.matchField,
				"ignored", match.conflicts)
		}
	}
	return state, nil
}

// inspectRepository loads every open MR of one repository through the shared
// loader, so a repository listed by two teams — or twice by one — is fetched
// once per run (§9.4).
func (s *Service) inspectRepository(ctx context.Context, loader *snapshotLoader, team domain.Team, repository string) ([]domain.MergeRequestSnapshot, error) {
	proj, err := s.gl.GetProject(ctx, projectKey(0, repository))
	if err != nil {
		return nil, fmt.Errorf("get project %s: %w", repository, err)
	}
	open, err := s.gl.ListOpenMRs(ctx, projectKey(proj.ID, proj.PathWithNamespace))
	if err != nil {
		return nil, fmt.Errorf("list open merge requests of %s: %w", proj.PathWithNamespace, err)
	}

	iids := make([]int64, 0, len(open))
	for _, mr := range open {
		iids = append(iids, mr.IID)
	}
	loader.primeReviewStates(ctx, proj.PathWithNamespace, iids)

	out := make([]domain.MergeRequestSnapshot, 0, len(open))
	for _, mr := range open {
		loaded, err := loader.load(ctx, team.Name, proj, mr.IID)
		if err != nil {
			// One unreadable MR degrades that MR, not the repository.
			s.log.Warnw("merge request could not be inspected",
				"project", proj.PathWithNamespace, "iid", mr.IID, "err", err)
			continue
		}
		out = append(out, loaded.Snapshot)
	}
	return out, nil
}

// teamState counts the four per-team gauges from a pass's snapshots.
func teamState(snapshots []domain.MergeRequestSnapshot, linearState linearDigestState) metrics.TeamState {
	var st metrics.TeamState
	for _, snap := range snapshots {
		_, linked := linearState.issueFor(snap)
		for _, r := range snap.Reviewers {
			if needsReviewerAction(snap, r, linked) {
				st.WaitingHumanReview++
				break
			}
		}
		actions := domain.ClassifyAuthorActions(snap)
		if len(actions.ChangesRequestedBy) > 0 {
			st.ChangesRequested++
		}
		if actions.UnresolvedThreads > 0 {
			st.UnresolvedThreads++
		}
		if actions.HasConflicts {
			st.Conflicts++
		}
		if actions.PipelineFailed {
			st.FailedPipeline++
		}
	}
	return st
}

// digestData turns classified snapshots into the Block Kit builder's input and
// reports how many distinct MRs the digest actually mentions.
func (s *Service) digestData(
	ctx context.Context,
	team domain.Team,
	snapshots []domain.MergeRequestSnapshot,
	failedRepos int,
	linearState linearDigestState,
) (slack.DigestData, int) {
	now := s.now()
	mentions := map[string]slack.Mention{}

	// One bucket per person, holding both halves of what they owe — and the bucket
	// is the rendered shape itself, so nothing has to be copied field by field
	// later. Two separate groupings (by reviewer, then by author) put the same
	// person in two places and the same merge request under every one of its
	// reviewers.
	people := map[string]*slack.PersonDigest{}
	at := func(u domain.User) *slack.PersonDigest {
		k := userKey(u)
		p := people[k]
		if p == nil {
			p = &slack.PersonDigest{Person: s.mention(ctx, mentions, u)}
			people[k] = p
		}
		return p
	}
	counted := map[string]bool{}

	// The per-row project label is dropped only when the whole digest is one
	// project, so the only facts to carry are which project and whether a second
	// one turned up; a set of paths existed only to be measured. The count
	// saturates at two — "more than one" is the entire rule.
	var soleProject string
	projectCount := 0
	noteProject := func(fullPath string) {
		switch {
		case projectCount == 0:
			soleProject, projectCount = fullPath, 1
		case fullPath != soleProject:
			projectCount = 2
		}
	}

	// Snapshots arrive grouped by repository and ordered by GitLab; sorting
	// here makes the digest read the same way from one run to the next
	// regardless of that ordering.
	ordered := make([]domain.MergeRequestSnapshot, len(snapshots))
	copy(ordered, snapshots)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Project.FullPath != ordered[j].Project.FullPath {
			return ordered[i].Project.FullPath < ordered[j].Project.FullPath
		}
		return ordered[i].MR.IID < ordered[j].MR.IID
	})

	for _, snap := range ordered {
		key := snapshotKey(snap.Project.ID, snap.MR.IID)
		issue, linked := linearState.issueFor(snap)

		for _, r := range snap.Reviewers {
			if !needsReviewerAction(snap, r, linked) {
				continue
			}
			p := at(r.User)
			p.ToReview = append(p.ToReview, slack.ReviewItem{
				Project: projectLabel(snap.Project.FullPath),
				IID:     snap.MR.IID,
				Title:   snap.MR.Title,
				WebURL:  snap.MR.WebURL,
				Waiting: waitingFor(snap, now),
			})
			counted[key] = true
			noteProject(snap.Project.FullPath)
		}

		actions := domain.ClassifyAuthorActions(snap)
		moveLinear := needsLinearMove(snap, issue, linked)
		if !actions.Any() && !moveLinear {
			continue
		}
		// The reviewers who asked for changes are named, so the author knows who to
		// go back to. Resolved through the same per-run cache as everyone else, so a
		// reviewer who is also an author costs no extra lookup.
		var requested []slack.Mention
		for _, u := range actions.ChangesRequestedBy {
			requested = append(requested, s.mention(ctx, mentions, u))
		}
		p := at(snap.MR.Author)
		p.Own = append(p.Own, slack.AuthorItem{
			Project:            projectLabel(snap.Project.FullPath),
			IID:                snap.MR.IID,
			Title:              snap.MR.Title,
			WebURL:             snap.MR.WebURL,
			ChangesRequestedBy: requested,
			UnresolvedThreads:  actions.UnresolvedThreads,
			MergeConflicts:     actions.HasConflicts,
			PipelineFailed:     actions.PipelineFailed,
			PipelineWebURL:     actions.Pipeline.WebURL,
			MoveLinear:         moveLinear,
			LinearIdentifier:   issue.Identifier,
			LinearWebURL:       issue.URL,
		})
		counted[key] = true
		noteProject(snap.Project.FullPath)
	}

	data := slack.DigestData{
		Team: team.Name, FailedRepos: failedRepos,
		LinearEnabled: linearState.enabled, LinearInReviewCount: linearState.inReviewCount,
	}
	// Only a single-project digest can drop the per-row project label.
	if projectCount == 1 {
		data.Project = projectLabel(soleProject)
	}

	for _, k := range orderedPeople(people) {
		p := people[k]
		// Oldest first: the detail rows a reader gets are the ones that have been
		// ignored longest, and the tail is the rest.
		sort.SliceStable(p.ToReview, func(i, j int) bool { return p.ToReview[i].Waiting > p.ToReview[j].Waiting })
		data.People = append(data.People, *p)
	}
	return data, len(counted)
}

// orderedPeople sorts the digest's people by how much they owe, most first, and
// falls back to the key so a tie is stable from one run to the next. The busiest
// queue is the one a reader is looking for; alphabetical order buried it.
func orderedPeople(people map[string]*slack.PersonDigest) []string {
	keys := make([]string, 0, len(people))
	for k := range people {
		keys = append(keys, k)
	}
	sort.SliceStable(keys, func(i, j int) bool {
		ti, tj := people[keys[i]].Total(), people[keys[j]].Total()
		if ti != tj {
			return ti > tj
		}
		return keys[i] < keys[j]
	})
	return keys
}

// waitingFor is how long the MR has been waiting on its reviewers: since the
// last push, i.e. "how long nobody reacted to the MR's current state". GitLab
// does not expose when a reviewer was assigned (§20.3), and the last push is
// the moment the previous review, if any, stopped being valid.
func waitingFor(snap domain.MergeRequestSnapshot, now time.Time) time.Duration {
	if snap.LastPushAt.IsZero() {
		return 0
	}
	if d := now.Sub(snap.LastPushAt); d > 0 {
		return d
	}
	return 0
}

// mention resolves a GitLab user to the way the digest addresses them, once per
// run per user. A directory that is down degrades to a plain name rather than
// failing the digest: a digest nobody is @-mentioned in still tells the team
// what is waiting.
func (s *Service) mention(ctx context.Context, cache map[string]slack.Mention, u domain.User) slack.Mention {
	key := userKey(u)
	if m, ok := cache[key]; ok {
		return m
	}
	gu := match.GitLabUser{Username: u.Username, Name: u.Name, Email: u.Email}

	m := slack.Mention{Display: match.Fallback(gu)}
	if s.matcher != nil {
		r, err := s.matcher.Match(ctx, gu)
		if err != nil {
			s.log.Warnw("slack user match failed; naming the user without a mention",
				"gitlab_user", u.Username, "err", err)
			metrics.SlackUserMatch(metrics.MatchNotFound)
		} else {
			metrics.SlackUserMatch(r.Status.String())
			// A user_map entry that did not resolve is a configuration mistake, not
			// an ordinary unmatched user, and it is invisible in the digest itself —
			// the person is simply named without a ping, exactly like everyone the
			// directory does not know. This is the only place it can be reported.
			if r.Note != "" {
				s.log.Warnw("slack user_map entry did not resolve; naming the user without a mention",
					"gitlab_user", u.Username, "detail", r.Note)
			}
			m = slack.MentionFrom(r)
		}
	}
	cache[key] = m
	return m
}

type persistDigestInput struct {
	// reuseID is set when a previous failed attempt already owns the row for
	// this (team, run_date, slot, attempt).
	reuseID          uuid.UUID
	team             domain.Team
	slot             string
	runDate          time.Time
	attempt          int
	status           string
	messageStatus    string
	mrCount          int
	linearIssueCount int
	messages         []slack.Message
}

// persistDigest writes the run and all its parts in one transaction: a run row
// whose messages are missing would be reported as built while nothing could
// ever be delivered.
func (s *Service) persistDigest(ctx context.Context, in persistDigestInput) (uuid.UUID, []uuid.UUID, error) {
	var (
		runID uuid.UUID
		ids   []uuid.UUID
	)
	err := s.st.RunInTx(ctx, func(tx pgx.Tx) error {
		if in.reuseID != uuid.Nil {
			runID = in.reuseID
			if err := s.st.SetDigestRunStatus(ctx, repo_digestrun.SetStatusParams{
				Status: in.status, Parts: int32(len(in.messages)), MrCount: int32(in.mrCount),
				LinearIssueCount: int32(in.linearIssueCount), ID: runID,
			}, store.WithTx(tx)); err != nil {
				return fmt.Errorf("update digest run: %w", err)
			}
		} else {
			created, err := s.st.CreateDigestRun(ctx, repo_digestrun.CreateParams{
				Team:             in.team.Name,
				Slot:             in.slot,
				RunDate:          in.runDate,
				Attempt:          int32(in.attempt),
				Status:           in.status,
				Parts:            int32(len(in.messages)),
				MrCount:          int32(in.mrCount),
				LinearIssueCount: int32(in.linearIssueCount),
			}, store.WithTx(tx))
			if err != nil {
				return fmt.Errorf("create digest run: %w", err)
			}
			runID = created.ID
		}

		for _, m := range in.messages {
			payload, err := dbtypes.Marshal(m)
			if err != nil {
				return fmt.Errorf("marshal digest part %d: %w", m.Part, err)
			}
			row, err := s.st.CreateDigestMessage(ctx, repo_digestmessage.CreateParams{
				DigestRunID: runID,
				PartNo:      int32(m.Part),
				PartsTotal:  int32(m.Parts),
				Channel:     in.team.SlackChannel,
				Payload:     payload,
				Status:      in.messageStatus,
			}, store.WithTx(tx))
			if err != nil {
				return fmt.Errorf("create digest part %d: %w", m.Part, err)
			}
			ids = append(ids, row.ID)
		}
		return nil
	})
	if err != nil {
		return uuid.Nil, nil, err
	}
	return runID, ids, nil
}

// recordDigestFailure leaves a journal entry for a run that never got as far as
// building anything, so an operator can see the slot was attempted.
func (s *Service) recordDigestFailure(ctx context.Context, team domain.Team, slot string, day time.Time, attempt int, reuseID uuid.UUID, cause error) {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	msg := security.Truncate(security.Mask(cause.Error()), maxStoredErrorLen)
	var err error
	if reuseID != uuid.Nil {
		err = s.st.DigestRun().SetStatus(writeCtx, repo_digestrun.SetStatusParams{
			Status: DigestFailed, Error: msg, ID: reuseID,
		})
	} else {
		_, err = s.st.DigestRun().Create(writeCtx, repo_digestrun.CreateParams{
			Team: team.Name, Slot: slot, RunDate: day, Attempt: int32(attempt),
			Status: DigestFailed, Error: msg,
		})
	}
	if err != nil {
		s.log.Errorw("recording the failed digest run failed", "team", team.Name, "slot", slot, "err", err)
	}
}

// logDigestPreview renders what would have been sent. A dry run is only useful
// if everything except the network call actually happened and can be inspected.
func (s *Service) logDigestPreview(team domain.Team, messages []slack.Message) {
	if len(messages) == 0 {
		s.log.Infow("digest dry-run: nothing to report", "team", team.Name, "channel", team.SlackChannel)
		return
	}
	for _, m := range messages {
		payload, err := dbtypes.Marshal(m.Post(team.SlackChannel))
		if err != nil {
			s.log.Warnw("digest dry-run preview unavailable", "team", team.Name, "part", m.Part, "err", err)
			continue
		}
		s.log.Infow("digest dry-run preview (nothing was sent)",
			"team", team.Name, "channel", team.SlackChannel,
			"part", m.Part, "parts", m.Parts,
			"payload", security.Truncate(string(payload), maxPreviewBytes))
	}
}

// truncateDay normalizes a timestamp to the date the digest_runs.run_date
// column stores. UTC is used deliberately: the caller already decided which
// calendar day the slot belongs to, and re-interpreting it in a container's
// local zone would move the slot.
func truncateDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// userKey identifies a GitLab account for grouping. Username is stable and
// always present; the id is not, on note authors from older payloads.
func userKey(u domain.User) string {
	if name := strings.ToLower(strings.TrimSpace(u.Username)); name != "" {
		return name
	}
	return fmt.Sprintf("id:%d", u.ID)
}

// projectLabel shortens a full path to the repository name the digest shows.
func projectLabel(fullPath string) string {
	if i := strings.LastIndex(fullPath, "/"); i >= 0 && i+1 < len(fullPath) {
		return fullPath[i+1:]
	}
	return fullPath
}
