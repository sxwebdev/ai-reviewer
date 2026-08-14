package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"
	"github.com/tkcrm/mx/logger"

	"github.com/sxwebdev/ai-reviewer/internal/coverage"
	"github.com/sxwebdev/ai-reviewer/internal/dbtypes"
	"github.com/sxwebdev/ai-reviewer/internal/domain"
	"github.com/sxwebdev/ai-reviewer/internal/gitlab"
	"github.com/sxwebdev/ai-reviewer/internal/metrics"
	"github.com/sxwebdev/ai-reviewer/internal/models"
	"github.com/sxwebdev/ai-reviewer/internal/review"
	"github.com/sxwebdev/ai-reviewer/internal/security"
	"github.com/sxwebdev/ai-reviewer/internal/store"
	"github.com/sxwebdev/ai-reviewer/internal/store/repos/repo_finding"
	"github.com/sxwebdev/ai-reviewer/internal/store/repos/repo_review"
)

// OnPersist runs INSIDE the transaction that writes mr_reviews and mr_findings.
//
// internal/jobs passes a closure that calls river's InsertTx with the
// publish_review job, which is what makes "review recorded but nobody will
// publish it" unrepresentable: a crash between the commit and the enqueue
// cannot happen if there is only one commit. The service owns the transaction
// (and so stays River-free); the job owns what goes into it.
//
// A nil OnPersist means "do not enqueue publication" — the dry-run path (§10.4).
type OnPersist func(ctx context.Context, tx pgx.Tx, reviewID uuid.UUID) error

// ReviewRequest identifies one merge request to review.
type ReviewRequest struct {
	Team string
	// ProjectPath is the GitLab full path ("backend/payments"). ProjectID wins
	// when both are set.
	ProjectPath string
	ProjectID   int64
	MRIID       int64
	// HeadSHA is the SHA the trigger saw, and the review is bound to it: if the
	// merge request's head has moved by the time this runs, nothing is persisted
	// and the outcome is a domain.ReasonHeadMoved skip.
	//
	// Retargeting the live head instead would spend the same tokens, but the
	// queue's uniqueness key is the head as QUEUED — so the moment anyone pushes,
	// the scanner's next pass finds no row for the new SHA, enqueues a second job
	// under a different key, and two full pipelines run the same review. The
	// loser then violates mr_reviews_success_uniq and is recorded as a failure,
	// which feeds a §6.5 strike to a perfectly healthy MR. Skipping costs at most
	// one scan_interval of latency and keeps the key honest.
	//
	// An empty HeadSHA means "whatever GitLab reports now" — the CLI path for an
	// MR nobody has queued.
	HeadSHA string
	// Publish is the EFFECTIVE publish decision, already resolved by the jobs
	// layer from service.ai_review_publish_enabled and the job's own args
	// (§15: job args win over config). False persists the review as a dry run.
	Publish bool
}

// ReviewOutcome reports what one RunReview call did.
type ReviewOutcome struct {
	ReviewID   uuid.UUID
	Status     string // reviewed | dry_run | failed
	Findings   int
	RiskLevel  string
	CostUSD    float64
	DurationMS int64
	// SkipReason is non-empty when the MR did not need a review at all, in
	// which case nothing was persisted and Status is empty.
	SkipReason domain.Reason
}

// Reasons for ai_reviews_failed_total — a small closed set so the metric stays
// groupable.
const (
	failReasonGitLab  = "gitlab"
	failReasonDiff    = "diff"
	failReasonLLM     = "llm"
	failReasonPersist = "persist"
	// failReasonCanceled is our own shutdown, not the merge request's fault. It
	// is a separate label precisely so it can be excluded from any "this MR is
	// poisoned" panel — and the row it would have written is discarded rather
	// than counted (see recordFailure).
	failReasonCanceled = "canceled"
)

// maxStoredErrorLen bounds what a runaway error message can put in a row.
const maxStoredErrorLen = 4000

