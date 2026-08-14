package jobs

import (
	"context"
	"fmt"
	"time"

	"github.com/riverqueue/river"
	"github.com/tkcrm/mx/logger"

	"github.com/sxwebdev/ai-reviewer/internal/domain"
)

// ReviewWorker runs one review. MaxAttempts is 1 (see reviewMaxAttempts): a
// failure here is recorded in mr_reviews and the §6.5 backoff, applied by the
// scanner, decides whether it is ever tried again.
type ReviewWorker struct {
	river.WorkerDefaults[ReviewArgs]
	log      logger.Logger
	svc      *Service
	reviewer Reviewer
}

// reviewLockSnooze is how long a review waits when another run of the same head
// SHA holds the advisory lock. Comfortably above River's 5s scheduler interval
// so the job lands in `scheduled` rather than being made available immediately
// and spinning, and far below the 30m a review can take: the holder is usually
// a `--local` debugging run, and coming back a minute later costs nothing.
const reviewLockSnooze = time.Minute

func (w *ReviewWorker) Timeout(*river.Job[ReviewArgs]) time.Duration { return reviewTimeout }

func (w *ReviewWorker) Work(ctx context.Context, job *river.Job[ReviewArgs]) error {
	return tracked(job, func() error {
		args := job.Args

		// Under the same advisory lock `--local` takes. River's uniqueness
		// excludes a second *queued* review of this SHA; it knows nothing about a
		// CLI process running one in-process, and the two together are what §15
		// promises exclusion against.
		out, ok, err := w.svc.lockedReview(ctx, args, w.reviewer)
		if err != nil {
			return fmt.Errorf("review %s!%d @%s: %w",
				args.ProjectPath, args.MRIID, shortSHA(args.HeadSHA), err)
		}
		if !ok {
			// Snooze rather than fail: review runs with MaxAttempts = 1, so an
			// error here would discard the job outright and this SHA would wait
			// for the next scan pass — or, once the §6.5 ladder counted the
			// failed row, longer. A snooze does not consume the attempt
			// (verified in river@v0.40.0: the executor writes Attempt-1), so the
			// job simply comes back when the holder is done, finds the review
			// recorded, and skips as up_to_date.
			w.log.Infow("review deferred: this head SHA is being reviewed elsewhere",
				"operation", "review", "team", args.Team, "repository", args.ProjectPath,
				"mr_iid", args.MRIID, "head_sha", shortSHA(args.HeadSHA),
				"job_id", job.ID, "retry_in", reviewLockSnooze.String())
			return river.JobSnooze(reviewLockSnooze)
		}

		fields := []any{
			"operation", "review", "team", args.Team,
			"project_id", args.ProjectID, "repository", args.ProjectPath,
			"mr_iid", args.MRIID, "head_sha", shortSHA(args.HeadSHA),
			"job_id", job.ID,
		}
		if out == nil {
			// A nil outcome with a nil error is the service saying "nothing to
			// do"; treat it as a success and say so rather than dereferencing.
			w.log.Infow("review produced no outcome", fields...)
			return nil
		}
		if out.SkipReason != "" {
			// A skip is a completed job, not a failure: nothing was persisted, so
			// the §6.5 ladder counts no strike against this SHA. head_moved is the
			// one that could read as a lost review, so it says what happens next —
			// the scanner enqueues the head that is live now, under the key that
			// actually matches it.
			fields = append(fields, "result", "skipped", "reason", out.SkipReason.String())
			if out.SkipReason == domain.ReasonHeadMoved {
				fields = append(fields, "next", "the current head is queued by the next scan pass")
			}
			w.log.Infow("review skipped", fields...)
			return nil
		}

		w.log.Infow("review finished", append(fields,
			"result", out.Status, "review_id", out.ReviewID, "findings", out.Findings,
			"risk", out.RiskLevel, "duration", time.Duration(out.DurationMS)*time.Millisecond,
		)...)
		return nil
	})
}

// PublishReviewWorker delivers a persisted review to GitLab. Ten attempts,
// because every failure it can suffer is a network failure and the expensive
// half of the work is already done and durable.
type PublishReviewWorker struct {
	river.WorkerDefaults[PublishReviewArgs]
	log      logger.Logger
	reviewer Reviewer
}

func (w *PublishReviewWorker) Timeout(*river.Job[PublishReviewArgs]) time.Duration {
	return publishTimeout
}

// NextRetry backs off quadratically, capped, instead of River's default
// schedule: publication competes with the rest of the service for GitLab's rate
// limit, and the failure it retries most often (429/5xx) is exactly the one a
// tight retry loop makes worse.
func (w *PublishReviewWorker) NextRetry(job *river.Job[PublishReviewArgs]) time.Time {
	return time.Now().Add(cappedBackoff(job.Attempt, 10*time.Minute))
}

func (w *PublishReviewWorker) Work(ctx context.Context, job *river.Job[PublishReviewArgs]) error {
	return tracked(job, func() error {
		published, err := w.reviewer.PublishReview(ctx, job.Args.ReviewID)
		if err != nil {
			// Ten attempts exist for transient GitLab failures. A deleted merge
			// request or a revoked scope is not one: it would fail ten times
			// over ~6 minutes and then be re-driven by every stale-publication
			// sweep after that.
			return classify(fmt.Errorf("publish review %s: %w", job.Args.ReviewID, err))
		}
		w.log.Infow("review published",
			"operation", "publish_review", "review_id", job.Args.ReviewID,
			"notes", published, "job_id", job.ID, "attempt", job.Attempt)
		return nil
	})
}

// SlackSendWorker delivers exactly one digest part.
//
// Rate limiting is handled twice, at different scales: internal/slack honours
// Retry-After inside a single attempt (bounded by MaxRetryAfter), and this
// backoff spaces the job's own attempts when that budget still ran out.
type SlackSendWorker struct {
	river.WorkerDefaults[SlackSendArgs]
	log      logger.Logger
	digester Digester
}

func (w *SlackSendWorker) Timeout(*river.Job[SlackSendArgs]) time.Duration { return slackSendTimeout }

func (w *SlackSendWorker) NextRetry(job *river.Job[SlackSendArgs]) time.Time {
	return time.Now().Add(cappedBackoff(job.Attempt, 5*time.Minute))
}

func (w *SlackSendWorker) Work(ctx context.Context, job *river.Job[SlackSendArgs]) error {
	return tracked(job, func() error {
		// The service owns the whole §6.4 claim protocol, including the no-op
		// branches: a message already 'sent', 'failed' or 'dry_run' returns nil,
		// so this job is idempotent without knowing why.
		if err := w.digester.SendMessage(ctx, job.Args.MessageID); err != nil {
			// The service has already marked the row failed for a bad token,
			// scope or channel; the remaining attempts would only re-claim a
			// terminal row. Rate limits and Slack's own outages stay retryable.
			return classify(fmt.Errorf("send digest message %s: %w", job.Args.MessageID, err))
		}
		w.log.Infow("digest part delivered",
			"operation", "slack_send", "team", job.Args.Team,
			"message_id", job.Args.MessageID, "job_id", job.ID)
		return nil
	})
}

// cappedBackoff is attempt² seconds, capped. Attempt is River's 1-based number.
func cappedBackoff(attempt int, limit time.Duration) time.Duration {
	d := time.Duration(attempt*attempt) * time.Second
	if d <= 0 || d > limit {
		return limit
	}
	return d
}

// shortSHA is the 8-character head SHA the plan's structured logs use (§14.2).
func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
