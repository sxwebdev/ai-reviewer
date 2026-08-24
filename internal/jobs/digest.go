package jobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/riverqueue/river"
	"github.com/tkcrm/mx/logger"
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

		// Parsed in this team's own location — looked up rather than held on the
		// worker, because one worker serves every team and per-team schedules can
		// disagree about which zone a slot was named in.
		//
		// Being honest about the stakes: today this cannot change the outcome.
		// args.RunDate carries no time, so parsing it anywhere yields midnight in
		// that zone, and service.truncateDay immediately reduces it to its calendar
		// date. The zone is used because it is the *correct* one by construction and
		// costs nothing — so the day run_date grows a time component, or a caller
		// uses it for something other than a date, this is already right.
		//
		// The lookup's failure is deliberately not fatal and deliberately not a
		// skip: it cannot happen (findTeam already proved the team is configured,
		// and the schedule map is built from the same slice), and dropping a team's
		// digest over an unobservable difference would be a real loss to avoid an
		// imaginary one. A zero Daily's Location() is UTC, which is exactly the
		// fallback that changes nothing.
		schedule, _ := w.svc.ScheduleFor(args.Team)

		// Location() rather than the Loc field: it is the accessor the scheduler
		// provides for exactly this, degrading a nil zone to UTC the way Next and
		// SlotAt do. time.ParseInLocation panics on a nil *time.Location, so the
		// field read would turn a degenerate schedule into a worker panic.
		runDate, err := time.ParseInLocation(runDateLayout, args.RunDate, schedule.Location())
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
			"run_id", out.RunID, "parts", out.Parts,
			"merge_requests", out.MRCount, "linear_issues", out.LinearIssueCount,
			"result", out.Status, "job_id", job.ID,
		}

		// Dry run: the digest is built and persisted in full, every part is a
		// digest_messages row with status='dry_run', and nothing is queued for
		// Slack. Gated on both the config switch and the status the service
		// reported, so neither side alone can leak a message into a channel.
		dryRun := !w.svc.cfg.SlackSendEnabled || out.Status == StatusDryRun

		var errs []error
		if !dryRun {
			for _, id := range out.Messages {
				if _, err := w.svc.EnqueueSlackSend(ctx, SlackSendArgs{MessageID: id, Team: team.Name}); err != nil {
					// One part failing to enqueue must not cancel the ones that did:
					// a digest missing its second half is still worth having.
					errs = append(errs, err)
				}
			}
		}

		msg, extra := digestLogLine(out, dryRun)
		w.log.Infow(msg, append(fields, extra...)...)
		return errors.Join(errs...)
	})
}

// Messages a digest job's summary line can carry.
const (
	msgDigestBuilt  = "digest built"
	msgDigestDryRun = "digest built (dry run — nothing sent)"
	msgDigestReused = "digest slot was already built; nothing reassembled"
)

// digestLogLine names what the pass did and returns the fields only that branch
// can answer.
//
// A function rather than a switch inside Work because the ordering is the whole
// content and it is otherwise testable only by capturing log output. Reused is
// answered FIRST, and that is the bug this replaced: the dry-run return used to
// come before it, so on a deployment with slack_send_enabled off — which is what
// config.example.yaml ships — every pass took that branch, and a run assembled
// hours earlier was announced as a fresh "digest built (dry run)". Whether
// anything was queued for Slack and whether anything was assembled are two
// different questions.
func digestLogLine(out *DigestOutcome, dryRun bool) (string, []any) {
	if out.Reused {
		// built_at is what makes the line an answer rather than a note — at 17:03,
		// "the 14:00 slot you are missing was assembled at 14:45" is the whole
		// question. failed_repos and linear_degraded are deliberately absent:
		// digest_runs does not store them, so printing zeros would report "nothing
		// degraded" for a question this pass never asked.
		return msgDigestReused, []any{"built_at", out.BuiltAt.Format(time.RFC3339)}
	}
	degradations := []any{"failed_repos", out.FailedRepos, "linear_degraded", out.LinearDegraded}
	if dryRun {
		return msgDigestDryRun, degradations
	}
	return msgDigestBuilt, degradations
}