// inFlightMarker is the `error` an mr_reviews row carries while its review is
// still running. The row is written before the expensive work starts and is
// removed or rewritten when the process gets to say what happened, so a row that
// still holds this text is a review whose process died without a word.
const inFlightMarker = "review started; the process did not report an outcome"

// RunReview reviews one merge request and persists the result.
//
// The order of operations is load-bearing (§10.4):
//
//  1. assemble the snapshot — one MR loaded once (§9.4);
//  2. classify cheaply, before any expensive call, and skip if there is nothing
//     to do;
//  3. build the engine input: diffs, prompt context, existing fingerprints,
//     prior review and interdiff;
//  4. run the engine;
//  5. write mr_reviews, mr_findings and the publication enqueue in ONE
//     transaction.
//
// A failed attempt records status='failed' so the §6.5 backoff can count it; a
// dry run records status='dry_run' so the same head SHA is not re-reviewed
// every scan interval. Both are what stop a deterministically-broken MR from
// burning tokens forever.
func (s *Service) RunReview(ctx context.Context, req ReviewRequest, onPersist OnPersist) (*ReviewOutcome, error) {
	if s.eng == nil {
		return nil, errors.New("service: review engine is not configured")
	}
	start := s.now()
	pk := projectKey(req.ProjectID, req.ProjectPath)

	proj, err := s.gl.GetProject(ctx, pk)
	if err != nil {
		return nil, fmt.Errorf("get project %s: %w", pk, err)
	}
	loaded, err := s.newSnapshotLoader(depthReview).load(ctx, req.Team, proj, req.MRIID)
	if err != nil {
		return nil, fmt.Errorf("load merge request %s!%d: %w", proj.PathWithNamespace, req.MRIID, err)
	}
	snap := loaded.Snapshot
	head := snap.HeadSHA

	if req.HeadSHA != "" && req.HeadSHA != head {
		// See ReviewRequest.HeadSHA: the queue's uniqueness key is the SHA this
		// job was queued for, so reviewing a different one silently disables
		// deduplication. Persist nothing; the scanner enqueues the current head
		// next pass, where alreadyReviewed correctly finds no row.
		metrics.ReviewSkipped(req.Team, domain.ReasonHeadMoved.String())
		s.log.Infow("head sha moved since the review was queued; leaving it to the next scan",
			"project", proj.PathWithNamespace, "iid", req.MRIID, "queued", req.HeadSHA, "current", head)
		return &ReviewOutcome{SkipReason: domain.ReasonHeadMoved}, nil
	}

	// Cheap classification first: the MR may have been merged, turned into a
	// draft, or already reviewed between the scan that queued this job and now.
	recorded, err := s.reviewOfHead(ctx, proj.ID, req.MRIID, head)
	if err != nil {
		return nil, err
	}
	reviewed := ""
	if recorded != nil {
		reviewed = recorded.HeadSha
	}
	ok, reason := domain.NeedsAIReview(snap, s.aiReviewEnabled(req.Team), reviewed)
	if !ok {
		// up_to_date is the one refusal that may still have work behind it: the
		// row satisfying it can be a dry run whose findings have never been
		// published. Every other reason (merged, closed, draft, team switched
		// off) means nothing should reach this merge request at all.
		if reason == domain.ReasonUpToDate {
			if out := s.publishExistingReview(ctx, req, proj, recorded, onPersist); out != nil {
				return out, nil
			}
		}
		metrics.ReviewSkipped(req.Team, reason.String())
		s.log.Infow("skipping review", "project", proj.PathWithNamespace, "iid", req.MRIID,
			"head_sha", head, "reason", reason.String())
		return &ReviewOutcome{SkipReason: reason}, nil
	}

	// The failure count for this SHA becomes the attempt number on whatever row
	// this run writes — the same rows the §6.5 ladder counts.
	failures, err := s.st.Review().FailureStats(ctx, repo_review.FailureStatsParams{
		ProjectID: proj.ID, MrIid: req.MRIID, HeadSha: head,
	})
	if err != nil {
		return nil, fmt.Errorf("failure stats: %w", err)
	}
	attempt := int32(failures.Failures + 1)

	// Everything below is expensive, which is why the classification above had
	// to be cheap — and why the attempt is recorded before it starts.
	inFlight, err := s.startAttempt(ctx, req, proj, head, attempt)
	if err != nil {
		return nil, err
	}

	diffs, err := s.gl.ListMRDiffs(ctx, pk, req.MRIID)
	if err != nil {
		return nil, s.recordFailure(ctx, req, proj, inFlight, head, attempt, failReasonGitLab, fmt.Errorf("list diffs: %w", err))
	}
	files := parseDiffs(diffs, s.cfg.IgnoreGlobs, s.log)
	if len(files) == 0 {
		return nil, s.recordFailure(ctx, req, proj, inFlight, head, attempt, failReasonDiff,
			errors.New("no reviewable changed files (binary, generated, vendored and ignored files are excluded)"))
	}

	workDir, agentMode, cleanup := s.prepareWorktree(ctx, proj, head)
	defer cleanup()

	// The enrichment builders are independent best-effort I/O (worktree reads,
	// raw-file fetches, test runs, git history), so total latency is the max
	// rather than the sum. Each goroutine writes only its own variable.
	var (
		fileContexts []review.FileContext
		covReport    *coverage.Report
		commits      []review.CommitInfo
		prior        *review.PriorReview
		risk         *review.RiskReport
		wg           sync.WaitGroup
	)
	wg.Go(func() { fileContexts = s.buildFileContexts(ctx, pk, head, files, workDir) })
	wg.Go(func() { covReport = s.buildCoverageReport(ctx, workDir, files) })
	wg.Go(func() { commits = s.buildCommits(ctx, pk, req.MRIID) })
	wg.Go(func() { prior = s.buildPriorReview(ctx, proj, req.MRIID, head) })
	wg.Go(func() { risk = s.buildRiskReport(ctx, proj, files) })
	existing := s.existingFingerprints(ctx, proj.ID, req.MRIID, loaded.Discussions)
	notes := s.buildDiscussionNotes(loaded.Discussions)
	wg.Wait()

	refs := loaded.Refs
	// Force the head: positions, the worktree and the mr_reviews row must never
	// disagree about which commit was reviewed.
	refs.HeadSHA = head

	in := review.ReviewInput{
		ProjectPath:          proj.PathWithNamespace,
		ProjectID:            proj.ID,
		MRIID:                req.MRIID,
		Title:                snap.MR.Title,
		Description:          snap.MR.Description,
		AuthorUsername:       snap.MR.Author.Username,
		ReviewerUsername:     s.cfg.ReviewerUsername,
		SourceBranch:         snap.MR.SourceBranch,
		TargetBranch:         snap.MR.TargetBranch,
		Files:                files,
		Refs:                 refs,
		FileContexts:         fileContexts,
		Commits:              commits,
		Discussions:          notes,
		PriorReview:          prior,
		Risk:                 risk,
		Coverage:             covReport,
		Profile:              s.cfg.Profile,
		ExistingFingerprints: existing,
		PipelineStatus:       pipelineStatus(snap),
		ExistingDiscussions:  len(loaded.Discussions),
		WorkDir:              workDir,
		AgentMode:            agentMode,
		AllowedTools:         s.cfg.AllowedTools,
		Model:                s.cfg.Model,
		Pipeline:             s.cfg.Pipeline,
	}
	// The skeptic pass reads code from the worktree; without one it degrades to
	// the self-reflection prune rather than silently verifying nothing.
	if in.Pipeline.VerifyMode == review.VerifySkeptic && workDir == "" {
		s.log.Infow("no worktree available: downgrading verify_mode skeptic -> reflect")
		in.Pipeline.VerifyMode = review.VerifyReflect
	}

	result, err := s.eng.Review(ctx, in)
	if err != nil {
		return nil, s.recordFailure(ctx, req, proj, inFlight, head, attempt, failReasonLLM, err)
	}
	maskResult(result)

	duration := s.now().Sub(start)
	rev, err := s.persistReview(ctx, persistInput{
		req:      req,
		proj:     proj,
		headSHA:  head,
		refs:     refs,
		attempt:  attempt,
		inFlight: inFlight,
		duration: duration,
		result:   result,
		risk:     risk,
		coverage: covReport,
	}, onPersist)
	if err != nil {
		return nil, s.recordFailure(ctx, req, proj, inFlight, head, attempt, failReasonPersist, err)
	}

	metrics.ObserveReview(req.Team, duration, result.CostUSD)
	s.log.Infow("review complete",
		"project", proj.PathWithNamespace, "iid", req.MRIID, "head_sha", head,
		"status", rev.Status, "findings", rev.FindingsCount, "risk", rev.RiskLevel,
		"cost_usd", result.CostUSD, "duration_ms", duration.Milliseconds())

	return &ReviewOutcome{
		ReviewID:   rev.ID,
		Status:     rev.Status,
		Findings:   int(rev.FindingsCount),
		RiskLevel:  rev.RiskLevel,
		CostUSD:    result.CostUSD,
		DurationMS: duration.Milliseconds(),
	}, nil
}

