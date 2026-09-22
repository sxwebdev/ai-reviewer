package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/sxwebdev/ai-reviewer/internal/domain"
	"github.com/sxwebdev/ai-reviewer/internal/metrics"
	"github.com/sxwebdev/ai-reviewer/internal/slack"
)

// CommandScope is which digest a Slack command asked for.
type CommandScope string

const (
	// ScopeTeam is the whole team's digest, posted to the channel.
	ScopeTeam CommandScope = "team"
	// ScopeMine is the caller's own rows, shown only to them.
	ScopeMine CommandScope = "mine"
)

// Valid reports whether s is one of the two scopes.
func (s CommandScope) Valid() bool { return s == ScopeTeam || s == ScopeMine }

// SlackCommandRequest is one command to answer.
type SlackCommandRequest struct {
	Team  domain.Team
	Scope CommandScope
	// SlackUserID is the caller. Required for ScopeMine, where it is the filter.
	SlackUserID string
	// ResponseURL is Slack's delayed-response capability for this command. It is
	// never logged and never included in an error.
	ResponseURL string
}

// noticeNothingForYou is the answer to a personal command with no rows.
//
// It names both causes on purpose. "Nothing waiting on you" alone is a lie the
// moment somebody's Slack account is not matched to their GitLab one: the
// service cannot tell the two apart — an unmatched person is simply absent from
// the digest, exactly like a person with an empty queue — and a reviewer who
// reads "you are clear" while three merge requests wait on them is worse off
// than before they asked.
const noticeNothingForYou = "Nothing is waiting on you right now. :tada:\n" +
	"_If that looks wrong, your Slack account may not be matched to your GitLab one — " +
	"an admin can map it with `slack.user_map`._"

// noticeNothingForTeam is the answer to a team command with an empty digest.
const noticeNothingForTeam = "Nothing to report: no merge request is waiting on anybody right now. :tada:"

// noticeTruncated is appended when the digest is longer than one response URL
// can carry.
//
// Slack takes five messages per command; a digest that needs more is not
// silently cut, because "your digest, in full" and "the first five sixths of
// your digest" have to be distinguishable — the whole point of the list is that
// nothing waiting is missing from it.
const noticeTruncated = ":warning: %d more part(s) are not shown — Slack accepts %d messages per command. " +
	"The scheduled digest carries the whole list."

// RunSlackCommand answers one in-chat command end to end: build the digest,
// narrow it if the command was personal, and deliver it to the response URL.
//
// It builds through PreviewDigest, so a command and the scheduled digest can
// never disagree about who owes what, and like a preview it records nothing: a
// command is a question, and answering a question must not consume the slot the
// scheduled run needs or move the per-team gauges that describe the service's own
// view of a team.
func (s *Service) RunSlackCommand(ctx context.Context, req SlackCommandRequest) error {
	if !req.Scope.Valid() {
		return fmt.Errorf("slack command: unknown scope %q", req.Scope)
	}
	if s.slack == nil {
		return errors.New("slack command: no Slack client is configured")
	}

	preview, err := s.PreviewDigest(ctx, req.Team)
	if err != nil {
		// The caller is told, in the channel, rather than left watching nothing
		// happen: from Slack a failed command and a slow one look identical.
		s.respond(ctx, req, slack.NoticeMessage(
			":x: Could not build the digest — every configured source failed. The log has the reason."))
		metrics.SlackCommand(req.Team.Name, string(req.Scope), metrics.ResultError)
		return fmt.Errorf("slack command: build digest for %s: %w", req.Team.Name, err)
	}

	data := preview.Data
	inChannel := req.Scope == ScopeTeam
	if req.Scope == ScopeMine {
		mine, ok := data.OnlyPerson(req.SlackUserID)
		if !ok {
			s.respond(ctx, req, slack.NoticeMessage(noticeNothingForYou))
			metrics.SlackCommand(req.Team.Name, string(req.Scope), metrics.ResultOK)
			return nil
		}
		data = mine
	}

	messages := slack.BuildDigest(data)
	if len(messages) == 0 {
		notice := noticeNothingForTeam
		if req.Scope == ScopeMine {
			notice = noticeNothingForYou
		}
		s.respond(ctx, req, slack.NoticeMessage(notice))
		metrics.SlackCommand(req.Team.Name, string(req.Scope), metrics.ResultOK)
		return nil
	}

	sent, err := s.deliverCommand(ctx, req, messages, inChannel)
	if err != nil {
		metrics.SlackCommand(req.Team.Name, string(req.Scope), metrics.ResultError)
		return err
	}

	s.log.Infow("slack command answered",
		"team", req.Team.Name, "scope", string(req.Scope), "user", req.SlackUserID,
		"parts", sent, "parts_built", len(messages), "merge_requests", preview.MRCount)
	metrics.SlackCommand(req.Team.Name, string(req.Scope), metrics.ResultOK)
	return nil
}

