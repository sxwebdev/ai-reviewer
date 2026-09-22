package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/sxwebdev/ai-reviewer/internal/domain"
	"github.com/sxwebdev/ai-reviewer/internal/jobs"
	"github.com/sxwebdev/ai-reviewer/internal/service"
)

// jobsAdapter bridges *service.Service to the three interfaces internal/jobs
// declares for itself (jobs.Reviewer, jobs.Scanner, jobs.Digester).
//
// The two packages describe the same operations with their own request and
// result types on purpose: internal/service must not import the queue (that
// would drag River into every service test), and internal/jobs declares the
// interfaces it consumes rather than importing the implementation. The
// translation is mechanical and lives here, in the composition root, which is
// the one place that legitimately knows about both.
type jobsAdapter struct{ svc *service.Service }

var (
	_ jobs.Reviewer  = jobsAdapter{}
	_ jobs.Scanner   = jobsAdapter{}
	_ jobs.Digester  = jobsAdapter{}
	_ jobs.Commander = jobsAdapter{}
)

func (a jobsAdapter) RunReview(ctx context.Context, req jobs.ReviewRequest, onPersist jobs.OnPersist) (*jobs.ReviewOutcome, error) {
	out, err := a.svc.RunReview(ctx, toServiceRequest(req), toServicePersist(onPersist))
	if err != nil {
		return nil, err
	}
	return fromServiceOutcome(out), nil
}

// toServicePersist converts the publication hand-off.
//
// A nil hand-off must stay nil: the service reads it as "this is a dry run, do
// not enqueue publication". Converting a nil func value does preserve nil, but
// the branch is spelled out because the tempting refactor here — wrapping the
// callback in a closure for logging — would make every review look publishable
// and quietly defeat the dry-run switch.
func toServicePersist(onPersist jobs.OnPersist) service.OnPersist {
	if onPersist == nil {
		return nil
	}
	return service.OnPersist(onPersist)
}

func toServiceRequest(req jobs.ReviewRequest) service.ReviewRequest {
	return service.ReviewRequest{
		Team:        req.Team,
		ProjectPath: req.ProjectPath,
		ProjectID:   req.ProjectID,
		MRIID:       req.MRIID,
		HeadSHA:     req.HeadSHA,
		Publish:     req.Publish,
	}
}

func fromServiceOutcome(out *service.ReviewOutcome) *jobs.ReviewOutcome {
	if out == nil {
		return nil
	}
	return &jobs.ReviewOutcome{
		ReviewID:   out.ReviewID,
		Status:     out.Status,
		Findings:   out.Findings,
		RiskLevel:  out.RiskLevel,
		CostUSD:    out.CostUSD,
		DurationMS: out.DurationMS,
		SkipReason: out.SkipReason,
	}
}

func (a jobsAdapter) PublishReview(ctx context.Context, reviewID uuid.UUID) (int, error) {
	return a.svc.PublishReview(ctx, reviewID)
}

func (a jobsAdapter) ScanRepository(ctx context.Context, team domain.Team, repository string) (*jobs.ScanResult, error) {
	res, err := a.svc.ScanRepository(ctx, team, repository)
	if err != nil {
		return nil, err
	}
	return fromServiceScan(res), nil
}

func fromServiceScan(res *service.ScanResult) *jobs.ScanResult {
	if res == nil {
		return nil
	}
	out := &jobs.ScanResult{
		StalePublish:   res.StalePublish,
		Snapshots:      res.Snapshots,
		Failed:         res.Failed,
		Open:           res.Open,
		ReviewDisabled: res.ReviewDisabled,
	}
	out.Candidates = make([]jobs.ReviewRequest, 0, len(res.Candidates))
	for _, c := range res.Candidates {
		out.Candidates = append(out.Candidates, jobs.ReviewRequest{
			Team:        c.Team,
			ProjectPath: c.ProjectPath,
			ProjectID:   c.ProjectID,
			MRIID:       c.MRIID,
			HeadSHA:     c.HeadSHA,
			// Publish is deliberately not carried over: the scanner does not
			// decide publication. The jobs layer resolves it when the review job
			// runs, from that job's own args and the config — which is what lets
			// a later `--publish` insert be told apart from this one.
		})
	}
	return out
}

func (a jobsAdapter) BuildDigest(ctx context.Context, team domain.Team, slot string, runDate time.Time, attempt int) (*jobs.DigestOutcome, error) {
	out, err := a.svc.BuildDigest(ctx, team, slot, runDate, attempt)
	if err != nil {
		return nil, err
	}
	return fromServiceDigest(out), nil
}

func fromServiceDigest(out *service.DigestOutcome) *jobs.DigestOutcome {
	if out == nil {
		return nil
	}
	return &jobs.DigestOutcome{
		RunID:            out.RunID,
		Messages:         out.Messages,
		Status:           out.Status,
		MRCount:          out.MRCount,
		LinearIssueCount: out.LinearIssueCount,
		Reused:           out.Reused,
		BuiltAt:          out.BuiltAt,
		FailedRepos:      out.FailedRepos,
		LinearDegraded:   out.LinearDegraded,
		Parts:            out.Parts,
	}
}

func (a jobsAdapter) SendMessage(ctx context.Context, messageID uuid.UUID) (jobs.SendOutcome, error) {
	out, err := a.svc.SendMessage(ctx, messageID)
	return jobs.SendOutcome{
		Delivered: out.Delivered, Status: out.Status,
		Part: out.Part, Parts: out.Parts, TS: out.TS,
	}, err
}

// RunSlackCommand carries a command across the seam. The scope is re-typed
// rather than passed through, and the service rejects one it does not know: the
// value comes from a durable job argument, so "team" and "mine" are a wire
// format, not an enum shared between the two packages.
func (a jobsAdapter) RunSlackCommand(ctx context.Context, req jobs.CommandRequest) error {
	return a.svc.RunSlackCommand(ctx, service.SlackCommandRequest{
		Team:        req.Team,
		Scope:       service.CommandScope(req.Scope),
		SlackUserID: req.SlackUserID,
		ResponseURL: req.ResponseURL,
	})
}