// reviewOfHead returns the non-failed review already recorded for exactly this
// SHA, or nil when there is none.
func (s *Service) reviewOfHead(ctx context.Context, projectID, iid int64, headSHA string) (*models.MrReview, error) {
	if headSHA == "" {
		return nil, nil
	}
	rev, err := s.st.Review().GetByHeadSHA(ctx, repo_review.GetByHeadSHAParams{
		ProjectID: projectID, MrIid: iid, HeadSha: headSHA,
	})
	switch {
	case err == nil:
		return rev, nil
	case errors.Is(err, pgx.ErrNoRows):
		return nil, nil
	default:
		return nil, fmt.Errorf("lookup review for %d!%d@%s: %w", projectID, iid, headSHA, err)
	}
}

// alreadyReviewed returns headSHA when a non-failed review of exactly this SHA
// already exists — the form domain.NeedsAIReview expects — and "" when none
// does.
func (s *Service) alreadyReviewed(ctx context.Context, projectID, iid int64, headSHA string) (string, error) {
	rev, err := s.reviewOfHead(ctx, projectID, iid, headSHA)
	if err != nil || rev == nil {
		return "", err
	}
	return rev.HeadSha, nil
}

// publishExistingReview is the escape hatch out of the §10.3 dry-run trap, and
// it returns a non-nil outcome only when it took it.
//
// Findings computed while ai_review_publish_enabled was false are persisted with
// note_id IS NULL against a status='dry_run' row. That row satisfies
// GetByHeadSHA, so NeedsAIReview answers up_to_date and the review is skipped —
// including for an explicit `review <ref> --publish`, which then reports
// "skipped: up_to_date" for a request it did not carry out. PublishReview is a
// deliberate no-op for dry runs and the §6.3 sweep only looks at 'reviewed', so
// the findings are unreachable forever: the fingerprint does not depend on the
// head SHA, so no later push frees them either.
//
// Promoting the row and enqueuing its publication in one transaction is the
// path that works. It costs no tokens (the findings are already there), and it
// cannot violate mr_reviews_success_uniq the way a second review of the same SHA
// would, because it is the same row.
func (s *Service) publishExistingReview(ctx context.Context, req ReviewRequest, proj *gitlab.Project, rev *models.MrReview, onPersist OnPersist) *ReviewOutcome {
	if rev == nil || rev.Status != StatusDryRun || !req.Publish || onPersist == nil {
		return nil
	}

	promoted := false
	err := s.st.RunInTx(ctx, func(tx pgx.Tx) error {
		n, err := s.st.Review(store.WithTx(tx)).PromoteDryRun(ctx, rev.ID)
		if err != nil {
			return fmt.Errorf("promote dry run: %w", err)
		}
		if n == 0 {
			// Somebody promoted it first; their transaction owns the enqueue.
			return nil
		}
		promoted = true
		return onPersist(ctx, tx, rev.ID)
	})
	if err != nil {
		s.log.Errorw("promoting the dry run for publication failed",
			"project", proj.PathWithNamespace, "iid", req.MRIID, "review_id", rev.ID, "err", err)
		return nil
	}
	if !promoted {
		return nil
	}

	s.log.Infow("publishing a review that was previously recorded as a dry run",
		"project", proj.PathWithNamespace, "iid", req.MRIID,
		"review_id", rev.ID, "head_sha", rev.HeadSha, "findings", rev.FindingsCount)
	return &ReviewOutcome{
		ReviewID:  rev.ID,
		Status:    StatusReviewed,
		Findings:  int(rev.FindingsCount),
		RiskLevel: rev.RiskLevel,
	}
}

