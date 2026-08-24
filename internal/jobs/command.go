package jobs

import (
	"context"
	"fmt"
	"time"

	"github.com/riverqueue/river"
	"github.com/tkcrm/mx/logger"
)

// SlackCommandWorker answers one in-chat command.
//
// It is on the slack queue with the deliveries rather than on default with the
// digest builds, and that placement is the decision worth stating: a command is
// somebody waiting in a channel, so it must not queue behind a scheduled digest
// that has ten minutes to itself. The queue's single worker is what keeps two
// people hammering /all from starting two full GitLab passes at once.
type SlackCommandWorker struct {
	river.WorkerDefaults[SlackCommandArgs]
	log       logger.Logger
	svc       *Service
	commander Commander
}

func (w *SlackCommandWorker) Timeout(*river.Job[SlackCommandArgs]) time.Duration {
	return slackCommandTimeout
}

func (w *SlackCommandWorker) Work(ctx context.Context, job *river.Job[SlackCommandArgs]) error {
	return tracked(job, func() error {
		args := job.Args

		team, ok := findTeam(w.svc.Teams(), args.Team)
		if !ok {
			// The team was renamed or removed between the command and its turn on
			// the queue. Nothing to answer with, and nothing to retry.
			w.log.Warnw("slack command skipped: team is no longer configured",
				"operation", "slack_command", "team", args.Team, "scope", args.Scope)
			return nil
		}

		if err := w.commander.RunSlackCommand(ctx, CommandRequest{
			Team:        team,
			Scope:       args.Scope,
			SlackUserID: args.SlackUserID,
			ResponseURL: args.ResponseURL,
		}); err != nil {
			return classify(fmt.Errorf("answer %s command for %s: %w", args.Scope, team.Name, err))
		}

		// The response URL is deliberately absent from this line, as from every
		// other: it lets whoever reads it post into the channel as this app.
		w.log.Debugw("slack command answered",
			"operation", "slack_command", "team", team.Name, "scope", args.Scope,
			"user", args.SlackUserID, "channel", args.ChannelID, "job_id", job.ID)
		return nil
	})
}
