package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tkcrm/mx/logger"
	"github.com/urfave/cli/v3"

	"github.com/sxwebdev/ai-reviewer/internal/app"
	"github.com/sxwebdev/ai-reviewer/internal/domain"
	"github.com/sxwebdev/ai-reviewer/internal/jobs"
	"github.com/sxwebdev/ai-reviewer/internal/scheduler"
	"github.com/sxwebdev/ai-reviewer/internal/store"
	"github.com/sxwebdev/ai-reviewer/internal/store/repos/repo_digestrun"
)

func digestCommand(boot logger.ExtendedLogger) *cli.Command {
	return &cli.Command{
		Name:  "digest",
		Usage: "Enqueue a digest run for the current slot, or print one with --dry-run",
		Flags: []cli.Flag{
			teamFlag(),
			&cli.BoolFlag{
				Name:  "force",
				Usage: "Repeat a slot that was already run, as a new attempt",
			},
			&cli.BoolFlag{
				Name:    "dry-run",
				Aliases: []string{"preview"},
				Usage:   "Build the digest in this process and print it: no database rows, no Slack, no slot consumed",
			},
			&cli.BoolFlag{
				Name:  "json",
				Usage: "With --dry-run, print the exact chat.postMessage payloads instead of the rendered text",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			// Refused rather than ignored: --force asks for a second *recorded*
			// attempt at a slot, and a dry run records nothing, so honouring one and
			// silently dropping the other is how somebody concludes they re-ran a
			// digest that never happened.
			if cmd.Bool("dry-run") && cmd.Bool("force") {
				return errors.New("--force is meaningless with --dry-run: a preview files no attempt")
			}
			if cmd.Bool("json") && !cmd.Bool("dry-run") {
				return errors.New("--json only applies to --dry-run")
			}

			a, err := open(ctx, boot, cmd)
			if err != nil {
				return err
			}
			defer func() { _ = a.Close() }()

			teams, err := digestTargets(a, cmd.String("team"))
			if err != nil {
				return err
			}

			now := time.Now()
			if cmd.Bool("dry-run") {
				return previewDigests(ctx, os.Stdout, a, teams, cmd.Bool("json"), now)
			}

			pg, q, err := a.Queue(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = pg.Stop(ctx) }()

			st, err := store.New(pg.Pool)
			if err != nil {
				return err
			}

			for _, team := range teams {
				schedule, err := teamSchedule(team)
				if err != nil {
					return err
				}
				// A manual run on a skipped day is done, not refused: somebody typed
				// the command, and the skip list describes the *schedule*. Saying so
				// is the difference between an operator who meant it and one who
				// forgot today is a holiday.
				if schedule.Skipped(now) {
					fmt.Printf("note: %s is a skipped day for %s (%s) — queuing anyway because you asked for it\n",
						now.In(schedule.Location()).Format("Monday 2006-01-02"), team.Name, schedule.Skip.Describe())
				}
				if err := enqueueDigest(ctx, q, st, team, schedule, now, cmd.Bool("force")); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

// teamSchedule builds one team's resolved digest schedule.
//
// Built per team, from the values the team carries: a manual digest has to land
// on exactly the slot and calendar day the scheduled run would have used, and
// with per-team schedules one shared Daily would name another team's slot —
// which the digest_runs unique index cannot catch, because it is a different
// slot rather than a repeat.
func teamSchedule(team domain.Team) (scheduler.Daily, error) {
	schedule, err := scheduler.NewDigest(scheduler.DigestSpec{
		Slots:        team.DigestSlots,
		Timezone:     team.DigestTimezone,
		SkipWeekdays: team.DigestSkipWeekdays,
		SkipDates:    team.DigestSkipDates,
	})
	if err != nil {
		return scheduler.Daily{}, fmt.Errorf("digest schedule for team %q: %w", team.Name, err)
	}
	return schedule, nil
}

// digestTargets resolves --team, or every configured team.
func digestTargets(a *app.App, name string) ([]domain.Team, error) {
	if name == "" {
		teams := app.Teams(a.Config)
		if len(teams) == 0 {
			return nil, app.ErrNoTeams
		}
		return teams, nil
	}
	team, ok := app.TeamByName(a.Config, name)
	if !ok {
		return nil, fmt.Errorf("no configured team named %q", name)
	}
	return []domain.Team{team}, nil
}

// enqueueDigest queues one team's digest, refusing a repeat of an already-run
// slot unless --force was given.
//
// The refusal is the point (§15). Attempt 0 belongs to the scheduled run, and
// the unique index on (team, run_date, slot, attempt) is what stops two
// replicas double-sending it. Letting the command hit that index would surface
// as a constraint violation — technically correct and completely unreadable.
// So the state is checked first and reported in words, and --force takes the
// next attempt, which produces a new digest_runs row and an honest journal of
// manual repeats.
func enqueueDigest(
	ctx context.Context,
	q *jobs.Queue,
	st *store.Store,
	team domain.Team,
	schedule scheduler.Daily,
	now time.Time,
	force bool,
) error {
	args := jobs.NewDigestArgs(team.Name, schedule, now, 0)
	// Location(), not the Loc field: it is the scheduler's nil-safe accessor,
	// and time.ParseInLocation panics on a nil *time.Location.
	runDate, err := time.ParseInLocation("2006-01-02", args.RunDate, schedule.Location())
	if err != nil {
		return err
	}

	existing, err := st.DigestRun().GetBySlot(ctx, repo_digestrun.GetBySlotParams{
		Team:    team.Name,
		RunDate: runDate,
		Slot:    args.Slot,
	})
	switch {
	case err != nil && !errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("look up the %s digest for %s %s: %w", team.Name, args.RunDate, args.Slot, err)

	case err == nil && !force:
		return cli.Exit(fmt.Sprintf(
			"the %s digest for %s %s has already run (attempt %d, status %s).\n"+
				"Repeat it with --force, which files a new attempt rather than overwriting the record.",
			team.Name, args.RunDate, args.Slot, existing.Attempt, existing.Status), 1)

	case force:
		next, err := st.DigestRun().NextAttempt(ctx, repo_digestrun.NextAttemptParams{
			Team:    team.Name,
			RunDate: runDate,
			Slot:    args.Slot,
		})
		if err != nil {
			return fmt.Errorf("compute the next attempt: %w", err)
		}
		args.Attempt = int(next)
	}

	res, err := q.EnqueueDigest(ctx, args)
	if err != nil {
		return err
	}
	if res.Deduplicated {
		fmt.Printf("%s %s %s (attempt %d): already queued as job %d\n",
			team.Name, args.RunDate, args.Slot, args.Attempt, res.ID())
		return nil
	}
	fmt.Printf("queued digest job %d: %s %s %s (attempt %d)\n",
		res.ID(), team.Name, args.RunDate, args.Slot, args.Attempt)
	return nil
}