// deliverCommand posts the built parts, within the response URL's budget, and
// reports how many it sent.
func (s *Service) deliverCommand(
	ctx context.Context, req SlackCommandRequest, messages []slack.Message, inChannel bool,
) (int, error) {
	// One of the five responses is reserved when there is more digest than
	// budget, so the truncation notice is itself deliverable. Nothing is reserved
	// when everything fits.
	budget := slack.MaxCommandResponses
	truncated := 0
	if len(messages) > budget {
		budget--
		truncated = len(messages) - budget
	}

	for i, m := range messages {
		if i >= budget {
			break
		}
		if err := s.slack.Respond(ctx, req.ResponseURL, m.Response(inChannel)); err != nil {
			// Every part after a failure would fail the same way — an expired URL
			// does not un-expire — and the ones already posted stay: a partial answer
			// beats deleting what the caller can already read.
			return i, fmt.Errorf("slack command: deliver part %d/%d: %w", m.Part, m.Parts, err)
		}
	}
	if truncated > 0 {
		notice := slack.NoticeMessage(fmt.Sprintf(noticeTruncated, truncated, slack.MaxCommandResponses))
		if err := s.slack.Respond(ctx, req.ResponseURL, notice); err != nil {
			return budget, fmt.Errorf("slack command: deliver the truncation notice: %w", err)
		}
		s.log.Warnw("slack command answer did not fit the response budget",
			"team", req.Team.Name, "scope", string(req.Scope),
			"parts", len(messages), "sent", budget)
	}
	return min(len(messages), budget), nil
}

// respond posts one notice and swallows the failure into a log line.
//
// Used only where the notice *is* the error report: failing to deliver "I could
// not build your digest" is worth a log line and nothing else, since returning it
// would replace a reported failure with an unreported one.
func (s *Service) respond(ctx context.Context, req SlackCommandRequest, msg slack.ResponseMessage) {
	if err := s.slack.Respond(ctx, req.ResponseURL, msg); err != nil {
		s.log.Warnw("slack command notice could not be delivered",
			"team", req.Team.Name, "scope", string(req.Scope), "err", err)
	}
}

// SlackCommandTeam resolves which team a command typed in a channel is about.
//
// The channel is the whole answer: a team owns exactly one channel, so a command
// typed in it can only mean that team. A channel nobody owns — a DM, or any
// other channel the app was invited to — resolves to the single configured team
// when there is only one, because on a one-team deployment there is nothing else
// it could mean; with several it is refused by name rather than answered with
// somebody else's queue.
func (s *Service) SlackCommandTeam(channelID string) (domain.Team, bool) {
	id := strings.TrimSpace(channelID)
	for _, t := range s.cfg.Teams {
		if id != "" && strings.EqualFold(strings.TrimSpace(t.SlackChannel), id) {
			return t, true
		}
	}
	if len(s.cfg.Teams) == 1 {
		return s.cfg.Teams[0], true
	}
	return domain.Team{}, false
}
