package jobs

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sxwebdev/ai-reviewer/internal/domain"
)

// This file is the consumer side of the internal/service ⇄ internal/jobs seam.
// The interfaces are declared here, where they are used, and internal/service
// provides concrete types that satisfy them. That keeps the queue out of the
// service's tests and the service's GitLab/Slack/LLM plumbing out of the
// queue's.
//
// The split of responsibility, restated so it is not re-litigated at a call
// site: the service owns every external call, the domain mapping, running the
// engine, persistence, the §6.5 backoff decision and the §6.4 claim protocol.
// This package owns queues, uniqueness, attempt budgets, timeouts, schedules
// and the decision of what the effective Publish flag is.

// OnPersist runs INSIDE the transaction that writes mr_reviews and mr_findings.
//
// It exists so the publication job can be inserted in that same transaction
// (§10.4) without internal/service importing River: the service owns the
// transaction and hands the raw pgx.Tx back, this package's closure calls
// river's InsertTx on it. A rollback then takes the job with it, and a commit
// guarantees somebody will publish — "review recorded but nobody will publish
// it" is not representable.
//
// A nil OnPersist means "do not enqueue publication": the dry-run path.
type OnPersist func(ctx context.Context, tx pgx.Tx, reviewID uuid.UUID) error

// ReviewRequest is one merge request to review.
type ReviewRequest struct {
	Team        string
	ProjectPath string // GitLab full path, e.g. "backend/payments"
	ProjectID   int64  // 0 when only the path is known; the service resolves it
	MRIID       int64
	HeadSHA     string // "" → the service resolves the current head from GitLab
	// Publish is the EFFECTIVE decision, already resolved from job args and
	// config by this package. The service does not consult config for it.
	Publish bool
}

// ReviewOutcome is what one review produced.
type ReviewOutcome struct {
	ReviewID   uuid.UUID
	Status     string // reviewed | dry_run | failed
	Findings   int
	RiskLevel  string
	CostUSD    float64
	DurationMS int64
	// SkipReason is non-empty when the MR did not need a review at all — the
	// job is a success, not a failure.
	SkipReason domain.Reason
}

// ScanResult is one repository's pass.
type ScanResult struct {
	// Candidates are the MRs needing an AI review, with the §6.5 poison-MR
	// backoff already applied by the service.
	Candidates []ReviewRequest
	// StalePublish are reviews stuck in status='reviewed' for more than 15
	// minutes — §6.3's second line of defence against a lost publication.
	StalePublish []uuid.UUID
	// Snapshots are the MRs the scan actually inspected in full — not every open
	// MR. §9.1's cheap filter runs on the list payload first, so an MR whose head
	// has not moved never gets a detail call and never appears here. The honest
	// "how many were seen" number is ai_reviewer_merge_requests_scanned.
	Snapshots []domain.MergeRequestSnapshot
	// Failed marks a repository that could not be fully inspected. It is a
	// normal partial result, not an error: the digest reports "⚠️ Partial data".
	Failed bool
	// Open is every merge request the listing returned, classified or not — the
	// honest "how many were seen". Zero and meaningless when ReviewDisabled.
	Open int
	// ReviewDisabled marks a pass that never listed the repository's merge
	// requests because the team's ai_review switch is off. The worker's summary
	// line reads on it: "inspected 0" without it says the repository is empty.
	ReviewDisabled bool
}

// DigestOutcome is one built (not yet delivered) digest.
type DigestOutcome struct {
	RunID uuid.UUID
	// Messages is one id per digest_messages row, in delivery order. Each
	// becomes one slack_send job.
	Messages         []uuid.UUID
	Status           string // built | dry_run | partial | failed
	MRCount          int
	LinearIssueCount int
	// Reused marks a run the service found rather than built — a retried job, or
	// RunOnStart landing on a slot that already went out. The worker's line reads
	// on it; "digest built" for a digest assembled three hours ago is a lie the
	// wrapper cannot otherwise avoid telling.
	Reused bool
	// BuiltAt is when a reused run was assembled; the zero time on a fresh build,
	// whose own log line is the timestamp.
	BuiltAt time.Time
	// FailedRepos and LinearDegraded name the degradations behind a 'partial'
	// status, which the status alone does not. Meaningful only when Reused is
	// false — digest_runs does not store them, so a reused run cannot answer, and
	// the worker omits the fields there rather than logging zeros.
	FailedRepos    int
	LinearDegraded bool
	// Parts is the run's message count, taken from the row: Messages is empty on
	// a dry run, so counting it reported "parts 0" for a digest that has them.
	Parts int
}

// StatusDryRun is the DigestOutcome/ReviewOutcome status that means "everything
// was computed and persisted, nothing was delivered".
const StatusDryRun = "dry_run"

// Reviewer runs and publishes reviews.
type Reviewer interface {
	// RunReview reviews one merge request and persists the result. onPersist,
	// when non-nil, runs inside the persistence transaction.
	RunReview(ctx context.Context, req ReviewRequest, onPersist OnPersist) (*ReviewOutcome, error)
	// PublishReview posts the review's unpublished findings and its summary
	// marker, and reports how many notes it created. It is idempotent: only
	// findings with note_id IS NULL are posted, and each is marked immediately
	// after its POST, so a retry after a mid-way failure completes the delivery
	// without duplicating anything.
	PublishReview(ctx context.Context, reviewID uuid.UUID) (published int, err error)
}

// Scanner inspects one repository per call, matching the scan_repo job.
type Scanner interface {
	ScanRepository(ctx context.Context, team domain.Team, repository string) (*ScanResult, error)
}

// Digester builds digests and delivers their parts.
type Digester interface {
	// BuildDigest persists digest_runs + digest_messages and returns the message
	// ids. It never calls chat.postMessage.
	BuildDigest(ctx context.Context, team domain.Team, slot string, runDate time.Time, attempt int) (*DigestOutcome, error)
	// SendMessage implements the §6.4 claim protocol end to end: claim, branch on
	// the previous status, POST, record. It returns nil for the no-op branches
	// (already sent, failed, dry-run), because those are successes — which is
	// exactly why it also returns an outcome: a nil error alone cannot tell the
	// worker whether anything reached Slack.
	SendMessage(ctx context.Context, messageID uuid.UUID) (SendOutcome, error)
}

// SendOutcome is what one SendMessage call did.
type SendOutcome struct {
	// Delivered is true only when this call posted to Slack.
	Delivered bool
	// Status is the row's status before the claim, which is what names the no-op
	// branch the call took.
	Status string
	Part   int
	Parts  int
	// TS is Slack's message timestamp, set only when Delivered.
	TS string
}

// CommandRequest is one in-chat command to answer.
//
// Scope is a plain string across this seam rather than the service's own typed
// constant, for the same reason the job args carry one: it arrives from a
// durable row, and this package must be able to hand across whatever was
// written there — including a value a future version stops recognising, which is
// the service's judgement to make and report, not the queue's to silently drop.
type CommandRequest struct {
	Team        domain.Team
	Scope       string // team | mine
	SlackUserID string
	// ResponseURL is Slack's delayed-response capability for this command.
	ResponseURL string
}

// Commander answers in-chat commands. Optional: a deployment without a Slack
// app-level token never receives one, and the worker is not registered.
type Commander interface {
	// RunSlackCommand builds the requested digest and delivers it to the
	// response URL. It reports an error only when the caller was left without an
	// answer worth retrying for — a digest with nothing in it is a success.
	RunSlackCommand(ctx context.Context, req CommandRequest) error
}
