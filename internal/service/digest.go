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
	// Reused marks an outcome that describes a run this call did not build: the
	// slot was already there. Without it the worker's summary says "digest built"
	// for one assembled hours earlier, which is the same mistake the log made
	// about a repository nobody scanned.
	Reused bool
	// BuiltAt is when the run was assembled, and is set only when Reused: for a
	// fresh build the log line's own timestamp already says it. It is what turns
	// the worker's line from "nothing reassembled" into "the slot you are looking
	// for went out three hours ago".
	BuiltAt time.Time
	// FailedRepos and LinearDegraded are the degradations behind a 'partial'
	// status. They are reported rather than logged here because the worker writes
	// the one summary line per job, and a status alone does not say which source
	// failed.
	//
	// Meaningful only when Reused is false. digest_runs stores the status but not
	// what degraded, so a reused run cannot recover them — and the worker leaves
	// the fields off that line rather than printing zeros, because "nothing
	// degraded" and "this call never asked" must not look alike.
	FailedRepos    int
	LinearDegraded bool
	// Parts is how many messages the run holds, from the row rather than from
	// Messages: Messages is deliberately empty for a dry run, so counting it
	// reported "parts 0" for a digest that has parts and simply is not being
	// delivered.
	Parts int
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
			// rather than assembling a second copy. outcomeForRun logs it.
			return s.outcomeForRun(ctx, existing)
		}
		// A failed attempt left a row but no messages; the unique index means
		// we must reuse it rather than insert a twin.
		reuseID = existing.ID
	}

	src := s.gatherDigestSources(ctx, team)
	// Published before the early return below: a run that learned nothing is
	// still a run, and a source failure that aborts the digest is the one an
	// operator most wants counted.
	s.publishDigestMetrics(src)

	if err := src.unusable(); err != nil {
		s.recordDigestFailure(ctx, team, slot, day, attempt, reuseID, err)
		metrics.DigestRun(team.Name, metrics.ResultError)
		return nil, err
	}

	// The builder's input is the preview's business; what gets persisted and
	// delivered is the rendered payload.
	_, messages, mrCount := s.renderDigest(ctx, src)

	failedSources := src.failedRepos
	if src.linearDegraded() {
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
		linearIssueCount: src.linear.inReviewCount,
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

	// No line here: DigestWorker writes one per job and it carries everything
	// this one did, plus the job id. Two lines for one build drift apart the first
	// time either is edited — which is exactly how "digest built" ended up
	// underneath "already built for this slot".
	out := &DigestOutcome{
		RunID: runID, Status: status, MRCount: mrCount,
		LinearIssueCount: src.linear.inReviewCount,
		FailedRepos:      src.failedRepos, LinearDegraded: src.linearDegraded(),
		Parts: len(messages),
	}
	if s.cfg.SlackSendEnabled {
		out.Messages = ids
	}
	return out, nil
}

// DigestPreview is one team's digest assembled but not recorded: no
// digest_runs row, no digest_messages, no delivery job, no chat.postMessage.
// It is what `digest --dry-run` prints.
type DigestPreview struct {
	Team domain.Team
	// Data is the builder's input, kept alongside the rendered messages because
	// it is the only place a Slack id can be traced back to the person it stands
	// for — a terminal renders "<@U024BE7LH>", Slack renders a name.
	Data     slack.DigestData
	Messages []slack.Message
	// MRCount is how many distinct merge requests the digest mentions.
	MRCount          int
	LinearIssueCount int
	// FailedRepos and LinearErr are the same degradations BuildDigest turns into
	// a 'partial' run. A preview has no status column to record them in, so it
	// reports them instead.
	FailedRepos int
	LinearErr   error
}

// PreviewDigest assembles a team's digest and returns it without writing
// anything anywhere.
//
// It is the same assembly BuildDigest performs — the same two sources, the same
// classification, the same Block Kit renderer — with the persistence and the
// delivery removed rather than reimplemented, which is what makes the preview
// worth trusting: a second renderer would eventually disagree with the one that
// ships.
//
// Two things a build does and a preview must not. It publishes no metrics: the
// per-team gauges describe the *service's* view of a team, and a CLI process
// that exits a second later would either report into a registry nobody scrapes
// or, worse, teach an operator to read a number that a hand-run command moved.
// And it takes no slot, so it can never collide with the digest_runs unique
// index that stops two replicas double-sending a slot.
func (s *Service) PreviewDigest(ctx context.Context, team domain.Team) (*DigestPreview, error) {
	src := s.gatherDigestSources(ctx, team)
	if err := src.unusable(); err != nil {
		return nil, err
	}
	data, messages, mrCount := s.renderDigest(ctx, src)
	return &DigestPreview{
		Team:             team,
		Data:             data,
		Messages:         messages,
		MRCount:          mrCount,
		LinearIssueCount: src.linear.inReviewCount,
		FailedRepos:      src.failedRepos,
		LinearErr:        src.linearErr,
	}, nil
}

