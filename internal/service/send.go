package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/sxwebdev/ai-reviewer/internal/metrics"
	"github.com/sxwebdev/ai-reviewer/internal/models"
	"github.com/sxwebdev/ai-reviewer/internal/security"
	"github.com/sxwebdev/ai-reviewer/internal/slack"
)

// SendMessage delivers one persisted digest part.
//
// chat.postMessage has no idempotency key, so the window "the POST went through
// but the worker died before recording it" is closed by state in the database
// (§6.4): claim the row, branch on the status it had *before* the claim, POST,
// record.
//
//	sent            → no-op, already delivered
//	failed, dry_run → no-op, nothing to resend
//	pending         → ordinary first delivery
//	sending         → a previous attempt died between the POST and the result
//	                  write. The message is sent AGAIN, on purpose: a duplicate
//	                  in the channel is noise, a lost digest is a team that
//	                  never learns something is waiting for them. The warning
//	                  and slack_resend_uncertain_total make the duplicate
//	                  visible.
//
// The outcome says which of those happened, because "returned nil" cannot: the
// no-op branches are successes too, and a caller that logs delivery on err ==
// nil reports one for every one of them. SlackSendWorker used to, one line below
// the service's own "needs no delivery" — two lines a millisecond apart, the
// second contradicting the first.
func (s *Service) SendMessage(ctx context.Context, messageID uuid.UUID) (SendOutcome, error) {
	msg, err := s.st.DigestMessage().GetByID(ctx, messageID)
	if err != nil {
		return SendOutcome{}, fmt.Errorf("load digest message %s: %w", messageID, err)
	}
	run, err := s.st.DigestRun().GetByID(ctx, msg.DigestRunID)
	if err != nil {
		return SendOutcome{}, fmt.Errorf("load digest run %s: %w", msg.DigestRunID, err)
	}
	team := run.Team

	// Checked BEFORE the claim: a missing Slack client is a precondition no
	// retry can satisfy, and claiming first would flip the row to 'sending',
	// send every subsequent attempt down the resend branch, and leave the row
	// stuck at 'sending' once the attempts ran out — with
	// slack_resend_uncertain_total reporting a duplicate-delivery risk that
	// never happened. Recording the terminal failure is what the equally
	// unsatisfiable postRequest error below does too.
	if s.slack == nil {
		err := errors.New("service: slack client is not configured")
		s.markSendFailed(ctx, messageID, err)
		metrics.SlackSendError(team, metrics.SlackErrAPI)
		s.settleRun(ctx, msg.DigestRunID, team)
		return SendOutcome{}, err
	}

	claim, err := s.st.DigestMessage().ClaimForSend(ctx, messageID)
	if err != nil {
		return SendOutcome{}, fmt.Errorf("claim digest message %s: %w", messageID, err)
	}
	if !claim.Claimed {
		// Every status outside ('pending','sending') means the message is done
		// with, one way or another. The CHECK constraint on the column is what
		// makes this branch total. Reported, not logged: the worker writes one
		// line per job and this is one of the things it can say.
		return SendOutcome{Status: claim.StatusBefore, Part: int(msg.PartNo), Parts: int(msg.PartsTotal)}, nil
	}
	if claim.StatusBefore == MessageSending {
		s.log.Warnw("resending a digest part whose previous delivery outcome is unknown; the channel may show it twice",
			"team", team, "message_id", messageID, "part", msg.PartNo, "parts", msg.PartsTotal)
		metrics.SlackResendUncertain(team)
	}

	req, err := postRequest(msg)
	if err != nil {
		// A payload we cannot decode will never decode: fail the row rather
		// than retry ten times.
		s.markSendFailed(ctx, messageID, err)
		metrics.SlackSendError(team, metrics.SlackErrAPI)
		s.settleRun(ctx, msg.DigestRunID, team)
		return SendOutcome{}, err
	}

	res, err := s.slack.PostMessage(ctx, req)
	if err != nil {
		metrics.SlackSendError(team, sendErrorReason(err))
		switch {
		case !retryableSendError(err):
			// A bad token, an unknown channel or a missing scope is a
			// configuration problem: retrying only burns the rate-limit budget.
			s.markSendFailed(ctx, messageID, err)
			s.settleRun(ctx, msg.DigestRunID, team)
		case deliveryRefused(err):
			// Slack answered at the application layer, so the message is
			// definitively NOT in the channel. Hand the claim back so the next
			// attempt is an ordinary 'pending' delivery: leaving it at 'sending'
			// is what made slack_resend_uncertain_total fire on `ratelimited`,
			// the most common answer Slack gives, and made the one metric that
			// should mean "a human may see a duplicate" unalertable.
			s.releaseClaim(ctx, messageID, err)
		}
		// A transport failure or a 5xx leaves the row 'sending' on purpose: the
		// POST may have been received, and the resend branch above is what makes
		// trying again safe and visible.
		return SendOutcome{}, fmt.Errorf("post digest part %d/%d: %w", msg.PartNo, msg.PartsTotal, err)
	}

	// Detached: the message is in the channel now, and losing this write is
	// exactly what produces the uncertain resend above.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := s.st.DigestMessage().MarkSent(writeCtx, res.TS, messageID); err != nil {
		return SendOutcome{}, fmt.Errorf("record digest part %s as sent: %w", messageID, err)
	}
	metrics.SlackSent(team)
	s.settleRun(ctx, msg.DigestRunID, team)
	return SendOutcome{
		Delivered: true, Status: claim.StatusBefore,
		Part: int(msg.PartNo), Parts: int(msg.PartsTotal), TS: res.TS,
	}, nil
}

