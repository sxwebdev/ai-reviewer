package jobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/riverqueue/river"
	"github.com/tkcrm/mx/logger"

	"github.com/sxwebdev/ai-reviewer/internal/scheduler"
)

// DigestWorker builds one team's digest and queues its parts for delivery.
//
// Building and sending are separate jobs on purpose (§6.3): a digest costs
// dozens of GitLab requests plus Slack user matching, a delivery costs one POST.
// Retrying the latter must never re-pay for the former.
type DigestWorker struct {
	river.WorkerDefaults[DigestArgs]
	log      logger.Logger
	svc      *Service
	digester Digester
	schedule scheduler.Daily
}

func (w *DigestWorker) Timeout(*river.Job[DigestArgs]) time.Duration { return digestTimeout }

func (w *DigestWorker) Work(ctx context.Context, job *river.Job[DigestArgs]) error {
	return tracked(job, func() error {
		args := job.Args

		team, ok := findTeam(w.svc.Teams(), args.Team)
		if !ok {
			w.log.Warnw("digest skipped: team is no longer configured",
				"operation", "digest", "team", args.Team, "slot", args.Slot)
			return nil
		}

		// Parsed in the schedule's own location, not the container's: run_date
		// names a Moscow calendar day, and at 23:30 UTC that is tomorrow.
		//
		// Location() rather than the Loc field: it is the accessor the scheduler
		// provides for exactly this, degrading a nil zone to UTC the way Next and
		// SlotAt do. time.ParseInLocation panics on a nil *time.Location, so the
		// field read would turn a degenerate schedule into a worker panic.
		runDate, err := time.ParseInLocation(runDateLayout, args.RunDate, w.schedule.Location())
		if err != nil {
			return fmt.Errorf("parse run_date %q: %w", args.RunDate, err)
		}

		out, err := w.digester.BuildDigest(ctx, team, args.Slot, runDate, args.Attempt)
		if err != nil {
			// A credential or permission failure produces the same result on
			// all three attempts; the next slot will try again with whatever
			// the operator fixed in the meantime.
			return classify(fmt.Errorf("build digest for %s %s %s: %w", team.Name, args.RunDate, args.Slot, err))
		}
		if out == nil {
			w.log.Infow("digest produced nothing",
				"operation", "digest", "team", team.Name, "slot", args.Slot, "run_date", args.RunDate)
			return nil
		}

		fields := []any{
			"operation", "digest", "team", team.Name, "slot", args.Slot,
			"run_date", args.RunDate, "attempt", args.Attempt,
			"run_id", out.RunID, "parts", len(out.Messages),
			"merge_requests", out.MRCount, "linear_issues", out.LinearIssueCount,
			"result", out.Status, "job_id", job.ID,
		}

		// Dry run: the digest is built and persisted in full, every part is a
		// digest_messages row with status='dry_run', and nothing is queued for
		// Slack. Gated on both the config switch and the status the service
		// reported, so neither side alone can leak a message into a channel.
		if !w.svc.cfg.SlackSendEnabled || out.Status == StatusDryRun {
			w.log.Infow("digest built (dry run — nothing sent)", fields...)
			return nil
		}

		var errs []error
		for _, id := range out.Messages {
			if _, err := w.svc.EnqueueSlackSend(ctx, SlackSendArgs{MessageID: id, Team: team.Name}); err != nil {
				// One part failing to enqueue must not cancel the ones that did:
				// a digest missing its second half is still worth having.
				errs = append(errs, err)
			}
		}

		w.log.Infow("digest built", fields...)
		return errors.Join(errs...)
	})
}
