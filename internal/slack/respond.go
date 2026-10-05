package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sxwebdev/xutils/retry"
)

// Response types. An empty type is ephemeral — Slack's own default, and the
// one a personal digest relies on.
const (
	ResponseInChannel = "in_channel"
	ResponseEphemeral = "ephemeral"
)

// MaxCommandResponses is how many messages one response URL accepts, and
// CommandResponseWindow is how long it lives. Both are Slack's limits, restated
// here because the digest can be several parts long and the cap therefore has
// to be visible to whoever splits it.
const (
	MaxCommandResponses   = 5
	CommandResponseWindow = 30 * time.Minute
)

// defaultResponseHost is the only host a response URL may point at.
//
// The URL arrives over an authenticated socket, so this is not distrust of
// Slack — it is that the URL is then stored in a job argument, i.e. a database
// column, and handed to an HTTP client by a worker that may run minutes later on
// another replica. Pinning the host keeps a corrupted or edited row from turning
// this service into a request forger against an arbitrary address.
const defaultResponseHost = "hooks.slack.com"

// ResponseMessage is a delayed answer to a slash command.
type ResponseMessage struct {
	// Text is both the notification fallback and the whole message when Blocks
	// is empty.
	Text   string  `json:"text"`
	Blocks []Block `json:"blocks,omitempty"`
	// ResponseType is ResponseInChannel to post to everyone. Empty is ephemeral.
	ResponseType string `json:"response_type,omitempty"`
	// ReplaceOriginal is never set for a slash command — Slack ignores it there —
	// and exists so the struct describes the API rather than one call site.
	ReplaceOriginal bool `json:"replace_original,omitempty"`
}

// Response turns a built digest part into a delayed command response.
func (m Message) Response(inChannel bool) ResponseMessage {
	msg := ResponseMessage{Text: m.Text, Blocks: m.Blocks}
	if inChannel {
		msg.ResponseType = ResponseInChannel
	}
	return msg
}

// NoticeMessage is a one-line answer with no digest behind it: "nothing waiting
// on you", "this channel is not a team's". Ephemeral unless the caller says
// otherwise, because a notice is for whoever asked.
func NoticeMessage(text string) ResponseMessage {
	return ResponseMessage{Text: text}
}

// Respond posts one delayed answer to the response URL Slack supplied with a
// slash command.
//
// It carries no Authorization header: the URL *is* the credential. It is
// therefore never logged, and never returned inside an error — the errors below
// name the host and the status, never the path.
func (c *Client) Respond(ctx context.Context, responseURL string, msg ResponseMessage) error {
	if err := c.checkResponseURL(responseURL); err != nil {
		return err
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal command response: %w", err)
	}

	r := retry.New(
		retry.WithMaxAttempts(c.cfg.MaxAttempts),
		retry.WithPolicy(retry.PolicyBackoff),
		retry.WithDelay(c.retryDelay),
		retry.WithMaxDelay(10*time.Second),
		retry.WithContext(ctx),
	)
	return r.Do(func() error {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, responseURL, bytes.NewReader(payload))
		if err != nil {
			return fmt.Errorf("%w: build command response: %w", retry.ErrExit, err)
		}
		req.Header.Set("Content-Type", "application/json; charset=utf-8")

		resp, err := c.http.Do(req)
		if err != nil {
			// Deliberately not wrapped with the URL: a *url.Error already carries
			// it, so the message is rebuilt from the host alone.
			return fmt.Errorf("post command response to %s: %w", c.responseHost, redactURL(err))
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))

		switch {
		case resp.StatusCode == http.StatusTooManyRequests:
			if err := c.waitRateLimit(ctx, resp.Header); err != nil {
				return fmt.Errorf("%w: %w", retry.ErrExit, err)
			}
			return &APIError{Method: "response_url", Status: resp.StatusCode, Code: codeRateLimited}
		case resp.StatusCode >= 500:
			return &APIError{Method: "response_url", Status: resp.StatusCode, Code: codeHTTP}
		case resp.StatusCode < 200 || resp.StatusCode >= 300:
			// 404 here is the ordinary expiry: the URL lived 30 minutes or has
			// already taken its five responses. Retrying cannot help.
			return fmt.Errorf("%w: %w", retry.ErrExit, &APIError{
				Method: "response_url", Status: resp.StatusCode,
				Code: responseErrorCode(body),
			})
		}
		return nil
	})
}

// checkResponseURL rejects anything that is not a Slack response URL.
func (c *Client) checkResponseURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return errors.New("slack response url is not a URL")
	}
	scheme := "https"
	if c.insecureResponse {
		scheme = "http"
	}
	if u.Scheme != scheme || u.Hostname() != c.responseHost {
		// The URL itself is not quoted: it is a capability, and an error message
		// is the one place it would predictably end up in a log.
		return fmt.Errorf("slack response url must be an %s://%s address", scheme, c.responseHost)
	}
	return nil
}

// responseErrorCode reads the short error Slack returns on a response URL. The
// body is a plain string like "expired_url" or "no_text", not the usual
// {"ok":false} envelope, so this is deliberately lenient.
func responseErrorCode(body []byte) string {
	s := strings.TrimSpace(string(body))
	if s == "" {
		return codeHTTP
	}
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}

// redactURL strips the URL out of a transport error, keeping the cause.
func redactURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}
