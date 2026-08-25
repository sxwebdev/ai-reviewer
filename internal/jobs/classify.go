package jobs

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/riverqueue/river"

	"github.com/sxwebdev/ai-reviewer/internal/gitlab"
	"github.com/sxwebdev/ai-reviewer/internal/slack"
)

// This file is §17's third error level at the job layer: fatal vs retryable.
// The transport already separates them per request (internal/gitlab retries
// 429/5xx and stops on other 4xx; internal/slack honours Retry-After), and the
// service separates them per row (a bad channel marks the digest part failed).
// Nothing translated that into River's retry budget, so a revoked token had
// every scan_repo in every repository burn all three attempts — and do it again
// every scan_interval, forever, against a door that will not open.
//
// Deciding this here rather than in internal/service is deliberate: attempt
// budgets and retries belong to this package (the seam contract says so), and
// the service must stay free of River. The cost is that this one file knows the
// two client error types; it uses only their public status/code surface.

// classify wraps err so River stops retrying when retrying cannot help.
//
// river.JobCancel ends the job at this attempt regardless of how many remain,
// which is what turns "three attempts against a 401" into one. The error text
// is preserved, so `river_job.errors` still says what happened.
func classify(err error) error {
	if err == nil {
		return nil
	}
	if reason, fatal := fatalReason(err); fatal {
		return river.JobCancel(fmt.Errorf("%s (not retryable): %w", reason, err))
	}
	return err
}

// fatalReason reports whether another attempt is pointless, and why.
//
// Deliberately narrow. Everything not named here — timeouts, 5xx, 429, a
// cancelled context on shutdown, a database blip — stays retryable, because the
// cost of wrongly retrying is one more request while the cost of wrongly
// cancelling is silence until the next periodic pass.
func fatalReason(err error) (string, bool) {
	var ae *gitlab.APIError
	if errors.As(err, &ae) {
		switch ae.Status {
		case http.StatusUnauthorized:
			// The token is revoked, expired or wrong. No number of attempts
			// fixes a credential.
			return "gitlab rejected the credentials", true
		case http.StatusForbidden:
			// Authenticated but not permitted: a scope or a project role.
			return "gitlab denied access", true
		case http.StatusNotFound:
			// The project or merge request is gone, renamed, or invisible to
			// this token. Either way the target does not exist for us.
			return "gitlab has no such project or merge request", true
		}
	}

	// Slack answers most failures with HTTP 200 and an error code. Only the
	// codes below are named, rather than "any code that is not a rate limit":
	// Slack also reports its own outages as codes (internal_error,
	// service_unavailable, http_error), and cancelling on one of those would
	// throw away a digest that a retry would have delivered. An unrecognised
	// code stays retryable on purpose — the cost is a few wasted attempts, the
	// cost of the opposite mistake is a digest nobody sees.
	if code := slack.ErrorCode(err); fatalSlackCodes[code] {
		return "slack rejected the request (" + code + ")", true
	}
	return "", false
}

// fatalSlackCodes are the Slack errors no retry can fix: the token, the scopes,
// the channel, or the payload itself. The service has already marked the row
// failed by the time one of these reaches here (see retryableSendError), so the
// remaining attempts would only re-claim a terminal row.
var fatalSlackCodes = map[string]bool{
	"invalid_auth":       true, // the token is not valid
	"not_authed":         true, // no token was sent
	"account_inactive":   true, // the bot user is deactivated
	"token_revoked":      true,
	"token_expired":      true,
	"missing_scope":      true, // the app needs a scope it was not granted
	"no_permission":      true,
	"channel_not_found":  true, // the configured channel does not exist
	"not_in_channel":     true, // the bot was never invited
	"is_archived":        true,
	"msg_too_long":       true, // the payload is stored; resending it cannot shrink it
	"invalid_blocks":     true,
	"invalid_block_part": true,
}