// SendOutcome is what one SendMessage call did. It exists so the worker's single
// summary line can name the branch instead of inferring delivery from a nil
// error — every no-op branch returns nil too.
type SendOutcome struct {
	// Delivered is true only when this call posted to Slack.
	Delivered bool
	// Status is the row's status before the claim: 'sent', 'failed' or 'dry_run'
	// on a no-op, 'pending' or 'sending' on a delivery. Empty when the call never
	// reached the claim.
	Status string
	Part   int
	Parts  int
	// TS is Slack's message timestamp, set only when Delivered.
	TS string
}

// settleRun recomputes the run's status once this part has stopped moving
// (§13.5: 'sent' when every part is confirmed, 'partial' when some are not).
//
// Nothing else ever writes those two values: the build writes 'built' /
// 'dry_run' / 'failed' and a build-time 'partial' (repositories that could not
// be inspected) and never touches the row again, so a digest whose parts all
// failed is byte-identical to one that was delivered — and the journal §6.6
// keeps "для отладки/метрик" cannot answer the one question it exists for.
//
// Every part calls it; the query is a no-op until the last one settles, so which
// worker gets there first does not matter. Best-effort: the delivery itself has
// already happened and is recorded, and losing the roll-up costs a journal
// entry, not a message.
func (s *Service) settleRun(ctx context.Context, runID uuid.UUID, team string) {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	statuses, err := s.st.DigestRun().SettleDelivery(writeCtx, runID)
	if err != nil {
		s.log.Warnw("recomputing the digest run status failed", "team", team, "run_id", runID, "err", err)
		return
	}
	if len(statuses) == 0 {
		return // parts still in flight
	}
	s.log.Infow("digest run settled", "team", team, "run_id", runID, "status", statuses[0])
}

// releaseClaim returns a claimed row to 'pending' after a failure that is known
// not to have delivered anything.
func (s *Service) releaseClaim(ctx context.Context, messageID uuid.UUID, cause error) {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	stored := security.Truncate(security.Mask(cause.Error()), maxStoredErrorLen)
	if err := s.st.DigestMessage().ReleaseClaim(writeCtx, stored, messageID); err != nil {
		s.log.Warnw("releasing the digest part claim failed; the retry will report an uncertain resend",
			"message_id", messageID, "err", err)
	}
}

// postRequest rebuilds the chat.postMessage payload from the stored part. The
// blocks are stored rather than rebuilt so a retry delivers exactly what the
// digest run assembled, even if the MRs have moved on since.
func postRequest(msg *models.DigestMessage) (slack.PostMessageRequest, error) {
	var m slack.Message
	if err := json.Unmarshal(msg.Payload, &m); err != nil {
		return slack.PostMessageRequest{}, fmt.Errorf("decode digest payload %s: %w", msg.ID, err)
	}
	return m.Post(msg.Channel), nil
}

// markSendFailed records a terminal delivery failure on a detached context.
func (s *Service) markSendFailed(ctx context.Context, messageID uuid.UUID, cause error) {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	stored := security.Truncate(security.Mask(cause.Error()), maxStoredErrorLen)
	if err := s.st.DigestMessage().MarkFailed(writeCtx, stored, messageID); err != nil {
		s.log.Errorw("recording the failed digest delivery failed", "message_id", messageID, "err", err)
	}
}

// retryableSendError reports whether another attempt could succeed. A Slack
// error code that is not a rate limit is a configuration problem; anything
// without a code is a transport failure and is worth retrying.
func retryableSendError(err error) bool {
	if slack.IsRateLimited(err) {
		return true
	}
	return slack.ErrorCode(err) == ""
}

// deliveryRefused reports whether Slack answered this POST with a definite "no".
//
// It is the difference between "not delivered" and "unknown", which is the whole
// basis of the §6.4 protocol. Slack refuses at the application layer with HTTP
// 200 plus `ok:false`, and rate-limits with HTTP 429 — in both cases the message
// is certainly not in the channel. A transport failure (no APIError at all) and
// a 5xx (`http_error`) are genuinely ambiguous: the request may have been
// received and executed before the connection broke, so those must keep the row
// at 'sending' and pay for the uncertain-resend warning.
func deliveryRefused(err error) bool {
	var apiErr *slack.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.Status == http.StatusOK || apiErr.Status == http.StatusTooManyRequests
}

// sendErrorReason maps a delivery failure onto the closed reason set of
// slack_send_errors_total.
func sendErrorReason(err error) string {
	switch code := slack.ErrorCode(err); {
	case slack.IsRateLimited(err):
		return metrics.SlackErrRateLimited
	case code == "":
		return metrics.SlackErrNetwork
	default:
		return metrics.SlackErrAPI
	}
}