// startAttempt records that this review is about to start spending money.
//
// review.MaxAttempts is 1, so River discards the job on the first failure — and
// an OOM kill, a SIGKILL or a node eviction inside the LLM pass never reaches
// recordFailure, which means FailureStats stays at 0 and scan_repo re-runs the
// same MR at full price every interval. That is exactly the pathological case
// §6.5 exists for (huge diff → huge prompt → OOM) and the one it could not see.
// The row written here is what makes the crash countable; the two paths where
// the process survives take it back or fill it in.
func (s *Service) startAttempt(ctx context.Context, req ReviewRequest, proj *gitlab.Project, headSHA string, attempt int32) (uuid.UUID, error) {
	row, err := s.st.Review().StartAttempt(ctx, repo_review.StartAttemptParams{
		ProjectID:   proj.ID,
		ProjectPath: proj.PathWithNamespace,
		Team:        req.Team,
		MrIid:       req.MRIID,
		HeadSha:     headSHA,
		ErrorText:   inFlightMarker,
		Attempt:     attempt,
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("record review attempt: %w", err)
	}
	return row.ID, nil
}

// maskResult runs the redactor over every model-controlled string that leaves
// this process.
//
// The engine masks the finding body, and only the body. Title, Suggestion and
// the run summary are copied through verbatim, and all three reach GitLab —
// which makes them the exfiltration sink for a prompt injection: an MR's title,
// description, commit messages and diff text are attacker-controlled and are
// interpolated into the prompt, so "put what you read into `suggestion`" is one
// instruction away from a token in a public merge-request comment.
//
// Masking here rather than at each sink is deliberate: this is the single
// boundary where model output enters the service, and it covers persistence,
// publication and logging at once. Anything added to Result later is masked by
// being added to this function, not by remembering four call sites.
func maskResult(r *review.Result) {
	if r == nil {
		return
	}
	r.Summary = security.Mask(r.Summary)
	r.Recommendation = security.Mask(r.Recommendation)
	for i := range r.Findings {
		r.Findings[i].Title = security.Mask(r.Findings[i].Title)
		r.Findings[i].Body = security.Mask(r.Findings[i].Body)
		r.Findings[i].Suggestion = security.Mask(r.Findings[i].Suggestion)
	}
	// Suppressed findings never reach GitLab, but they are stored in
	// pipeline_json, which is the evidence an operator reads.
	for i := range r.Suppressed {
		r.Suppressed[i].Title = security.Mask(r.Suppressed[i].Title)
		r.Suppressed[i].Body = security.Mask(r.Suppressed[i].Body)
		r.Suppressed[i].Reason = security.Mask(r.Suppressed[i].Reason)
	}
}

// existingFingerprints is the dedupe set handed to the engine (§10.3): findings
// that are already hanging in GitLab.
//
// It is the union of two sources, and the union is the point. The database
// knows what we published; the MR's own discussions know what is actually
// there, which survives losing or rebuilding the database.
//
// ListPublishedFingerprints filters on note_id IS NOT NULL, and that condition
// is load-bearing: dedupe must mean "this finding is already posted", not "we
// once computed it". Without it every dry run writes findings with a NULL
// note_id, and switching publication on would make the engine treat them as
// duplicates forever — the fingerprint does not depend on the head SHA, so no
// future push could clear them.
func (s *Service) existingFingerprints(ctx context.Context, projectID, iid int64, discussions []gitlab.Discussion) map[string]bool {
	out := map[string]bool{}
	published, err := s.st.Finding().ListPublishedFingerprints(ctx, projectID, iid)
	if err != nil {
		// Degrading here costs a duplicate comment at worst; failing the review
		// costs the whole run.
		s.log.Warnw("published fingerprints unavailable; dedupe falls back to markers only", "err", err)
	}
	for _, fp := range published {
		if fp = normalizeFingerprint(fp); fp != "" {
			out[fp] = true
		}
	}
	for fp := range gitlab.ScanDiscussions(discussions).Fingerprints {
		if fp = normalizeFingerprint(fp); fp != "" {
			out[fp] = true
		}
	}
	return out
}

func normalizeFingerprint(fp string) string {
	return strings.ToLower(strings.TrimSpace(fp))
}

// pipelineStatus is the head pipeline's status as a prompt hint, and only when
// GitLab actually reported one.
func pipelineStatus(snap domain.MergeRequestSnapshot) string {
	if !snap.Pipeline.Known {
		return ""
	}
	return snap.Pipeline.Status
}

// persistInput groups what the persistence step needs; the argument list would
// otherwise be a dozen positional values.
type persistInput struct {
	req     ReviewRequest
	proj    *gitlab.Project
	headSHA string
	refs    gitlab.DiffRefs
	attempt int32
	// inFlight is the provisional attempt row startAttempt wrote. It is removed
	// in the same transaction as the real row.
	inFlight uuid.UUID
	duration time.Duration
	result   *review.Result
	risk     *review.RiskReport
	coverage *coverage.Report
}

// persistReview writes the review, its findings and the publication enqueue in
// a single transaction (§10.4).
//
// Publication is enqueued only when the caller asked for it AND supplied a
// callback; either one missing makes this a dry run, which is still persisted —
// that row is what stops the scanner re-reviewing the same SHA every five
// minutes.
func (s *Service) persistReview(ctx context.Context, in persistInput, onPersist OnPersist) (*models.MrReview, error) {
	publish := in.req.Publish && onPersist != nil
	status := StatusDryRun
	if publish {
		status = StatusReviewed
	}

	params := repo_review.CreateParams{
		ProjectID:     in.proj.ID,
		ProjectPath:   in.proj.PathWithNamespace,
		Team:          in.req.Team,
		MrIid:         in.req.MRIID,
		HeadSha:       in.headSHA,
		BaseSha:       in.refs.BaseSHA,
		StartSha:      in.refs.StartSHA,
		Status:        status,
		FindingsCount: int32(len(in.result.Findings)),
		RiskLevel:     in.result.RiskLevel,
		Summary:       in.result.Summary,
		PipelineJson:  pipelineJSON(in.result, in.coverage, s.log),
		RiskJson:      jsonOrEmpty(in.risk, s.log),
		// cost_usd is numeric(12,6), so the column is exact and the conversion
		// happens here, at the last point the value is still a float64. It
		// arrives as one — `claude` reports total_cost_usd as a JSON number and
		// the engine sums it across passes — and NewFromFloat takes the shortest
		// decimal that round-trips that float, so a spend of 0.0123 is stored as
		// 0.0123 rather than the binary approximation of it.
		CostUsd:    decimal.NewFromFloat(in.result.CostUSD),
		DurationMs: in.duration.Milliseconds(),
		Attempt:    in.attempt,
	}

	var rev *models.MrReview
	err := s.st.RunInTx(ctx, func(tx pgx.Tx) error {
		reviews := s.st.Review(store.WithTx(tx))
		// The provisional attempt row goes in the same transaction as its
		// replacement: an observer must never see both (a phantom failure beside
		// a success) or neither (a crash that cost nothing but is invisible to
		// the §6.5 ladder).
		if in.inFlight != uuid.Nil {
			if _, err := reviews.DiscardAttempt(ctx, in.inFlight); err != nil {
				return fmt.Errorf("close review attempt: %w", err)
			}
		}
		created, err := s.st.CreateReview(ctx, params, store.WithTx(tx))
		if err != nil {
			return fmt.Errorf("create review: %w", err)
		}
		for _, f := range in.result.Findings {
			// One Insert per finding, never a batched multi-row VALUES: the
			// query's ON CONFLICT DO UPDATE cannot touch the same row twice in
			// one statement.
			//
			// A fingerprint already recorded for this MR is an expected outcome
			// (dedupe degrades whenever discussions cannot be read) and must
			// never abort the whole review's persistence. The row count says
			// which happened: 1 means the finding is attached to THIS review and
			// awaiting publication — a fresh insert, or a re-attach from an
			// earlier review that never published it — and 0 means it is already
			// in GitLab and bound to the review that put it there.
			attached, err := s.st.Finding(store.WithTx(tx)).Insert(ctx,
				findingParams(created.ID, in.proj.ID, in.req.MRIID, f, s.log))
			if err != nil {
				return fmt.Errorf("insert finding %q: %w", f.Title, err)
			}
			if attached == 0 {
				s.log.Debugw("finding is already published; leaving it with the review that posted it",
					"fingerprint", f.Fingerprint, "title", f.Title)
			}
		}
		if publish {
			if err := onPersist(ctx, tx, created.ID); err != nil {
				return fmt.Errorf("enqueue publication: %w", err)
			}
		}
		// Assigned last so a rolled-back transaction cannot hand the caller a
		// row id that does not exist.
		rev = created
		return nil
	})
	if err != nil {
		return nil, err
	}
	return rev, nil
}

// pipelineProvenance is what mr_reviews.pipeline_json holds: per-pass cost and
// duration, plus the side reports that have no column of their own. They live
// here rather than being dropped because they are the evidence behind the
// summary a human reads.
type pipelineProvenance struct {
	Passes       []review.PassReport        `json:"passes,omitempty"`
	Completeness *review.CompletenessReport `json:"completeness,omitempty"`
	Coverage     *coverage.Report           `json:"coverage,omitempty"`
	Suppressed   []review.SuppressedFinding `json:"suppressed,omitempty"`
}

func pipelineJSON(result *review.Result, cov *coverage.Report, log logger.Logger) dbtypes.JSON {
	return jsonOrEmpty(pipelineProvenance{
		Passes:       result.PassReports,
		Completeness: result.Completeness,
		Coverage:     cov,
		Suppressed:   result.Suppressed,
	}, log)
}

// findingParams maps one validated finding onto its row. The GitLab position is
// stored verbatim so publication never recomputes it: Go owns positions, and a
// second owner recomputing against a moved head is exactly how comments land on
// the wrong line.
func findingParams(reviewID uuid.UUID, projectID, iid int64, f review.ValidatedFinding, log logger.Logger) repo_finding.InsertParams {
	p := repo_finding.InsertParams{
		ReviewID:     reviewID,
		ProjectID:    projectID,
		MrIid:        iid,
		Fingerprint:  normalizeFingerprint(f.Fingerprint),
		Severity:     f.Severity,
		Category:     f.Category,
		FilePath:     f.FilePath,
		Title:        f.Title,
		Body:         findingContent(f),
		PositionJson: dbtypes.EmptyObject(),
		Pass:         f.Pass,
		Verification: f.Verification,
	}
	if f.Position != nil {
		p.PositionJson = jsonOrEmpty(f.Position, log)
	}
	return p
}

// findingContent is the human-readable body of a finding comment: what the
// model wrote, plus its concrete suggestion. The severity/category header and
// the fingerprint marker are rendered at publication time from columns, so the
// stored row stays normalized.
func findingContent(f review.ValidatedFinding) string {
	body := strings.TrimSpace(f.Body)
	suggestion := strings.TrimSpace(f.Suggestion)
	if suggestion == "" {
		return body
	}
	return body + "\n\n**Suggested change:**\n\n" + suggestion
}

// jsonOrEmpty marshals v for a NOT NULL jsonb column. The zero dbtypes.JSON is
// SQL NULL, which those columns reject, so every failure path returns an empty
// object instead.
func jsonOrEmpty(v any, log logger.Logger) dbtypes.JSON {
	raw, err := dbtypes.Marshal(v)
	if err != nil {
		log.Warnw("marshal json column failed; storing an empty object", "err", err)
		return dbtypes.EmptyObject()
	}
	if len(raw) == 0 || string(raw) == "null" {
		return dbtypes.EmptyObject()
	}
	return raw
}

// recordFailure closes out the in-flight attempt row and returns the error to
// propagate.
//
// The row is what the §6.5 backoff counts, so the write has to survive the very
// cancellation that often causes the failure: a review killed by its job timeout
// that recorded nothing would be retried at full price every scan interval
// forever. Hence the detached context.
//
// The one failure that must NOT be counted is our own shutdown. River's
// SoftStopTimeout cancels in-flight work on every rolling deploy, and that
// cancellation reaches here as an ordinary error — so four deploys landing
// during long reviews would drive a perfectly healthy head SHA to the top of the
// ladder and blacklist it until somebody pushes. A cancelled context is the
// discriminator: context.Canceled is somebody stopping us, while a job timeout
// arrives as context.DeadlineExceeded and IS the MR's fault (a diff so large the
// pipeline cannot finish is exactly the pathological case the ladder is for).
func (s *Service) recordFailure(ctx context.Context, req ReviewRequest, proj *gitlab.Project, inFlight uuid.UUID, headSHA string, attempt int32, reason string, cause error) error {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	if errors.Is(ctx.Err(), context.Canceled) {
		metrics.ReviewFailed(req.Team, failReasonCanceled)
		s.log.Warnw("review cancelled by shutdown; not counting it against the merge request",
			"project", proj.PathWithNamespace, "iid", req.MRIID, "head_sha", headSHA,
			"attempt", attempt, "reason", reason, "err", cause)
		if inFlight != uuid.Nil {
			if _, err := s.st.Review().DiscardAttempt(writeCtx, inFlight); err != nil {
				s.log.Errorw("discarding the cancelled review attempt failed; the backoff ladder will count it",
					"review_id", inFlight, "err", err)
			}
		}
		return cause
	}

	metrics.ReviewFailed(req.Team, reason)
	s.log.Errorw("review failed",
		"project", proj.PathWithNamespace, "iid", req.MRIID, "head_sha", headSHA,
		"attempt", attempt, "reason", reason, "err", cause)

	stored := security.Truncate(security.Mask(cause.Error()), maxStoredErrorLen)
	if inFlight != uuid.Nil {
		if err := s.st.Review().FailAttempt(writeCtx, stored, inFlight); err != nil {
			s.log.Errorw("recording the failed review failed; the backoff ladder will count it without a reason", "err", err)
		}
		return cause
	}

	// No in-flight row: the failure happened before the attempt was recorded.
	if _, err := s.st.Review().Create(writeCtx, repo_review.CreateParams{
		ProjectID:    proj.ID,
		ProjectPath:  proj.PathWithNamespace,
		Team:         req.Team,
		MrIid:        req.MRIID,
		HeadSha:      headSHA,
		Status:       StatusFailed,
		PipelineJson: dbtypes.EmptyObject(),
		RiskJson:     dbtypes.EmptyObject(),
		Error:        stored,
		Attempt:      attempt,
	}); err != nil {
		s.log.Errorw("recording the failed review failed; the backoff ladder will not count it", "err", err)
	}
	return cause
}