// digestSources is everything one digest read before anything was rendered. It
// exists so the scheduled build and the CLI preview cannot drift: both gather
// through gatherDigestSources and render through renderDigest, and the only
// difference between them is what happens to the result.
type digestSources struct {
	team        domain.Team
	snapshots   []domain.MergeRequestSnapshot
	failedRepos int
	linear      linearDigestState
	// linearErr is the failure gatherLinear reported, or nil. It is not itself
	// the degradation flag — see linearDegraded.
	linearErr error
}

// gatherDigestSources reads GitLab and then Linear for one team.
func (s *Service) gatherDigestSources(ctx context.Context, team domain.Team) digestSources {
	snapshots, failedRepos := s.gatherTeam(ctx, team)
	linearState, linearErr := s.gatherLinear(ctx, team, snapshots)
	return digestSources{
		team: team, snapshots: snapshots, failedRepos: failedRepos,
		linear: linearState, linearErr: linearErr,
	}
}

// linearConfigured reports whether this team declares any Linear team at all. A
// deployment without linear_team_ids can neither degrade nor warn.
func (src digestSources) linearConfigured() bool { return len(src.team.LinearTeamIDs) > 0 }

// linearDegraded asks whether anything about Linear failed, and drives the
// partial status and the warning. It is a different question from
// linear.enabled, which asks whether the board total actually arrived and is the
// only one allowed to decide that Linear contributed nothing at all.
func (src digestSources) linearDegraded() bool {
	return src.linearConfigured() && src.linearErr != nil
}

// unusable reports the one case where there is no digest to build at all, as
// opposed to a partial one, which is a normal result: every repository failed
// and Linear either is not configured or did not answer either.
func (src digestSources) unusable() error {
	gitLabUnavailable := src.failedRepos > 0 && len(src.snapshots) == 0 && len(src.team.Repositories) > 0
	if !gitLabUnavailable || (src.linearConfigured() && src.linear.enabled) {
		return nil
	}
	return fmt.Errorf("digest for team %q: all configured sources failed (GitLab repositories: %d, Linear configured: %t)",
		src.team.Name, src.failedRepos, src.linearConfigured())
}

// publishDigestMetrics sets everything one build owns.
//
// This is the only place that sees a whole team at once, so it is the only place
// that can set the per-team gauges honestly — overwritten every pass, including
// when every count is zero, because a team that drops to zero must report zero
// rather than keep its last non-zero value forever.
func (s *Service) publishDigestMetrics(src digestSources) {
	team := src.team
	st := teamState(src.snapshots, src.linear)
	metrics.SetTeamState(team.Name, st)
	// Same rule as the count below, for the same reason: the count is a hard zero
	// whenever the gate could not run, which is the claim "nothing is stuck before
	// In Review" at exactly the moment the service stopped being able to tell — and
	// the same build pushes those merge requests back onto reviewers, so
	// waiting_human_review jumps in the same scrape and the pair reads as work
	// moving forward.
	//
	// **Both** halves have to be known, and gateKnown alone is not enough: it is
	// decided before the batch lookup, so a failed ListIssuesByNumbers leaves it
	// true with an empty linksByMR — nothing is linked, nothing grades "not ready",
	// and the gauge publishes the same false zero by the other door. A team that
	// dropped Linear must also stop publishing it.
	if src.linearConfigured() && src.linear.gateKnown && src.linear.linksKnown {
		metrics.SetLinearNotReady(team.Name, st.LinearNotReady)
	} else {
		metrics.ClearLinearNotReady(team.Name)
	}
	if src.linear.enabled {
		metrics.SetLinearIssuesInReview(team.Name, src.linear.inReviewCount)
	} else {
		// Unknown, so the series goes absent rather than stale or falsely zero —
		// see metrics.ClearLinearIssuesInReview. Covers both the outage and the
		// team that no longer declares linear_team_ids.
		metrics.ClearLinearIssuesInReview(team.Name)
	}
	if src.failedRepos > 0 {
		metrics.DigestSourceError(team.Name, metrics.SourceGitLab)
	}
	if src.linearDegraded() {
		metrics.DigestSourceError(team.Name, metrics.SourceLinear)
	}
}

