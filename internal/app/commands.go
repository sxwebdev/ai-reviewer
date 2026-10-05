package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/tkcrm/mx/logger"

	"github.com/sxwebdev/ai-reviewer/internal/jobs"
	"github.com/sxwebdev/ai-reviewer/internal/service"
	"github.com/sxwebdev/ai-reviewer/internal/slack"
)

// The answers a command gets inside its acknowledgement — the only response
// that reliably beats Slack's three-second deadline.
const (
	ackTeam = "Building the team digest — it will land in this channel in a moment. :hourglass_flowing_sand:"
	ackMine = "Checking what is waiting on you… :hourglass_flowing_sand:"
	// ackQueued is the answer to a command that collapsed into one already
	// running. Saying "already on it" rather than repeating "building" is what
	// tells a second presser that their press did something.
	ackQueued = "Already working on that one — the answer is on its way. :hourglass_flowing_sand:"
	// ackNoTeam names the fix. A command typed in the wrong channel is the
	// ordinary mistake, and "unknown channel" would leave the caller guessing
	// which channel is the right one.
	ackNoTeam = "This channel is not any team's digest channel, so I do not know whose digest to build. " +
		"Run the command in your team's channel."
	ackFailed = "Could not queue that — the service log has the reason."
)

// commandDispatcher turns a slash command into a River job.
//
// It is the whole of the socket's business logic, and it is deliberately tiny:
// resolve which of the two commands was typed, resolve the team from the
// channel, enqueue, answer. Everything expensive happens in the worker, because
// this runs inside the three seconds Slack gives an acknowledgement — and
// because a command that arrived while this replica was shutting down should be
// a durable job rather than work that died with the process.
type commandDispatcher struct {
	log logger.Logger
	// queue is the insert side of River. The dispatcher never works a job.
	queue *jobs.Queue
	// svc resolves the channel to a team. It is the service's own rule so that
	// a command and the scheduled digest cannot disagree about which team owns
	// which channel.
	svc *service.Service
	// team / mine are the configured command names, lower-cased once here so the
	// comparison below is a lookup rather than a fold per command.
	team string
	mine string
}

// newCommandDispatcher builds the dispatcher for the configured command names.
func newCommandDispatcher(log logger.Logger, queue *jobs.Queue, svc *service.Service, cfg SlackCommandNames) *commandDispatcher {
	return &commandDispatcher{
		log:   log,
		queue: queue,
		svc:   svc,
		team:  normalizeCommand(cfg.Team),
		mine:  normalizeCommand(cfg.Mine),
	}
}

// SlackCommandNames are the two configured command names.
type SlackCommandNames struct {
	Team string
	Mine string
}

// slackCommandNames reads them from the config.
func (a *App) slackCommandNames() SlackCommandNames {
	return SlackCommandNames{
		Team: a.Config.Slack.Commands.Team,
		Mine: a.Config.Slack.Commands.Mine,
	}
}

// normalizeCommand puts a configured or received command name into the one form
// they are compared in: lower-cased, trimmed, with the leading slash.
//
// Slack sends the command exactly as registered, so this is not defensive about
// Slack — it is about the config, where "/All" and "all" are the same intent
// typed two ways, and a mismatch would present as a command that silently does
// nothing.
func normalizeCommand(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return ""
	}
	if !strings.HasPrefix(s, "/") {
		return "/" + s
	}
	return s
}

// Handle answers one slash command. It matches slack.CommandFunc.
func (d *commandDispatcher) Handle(ctx context.Context, cmd slack.SlashCommand) slack.CommandAck {
	name := normalizeCommand(cmd.Command)

	var (
		scope string
		ack   string
	)
	switch name {
	case d.team:
		scope, ack = string(service.ScopeTeam), ackTeam
	case d.mine:
		scope, ack = string(service.ScopeMine), ackMine
	default:
		// Slack only delivers commands this app registered, so this is a
		// configuration drift — the command exists in the workspace under a name
		// the config no longer knows. Said out loud, because from the channel it
		// looks like the service is ignoring people.
		d.log.Warnw("slack command not recognised; check slack.commands",
			"command", cmd.Command, "known", []string{d.team, d.mine})
		return slack.CommandAck{Text: fmt.Sprintf(
			"I do not know %s. This service answers %s and %s.", cmd.Command, d.team, d.mine)}
	}

	team, ok := d.svc.SlackCommandTeam(cmd.ChannelID)
	if !ok {
		return slack.CommandAck{Text: ackNoTeam}
	}

	res, err := d.queue.EnqueueSlackCommand(ctx, jobs.SlackCommandArgs{
		Scope:       scope,
		Team:        team.Name,
		SlackUserID: cmd.UserID,
		ChannelID:   cmd.ChannelID,
		ResponseURL: cmd.ResponseURL,
	})
	if err != nil {
		d.log.Errorw("slack command could not be queued",
			"command", name, "team", team.Name, "user", cmd.UserID, "err", err)
		return slack.CommandAck{Text: ackFailed}
	}
	if res.Deduplicated {
		// The same person asked the same question twice, in the same conversation,
		// while the first answer was still being built. The response URL of *this*
		// invocation is the one that was dropped, so the answer arrives on the
		// first one — which the unique key guarantees is this same channel, and for
		// /my the same person.
		d.log.Debugw("slack command folded into the one already running",
			"command", name, "team", team.Name, "user", cmd.UserID, "job_id", res.ID())
		return slack.CommandAck{Text: ackQueued}
	}

	d.log.Infow("slack command queued",
		"command", name, "scope", scope, "team", team.Name,
		"user", cmd.UserID, "channel", cmd.ChannelID, "job_id", res.ID())
	return slack.CommandAck{Text: ack}
}
