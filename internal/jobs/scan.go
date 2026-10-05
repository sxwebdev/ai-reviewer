package jobs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/riverqueue/river"
	"github.com/tkcrm/mx/logger"

	"github.com/sxwebdev/ai-reviewer/internal/domain"
	"github.com/sxwebdev/ai-reviewer/internal/metrics"
)

// ScanWorker is the dispatcher half of the scan (§6.3): it reads the configured
// teams and enqueues one scan_repo per repository. It makes no network calls,
// which is what lets it live inside a 2-minute timeout while the work it fans
// out gets ten.
type ScanWorker struct {
	river.WorkerDefaults[ScanArgs]
	log logger.Logger
	svc *Service
}

func (w *ScanWorker) Timeout(*river.Job[ScanArgs]) time.Duration { return scanTimeout }

func (w *ScanWorker) Work(ctx context.Context, job *river.Job[ScanArgs]) error {
	return tracked(job, func() error {
		teams := w.svc.Teams()
		filter := strings.TrimSpace(job.Args.Team)

		var (
			errs      []error
			queued    int
			deduped   int
			addressed int
		)
		for _, team := range teams {
			if filter != "" && !strings.EqualFold(filter, team.Name) {
				continue
			}
			addressed++
			for _, repo := range team.Repositories {
				res, err := w.svc.EnqueueScanRepo(ctx, ScanRepoArgs{Team: team.Name, Repository: repo})
				if err != nil {
					// Keep going: one repository's insert failing is no reason to
					// leave the other 39 unscanned. The job still fails so River
					// retries, and the inserts that did land are idempotent.
					errs = append(errs, fmt.Errorf("%s/%s: %w", team.Name, repo, err))
					continue
				}
				if res.Deduplicated {
					deduped++
					continue
				}
				queued++
			}
		}

		if filter != "" && addressed == 0 {
			// A hard error rather than a silent success: the operator asked for a
			// team that no longer exists in the config.
			return fmt.Errorf("no configured team named %q", filter)
		}

		w.log.Infow("scan dispatched",
			"operation", "scan", "teams", addressed,
			"queued", queued, "already_running", deduped, "trigger", triggerOf(job.Args.Team))

		return errors.Join(errs...)
	})
}

// triggerOf labels the log line: a team-scoped scan only ever comes from the
// CLI, an unscoped one from the periodic schedule (or from `scan` with no flag).
func triggerOf(team string) string {
	if team != "" {
		return "cli"
	}
	return "schedule"
}

// ScanRepoWorker inspects one repository and enqueues what that pass found.
// Every GitLab call of a scan happens here.
type ScanRepoWorker struct {
	river.WorkerDefaults[ScanRepoArgs]
	log     logger.Logger
	svc     *Service
	scanner Scanner
}

func (w *ScanRepoWorker) Timeout(*river.Job[ScanRepoArgs]) time.Duration { return scanRepoTimeout }

func (w *ScanRepoWorker) Work(ctx context.Context, job *river.Job[ScanRepoArgs]) error {
	return tracked(job, func() error {
		args := job.Args

		team, ok := findTeam(w.svc.Teams(), args.Team)
		if !ok {
			// The team was removed from the config between the dispatch and now.
			// Retrying cannot fix that, so the job succeeds having done nothing.
			w.log.Warnw("scan_repo skipped: team is no longer configured",
				"operation", "scan_repo", "team", args.Team, "repository", args.Repository)
			return nil
		}

		start := time.Now()
		res, err := w.scanner.ScanRepository(ctx, team, args.Repository)
		if err != nil {
			// classify: a revoked token or a repository that no longer exists
			// fails identically on all three attempts, and the periodic scan
			// then repeats that for every repository, every interval.
			return classify(fmt.Errorf("scan %s: %w", args.Repository, err))
		}

		// A repository the service could only partially inspect is a partial
		// result, not a failure: the digest says so and the rest of the pass
		// stands. See §17 "Partial failures".
		//
		// ai_reviewer_scans_total / _duration_seconds are deliberately NOT
		// emitted here. ScanRepository owns them — a repository is the unit that
		// does the scanning work, and this worker is a wrapper around exactly one
		// such call. Recording them on both sides doubled every sample: the rate
		// panels and every alert threshold built on them read 2× the truth. The
		// label below is only for the log line.
		outcome := metrics.ResultOK
		if res.Failed {
			outcome = metrics.ResultPartial
		}

		var errs []error

		// Reviews. Publish stays nil in the args so the decision is taken from
		// config when the job actually runs, not when it was queued — and so a
		// later `--publish` insert for the same SHA is recognisably different.
		var queued, alreadyQueued int
		for _, c := range res.Candidates {
			ins, err := w.svc.EnqueueReview(ctx, ReviewArgs{
				Team:        c.Team,
				ProjectPath: c.ProjectPath,
				ProjectID:   c.ProjectID,
				MRIID:       c.MRIID,
				HeadSHA:     c.HeadSHA,
			})
			if err != nil {
				errs = append(errs, err)
				continue
			}
			// Counted separately, because "queued 0" alone is ambiguous in exactly
			// the situation an operator most needs to understand: 25 candidates and
			// nothing enqueued reads as "nothing to do" when it actually means "all
			// 25 are still sitting in the queue behind two workers".
			if ins.Deduplicated {
				alreadyQueued++
			} else {
				queued++
			}
		}

		// §6.3's safety net: reviews that were persisted but never delivered —
		// publication exhausted its attempts, the job was removed by hand, the
		// database was restored from a backup. Only the cheap half is repeated.
		var republished int
		for _, id := range res.StalePublish {
			ins, err := w.svc.EnqueuePublishReview(ctx, PublishReviewArgs{ReviewID: id})
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if !ins.Deduplicated {
				republished++
			}
		}

		fields := []any{
			"operation", "scan_repo", "team", team.Name, "repository", args.Repository,
			// Both numbers, because they answer different questions: open is every
			// merge request the listing returned, inspected only those §9.1's cheap
			// filter thought worth a detail call. "inspected 0" alone reads as an
			// empty repository rather than one whose merge requests have not moved.
			"merge_requests_open", res.Open,
			"merge_requests_inspected", len(res.Snapshots), "reviews_queued", queued,
			"reviews_already_queued", alreadyQueued,
			"publications_requeued", republished, "result", outcome,
			"duration", time.Since(start).String(),
		}

		switch {
		case !res.ReviewDisabled:
			w.log.Infow("repository scanned", fields...)
		case republished > 0 || res.Failed:
			// The sweep still runs with review off, and a publication it rescued is
			// worth a line whatever the switch says.
			w.log.Infow("repository not scanned: ai_review is off for this team", fields...)
		default:
			// Debug: nine repositories every five minutes is 2600 lines a day
			// saying nothing happened, and the reason it did not is already in the
			// "effective mode" line at startup and in `doctor`. "scan dispatched"
			// still reports the pass at info.
			w.log.Debugw("repository not scanned: ai_review is off for this team", fields...)
		}

		return errors.Join(errs...)
	})
}

// findTeam looks a team up by name, case-insensitively — config validation
// already guarantees names are unique under that comparison.
func findTeam(teams []domain.Team, name string) (domain.Team, bool) {
	for _, t := range teams {
		if strings.EqualFold(t.Name, name) {
			return t, true
		}
	}
	return domain.Team{}, false
}
