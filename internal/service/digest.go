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
	Messages []uuid.UUID
	Status   string // built | dry_run | partial | failed
	MRCount  int
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
	// This is the only place that sees a whole team at once, so it is the only
	// place that can set the per-team gauges honestly. They are overwritten
	// every pass, including when every count is zero: a team that drops to zero
	// must report zero rather than keep its last non-zero value forever.
	metrics.SetTeamState(team.Name, teamState(snapshots))

	if failedRepos > 0 && len(snapshots) == 0 && len(team.Repositories) > 0 {
		// Nothing was learned at all, so there is no digest to build — as
		// opposed to a partial one, which is a normal result.
		err := fmt.Errorf("digest for team %q: all %d repositories failed inspection", team.Name, failedRepos)
		s.recordDigestFailure(ctx, team, slot, day, attempt, reuseID, err)
		metrics.DigestRun(team.Name, metrics.ResultError)
		return nil, err
	}

	data, mrCount := s.digestData(ctx, team, snapshots, failedRepos)
	messages := slack.BuildDigest(data)

	status, messageStatus := digestStatuses(s.cfg.SlackSendEnabled, failedRepos)
	if !s.cfg.SlackSendEnabled {
		s.logDigestPreview(team, messages)
	}

	runID, ids, err := s.persistDigest(ctx, persistDigestInput{
		reuseID:       reuseID,
		team:          team,
		slot:          slot,
		runDate:       day,
		attempt:       attempt,
		status:        status,
		messageStatus: messageStatus,
		mrCount:       mrCount,
		messages:      messages,
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
		"status", status, "parts", len(messages), "mrs", mrCount, "failed_repos", failedRepos)

	out := &DigestOutcome{RunID: runID, Status: status, MRCount: mrCount}
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
	out := &DigestOutcome{RunID: run.ID, Status: run.Status, MRCount: int(run.MrCount)}
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
func teamState(snapshots []domain.MergeRequestSnapshot) metrics.TeamState {
	var st metrics.TeamState
	for _, snap := range snapshots {
		for _, r := range snap.Reviewers {
			if domain.NeedsHumanReview(snap, r) {
				st.WaitingHumanReview++
				break
			}
		}
		actions := domain.ClassifyAuthorActions(snap)
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
func (s *Service) digestData(ctx context.Context, team domain.Team, snapshots []domain.MergeRequestSnapshot, failedRepos int) (slack.DigestData, int) {
	now := s.now()
	mentions := map[string]slack.Mention{}

	type reviewerBucket struct {
		mention slack.Mention
		items   []slack.ReviewItem
	}
	type authorBucket struct {
		mention slack.Mention
		items   []slack.AuthorItem
	}
	reviewers := map[string]*reviewerBucket{}
	authors := map[string]*authorBucket{}
	counted := map[string]bool{}

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
		author := s.mention(ctx, mentions, snap.MR.Author)

		for _, r := range snap.Reviewers {
			if !domain.NeedsHumanReview(snap, r) {
				continue
			}
			rk := userKey(r.User)
			b := reviewers[rk]
			if b == nil {
				b = &reviewerBucket{mention: s.mention(ctx, mentions, r.User)}
				reviewers[rk] = b
			}
			b.items = append(b.items, slack.ReviewItem{
				Project: projectLabel(snap.Project.FullPath),
				IID:     snap.MR.IID,
				Title:   snap.MR.Title,
				WebURL:  snap.MR.WebURL,
				Author:  author,
				Waiting: waitingFor(snap, now),
			})
			counted[key] = true
		}

		actions := domain.ClassifyAuthorActions(snap)
		if !actions.Any() {
			continue
		}
		ak := userKey(snap.MR.Author)
		b := authors[ak]
		if b == nil {
			b = &authorBucket{mention: author}
			authors[ak] = b
		}
		b.items = append(b.items, slack.AuthorItem{
			Project:           projectLabel(snap.Project.FullPath),
			IID:               snap.MR.IID,
			Title:             snap.MR.Title,
			WebURL:            snap.MR.WebURL,
			UnresolvedThreads: actions.UnresolvedThreads,
			MergeConflicts:    actions.HasConflicts,
			PipelineFailed:    actions.PipelineFailed,
			PipelineWebURL:    actions.Pipeline.WebURL,
		})
		counted[key] = true
	}

	data := slack.DigestData{Team: team.Name, FailedRepos: failedRepos}
	for _, k := range sortedKeys(reviewers) {
		data.ReviewsNeeded = append(data.ReviewsNeeded, slack.ReviewerGroup{
			Reviewer: reviewers[k].mention, MRs: reviewers[k].items,
		})
	}
	for _, k := range sortedKeys(authors) {
		data.AuthorActions = append(data.AuthorActions, slack.AuthorGroup{
			Author: authors[k].mention, MRs: authors[k].items,
		})
	}
	return data, len(counted)
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
			m = slack.MentionFrom(r)
		}
	}
	cache[key] = m
	return m
}

type persistDigestInput struct {
	// reuseID is set when a previous failed attempt already owns the row for
	// this (team, run_date, slot, attempt).
	reuseID       uuid.UUID
	team          domain.Team
	slot          string
	runDate       time.Time
	attempt       int
	status        string
	messageStatus string
	mrCount       int
	messages      []slack.Message
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
				Status: in.status, Parts: int32(len(in.messages)), MrCount: int32(in.mrCount), ID: runID,
			}, store.WithTx(tx)); err != nil {
				return fmt.Errorf("update digest run: %w", err)
			}
		} else {
			created, err := s.st.CreateDigestRun(ctx, repo_digestrun.CreateParams{
				Team:    in.team.Name,
				Slot:    in.slot,
				RunDate: in.runDate,
				Attempt: int32(in.attempt),
				Status:  in.status,
				Parts:   int32(len(in.messages)),
				MrCount: int32(in.mrCount),
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

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