// renderDigest classifies the gathered sources and renders the Slack messages,
// returning the builder's input alongside them and how many distinct merge
// requests the digest mentions.
func (s *Service) renderDigest(ctx context.Context, src digestSources) (slack.DigestData, []slack.Message, int) {
	team := src.team
	data, mrCount := s.digestData(ctx, team, src.snapshots, src.failedRepos, src.linear)
	if src.linearDegraded() {
		data.Warnings = append(data.Warnings, linearWarning(src.linear))
		s.log.Warnw("Linear could not be fully inspected for the digest",
			"team", team.Name, "count_known", src.linear.enabled,
			"links_known", src.linear.linksKnown, "gate_known", src.linear.gateKnown,
			"gate_affected_links", src.linear.gateDegradedLinks, "err", src.linearErr)
	}
	return data, slack.BuildDigest(data), mrCount
}

// linearWarning names the degradation that actually happened.
//
// The four are not interchangeable. "could not be inspected" explains an absent
// count; with the count present it would read as a contradiction of the line
// right above it, and hide which half is stale. Selected on the specific fact
// that degraded rather than by elimination: a fifth failure mode added later must
// not silently inherit the fourth one's words. Each string may only claim what
// actually happened — the version that announced "every linked merge request was
// treated as ready for review" printed that above a row proving the opposite.
//
// The four arms are exhaustive over the errors gatherLinear can return today, so
// the default is unreachable. It stays as the landing place for that fifth mode,
// because the alternative is an empty warning string appended to a digest.
func linearWarning(state linearDigestState) string {
	switch {
	case !state.enabled:
		return "Partial data: Linear could not be inspected."
	case !state.linksKnown:
		return "Partial data: Linear issue links could not be resolved; the In Review count is current."
	case state.gateDegradedLinks:
		// "kept notifying reviewers" would be false for an already-approved merge
		// request on that board: the completion rule reads the status *name*, which
		// needs no ordering, so it can still silence everyone. "Not graded" is what
		// actually happened in every case.
		return "Partial data: a Linear board could not be ordered; merge requests linked to it were not graded against it."
	case !state.gateKnown:
		return "Partial data: a Linear board could not be ordered; no merge request in this digest was linked to it."
	default:
		return "Partial data: Linear could not be fully inspected."
	}
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
// that slot already ran" would make the scheduled attempt 0 collide with
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
		Reused:           true,
		BuiltAt:          run.CreatedAt,
		Parts:            int(run.Parts),
	}
	if run.Status != DigestDryRun {
		for _, m := range msgs {
			out.Messages = append(out.Messages, m.ID)
		}
	}
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

// linearGatherTimeout bounds everything one digest build spends on Linear.
//
// Without it the Linear phase can consume the whole job. Each request carries its
// own retry budget (4 attempts, a 15s per-request timeout and an honoured
// Retry-After of up to 60s), so one call's worst case is minutes, and the phase
// makes 2 + one-per-configured-Linear-team of them serially. Against
// jobs.digestTimeout that arithmetic already overruns with a single Linear team
// once Linear starts answering 429 — which is the *expected* failure, since every
// team's digest fires in the same slot on one API key. Overrunning loses the whole
// digest including complete GitLab data, and then re-pays for it on retry, which
// inverts the contract that a Linear failure is non-fatal. Bounded, the same
// outage degrades to `partial`. doctor bounds its own Linear fan-out for the same
// reason.
const linearGatherTimeout = 2 * time.Minute

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
	ctx, cancel := context.WithTimeout(ctx, linearGatherTimeout)
	defer cancel()
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
		linksByMR:     make(map[string]linearLink),
	}

	// The workflows are gathered before the links but their failure is not fatal,
	// and that asymmetry is the whole degradation contract of the readiness gate:
	// a team whose columns could not be read leaves every one of its issues at
	// linear.StageUnknown, which every rule reads as "ask the reviewers, as
	// before". A board that cannot be ordered must not silence anybody, so this
	// records the problem and carries on rather than returning.
	workflows, unordered, workflowErr := s.gatherLinearWorkflows(ctx, team)
	state.gateKnown = workflowErr == nil

	issues := make(map[string]linear.Issue, len(inReview))
	for _, issue := range inReview {
		issues[strings.ToUpper(issue.Identifier)] = issue
	}
	numbers := linearIssueNumbers(snapshots)
	// Nothing to look up is not a failure: with no candidate numbers the link set
	// is complete and known, it is simply empty. Saying otherwise made a team whose
	// merge requests mention no Linear id report "issue links could not be
	// resolved" whenever its board was unreadable — naming the half that worked.
	state.linksKnown = true
	if len(numbers) > 0 {
		linked, err := s.linear.ListIssuesByNumbers(ctx, team.LinearTeamIDs, numbers)
		if err != nil {
			state.linksKnown = false
			// Joined, not replaced: with both halves broken the workflow error is the
			// one that names a configuration fault ("expected exactly one \"In Review\"
			// workflow state, found 0"), and dropping it left `gate_known=false` in the
			// log with no reason attached anywhere.
			return state, errors.Join(workflowErr, err)
		}
		for _, issue := range linked {
			issues[strings.ToUpper(issue.Identifier)] = issue
		}
	}

	// gateDegraded is the difference between "a board could not be ordered" and
	// "a merge request was affected by that", and the digest warning may only claim
	// the second when it happened. One boolean over N teams reported the digest-wide
	// sentence "every linked merge request was treated as ready for review" even
	// when the unreadable board carried no merge requests at all — printed directly
	// above a row proving the opposite.
	gateDegraded := false
	for _, snapshot := range snapshots {
		match := matchLinearIssue(snapshot.MR, issues)
		if !match.found {
			continue
		}
		// Keyed by the issue's own team, never by the first configured one: a service
		// team may map several Linear teams and each orders its columns
		// independently, so grading an issue against another team's board is how a
		// ready card gets read as unready.
		stageOf := func(issue linear.Issue) linear.Stage {
			teamID := strings.TrimSpace(issue.Team.ID)
			if unordered[teamID] {
				gateDegraded = true
			}
			return workflows[teamID].Stage(issue.State)
		}
		state.linksByMR[snapshotKey(snapshot.Project.ID, snapshot.MR.IID)] = linearLink{
			issue: match.issue,
			stage: linearStage(match, stageOf),
		}
		if len(match.conflicts) > 0 {
			// "ignored" is true of the *rendered* identifier only. Since linearStage the
			// extra candidates decide whether the readiness gate applies at all, so the
			// two outcomes are logged apart: an operator asking "our card is in Backlog,
			// why were three reviewers still pinged?" has no other signal, and the
			// identifier regex produces incidental candidates from ordinary branch names
			// (`feature/CHAIN-184-retry-fix-2` yields FIX-2), so this is reachable
			// without anybody deliberately naming two tickets.
			gated := state.linksByMR[snapshotKey(snapshot.Project.ID, snapshot.MR.IID)].stage
			if gated == linear.StageUnknown {
				metrics.LinearGateAmbiguous(team.Name)
				s.log.Warnw("merge request references Linear issues at different statuses; the readiness gate is off for it",
					"project", snapshot.Project.FullPath, "iid", snapshot.MR.IID,
					"selected", match.issue.Identifier, "selected_from", match.matchField,
					"also_matched", match.conflicts)
			} else {
				s.log.Warnw("merge request references multiple Linear issues at the same status; using the first valid match",
					"project", snapshot.Project.FullPath, "iid", snapshot.MR.IID,
					"selected", match.issue.Identifier, "selected_from", match.matchField,
					"also_matched", match.conflicts)
			}
		}
	}
	state.gateDegradedLinks = gateDegraded
	return state, workflowErr
}

// gatherLinearWorkflows resolves each configured Linear team's column order.
//
// Errors are returned alongside whatever did resolve, per team: one team with a
// duplicated "In Review" state must not cost the others their readiness gate. A
// team missing from the result is not an error at the call site — the zero
// linear.Workflow answers StageUnknown for every state, which is exactly the
// fail-open behaviour, so the caller indexes the map without checking.
// It also returns the set of Linear team ids whose order is missing, so the caller
// can tell whether any merge request actually landed on one of them.
func (s *Service) gatherLinearWorkflows(
	ctx context.Context, team domain.Team,
) (map[string]linear.Workflow, map[string]bool, error) {
	workflows := make(map[string]linear.Workflow, len(team.LinearTeamIDs))
	unordered := make(map[string]bool)
	// The requested id is recorded alongside the resolved one, because a team that
	// answered under a different id would otherwise be unreportable.
	markUnordered := func(ids ...string) {
		for _, id := range ids {
			if id = strings.TrimSpace(id); id != "" {
				unordered[id] = true
			}
		}
	}
	var errs []error
	for _, id := range team.LinearTeamIDs {
		lt, err := s.linear.GetTeam(ctx, id)
		if err != nil {
			errs = append(errs, fmt.Errorf("linear team %s: %w", id, err))
			markUnordered(id)
			continue
		}
		wf, err := linear.NewWorkflow(lt.States)
		if err != nil {
			// A configuration problem rather than an outage, and doctor reports the
			// same one — but doctor runs when somebody asks, and this is what makes
			// the digest say so on the day the column was renamed.
			errs = append(errs, fmt.Errorf("linear team %s: %w", teamLabel(lt, id), err))
			markUnordered(id, lt.ID)
			continue
		}
		// Keyed by the response's own id, which is the form an issue carries.
		workflows[strings.TrimSpace(lt.ID)] = wf
	}
	return workflows, unordered, errors.Join(errs...)
}

// teamLabel names a Linear team for an error message. linear.Team.Label owns the
// "no key means no parentheses" rule so doctor and the digest cannot disagree; the
// requested id is appended because that is what the operator has in their config.
func teamLabel(t *linear.Team, requested string) string {
	return fmt.Sprintf("%s [%s]", t.Label(), strings.TrimSpace(requested))
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

// teamState counts the per-team gauges from a pass's snapshots.
func teamState(snapshots []domain.MergeRequestSnapshot, linearState linearDigestState) metrics.TeamState {
	var st metrics.TeamState
	for _, snap := range snapshots {
		link, linked := linearState.linkFor(snap)
		for _, r := range snap.Reviewers {
			if needsReviewerAction(snap, r, link, linked) {
				st.WaitingHumanReview++
				break
			}
		}
		if needsLinearStart(snap, link, linked) {
			st.LinearNotReady++
		}
		// Counted per merge request, and only where the answer was actually
		// wanted: a snapshot loaded at depthReview never asked for approvals, so
		// counting it here would report an outage on every scan. The digest path is
		// the only one that reaches this function.
		if !snap.ApprovalsKnown {
			st.ApprovalsUnknown++
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
		// The gated answer, not actions.NoReviewers: a gauge that counted merge
		// requests the digest deliberately says nothing about would contradict the
		// message it is meant to explain.
		if needsReviewerTag(snap, link, linked) {
			st.NoReviewers++
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
		link, linked := linearState.linkFor(snap)

		for _, r := range snap.Reviewers {
			if !needsReviewerAction(snap, r, link, linked) {
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
		moveLinear := needsLinearMove(snap, link, linked)
		startLinear := needsLinearStart(snap, link, linked)
		// The board can withdraw this one flag, and only this one: an MR whose card
		// has not been offered for review was not forgotten. Whenever it does,
		// startLinear is true by construction, so the row still exists and still
		// says what to do — see needsReviewerTag.
		addReviewer := needsReviewerTag(snap, link, linked)
		if !actions.Any() && !moveLinear && !startLinear {
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
			AddReviewer:        addReviewer,
			ChangesRequestedBy: requested,
			UnresolvedThreads:  actions.UnresolvedThreads,
			MergeConflicts:     actions.HasConflicts,
			PipelineFailed:     actions.PipelineFailed,
			PipelineWebURL:     actions.Pipeline.WebURL,
			MoveLinear:         moveLinear,
			StartLinear:        startLinear,
			LinearIdentifier:   link.issue.Identifier,
			LinearWebURL:       link.issue.URL,
			LinearState:        link.issue.State.Name,
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

// orderedPeople sorts the digest's people alphabetically by the name the reader
// sees, with the map key as the tiebreak so a run is reproducible.
//
// By display name and not by the key, which is the GitLab username: the key is
// what the service calls a person, the display name is what Slack draws, and on
// a digest full of `<@U024BE7LH>` an order derived from the username reads as no
// order at all. Mention.Display carries the resolved Slack name for a matched
// person and the "John Smith (@john)" fallback for everyone else, so the sort key
// is the rendered text in both cases.
//
// This replaces workload-descending. Ranking the busiest first put the longest
// queue where a reader looks first, and the cost was that nobody could find
// *themselves*: a person's position moved every slot, so reading the digest meant
// scanning the whole thing. A digest is read far more often to answer "what do I
// owe" than "who is drowning" — and the per-person counts in the header
// (`· to review 11`) still answer the second question without an ordering that
// reshuffles between slots.
func orderedPeople(people map[string]*slack.PersonDigest) []string {
	keys := make([]string, 0, len(people))
	for k := range people {
		keys = append(keys, k)
	}
	// Lower-cased so "anna" and "Anna" sort together, which is what alphabetical
	// means to a reader. Byte order beyond that, so a Cyrillic name sorts after
	// every Latin one rather than interleaved — a full Unicode collation would
	// mean a new dependency to settle an order nobody is looking up by letter.
	sortKey := func(k string) string {
		if d := strings.ToLower(strings.TrimSpace(people[k].Person.Display)); d != "" {
			return d
		}
		return k
	}
	sort.SliceStable(keys, func(i, j int) bool {
		si, sj := sortKey(keys[i]), sortKey(keys[j])
		if si != sj {
			return si < sj
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
