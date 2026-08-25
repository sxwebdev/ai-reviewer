// Package slack is a minimal, hand-written Slack Web API client plus the
// Block Kit builder for the MR digest. It deliberately avoids an SDK and
// mirrors the house style of internal/gitlab: a plain Config, xutils/retry
// transport, a typed API error, context on every call.
//
// Slack is a notification surface only — review findings live in GitLab.
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
	"strconv"
	"strings"
	"time"

	"github.com/sxwebdev/ai-reviewer/internal/security"
	"github.com/sxwebdev/xutils/retry"
)

// defaultBaseURL is the Slack Web API root.
const defaultBaseURL = "https://slack.com/api"

// usersListPageSize is the page size for users.list. Slack recommends no more
// than 200 per page for that method regardless of the documented maximum.
const usersListPageSize = 200

// maxUserPages bounds users.list pagination so a server that keeps handing out
// a cursor cannot spin the worker forever. 200 pages = 40k members.
const maxUserPages = 200

// Config configures the Slack client.
type Config struct {
	// Token is the bot token. It travels in the Authorization header only —
	// never in a URL, a query string, or an error message.
	Token string
	// AppToken is the app-level token (xapp-…) that opens a Socket Mode
	// connection, and the only thing it can do: apps.connections.open is the one
	// method that accepts it, and it rejects the bot token with
	// not_allowed_token_type. Empty means this deployment does not listen for
	// commands.
	AppToken string
	// BaseURL defaults to https://slack.com/api. Tests point it at httptest.
	BaseURL string
	// Timeout is the per-request HTTP timeout (default 30s).
	Timeout time.Duration
	// MaxAttempts bounds retries of transient failures (default 4).
	MaxAttempts int
	// MaxRetryAfter caps an honoured Retry-After (default 60s). A longer
	// hint is clamped rather than obeyed: a worker blocked for minutes is
	// worse than one that burns an attempt and reports the rate limit.
	MaxRetryAfter time.Duration
}

// Client is a Slack Web API client.
type Client struct {
	cfg     Config
	baseURL string
	http    *http.Client
	// sleep waits out a Retry-After. It is a field rather than a direct
	// time.Sleep so tests can assert the honoured delay without waiting.
	sleep func(ctx context.Context, d time.Duration) error
	// retryDelay is the transport's own backoff base, separate from any
	// Retry-After Slack asks for.
	retryDelay time.Duration
	// responseHost pins where a delayed command answer may be POSTed, and
	// insecureResponse allows plain HTTP for it. Only a test moves either.
	responseHost     string
	insecureResponse bool
}

// APIError is a failed Slack API call. Slack reports most failures with HTTP
// 200 and {"ok":false,"error":"…"}, so Code — not Status — carries the meaning.
// The response body is deliberately not retained: it echoes request content and
// nothing in it is worth the risk of it reaching a log.
type APIError struct {
	Method   string // Slack method, e.g. "users.list"
	Status   int    // HTTP status; 200 for most Slack-level failures
	Code     string // Slack error code, e.g. "invalid_auth", "ratelimited"
	Needed   string // scope Slack says the call needs (missing_scope)
	Provided string // scopes the token actually carries
}

func (e *APIError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "slack %s: %s (http %d", e.Method, e.Code, e.Status)
	if e.Needed != "" {
		fmt.Fprintf(&b, "; needed %s", e.Needed)
	}
	if e.Provided != "" {
		fmt.Fprintf(&b, "; provided %s", e.Provided)
	}
	b.WriteString(")")
	return b.String()
}

// ErrorCode returns the Slack error code carried by err, or "" if err is not
// an *APIError.
func ErrorCode(err error) string {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Code
	}
	return ""
}

// IsRateLimited reports whether err is Slack's rate limit.
func IsRateLimited(err error) bool { return ErrorCode(err) == codeRateLimited }

// Slack error codes that are worth another attempt. Everything else (bad
// token, missing scope, unknown channel) is a configuration problem and
// retrying it only wastes the rate-limit budget.
const (
	codeRateLimited = "ratelimited"
	codeHTTP        = "http_error"
)

func isRetryableCode(code string) bool {
	switch code {
	case codeRateLimited, "internal_error", "service_unavailable", "fatal_error", "request_timeout":
		return true
	default:
		return false
	}
}

// New builds a client from cfg.
func New(cfg Config) (*Client, error) {
	if cfg.Token == "" {
		return nil, fmt.Errorf("slack token is required")
	}
	// The token must never surface in logs or job output, whichever layer
	// ends up printing an error that happens to embed it.
	security.RegisterSecret(cfg.Token)
	if cfg.AppToken != "" {
		security.RegisterSecret(cfg.AppToken)
	}

	if cfg.BaseURL == "" {
		cfg.BaseURL = defaultBaseURL
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 4
	}
	if cfg.MaxRetryAfter <= 0 {
		cfg.MaxRetryAfter = 60 * time.Second
	}
	return &Client{
		cfg:          cfg,
		baseURL:      strings.TrimRight(cfg.BaseURL, "/"),
		http:         &http.Client{Timeout: cfg.Timeout},
		sleep:        sleepCtx,
		retryDelay:   defaultRetryDelay,
		responseHost: defaultResponseHost,
	}, nil
}

// sleepCtx waits for d, or returns early if ctx is done.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// envelope is the part of every Slack response that is method-independent.
type envelope struct {
	OK       bool   `json:"ok"`
	Error    string `json:"error"`
	Needed   string `json:"needed"`
	Provided string `json:"provided"`
}

// get performs a GET with query parameters and decodes a successful response
// into out.
func (c *Client) get(ctx context.Context, method string, query url.Values, out any) error {
	return c.call(ctx, http.MethodGet, method, c.cfg.Token, query, nil, out)
}

// post performs a POST with a JSON body and decodes a successful response
// into out.
func (c *Client) post(ctx context.Context, method string, body, out any) error {
	return c.call(ctx, http.MethodPost, method, c.cfg.Token, nil, body, out)
}

// call performs one Slack API call with retry/backoff. Retryable: HTTP 429,
// HTTP 5xx, transport errors and the handful of Slack codes that mean "try
// again". Everything else stops immediately via retry.ErrExit.
//
// A Retry-After is waited out inside the attempt, because xutils/retry has no
// per-attempt delay hook; the policy's own backoff is then added on top. That
// overshoot is intentional slack (seconds on top of tens of seconds) — for a
// Tier 2 method like users.list, waiting slightly too long is free and waiting
// too little costs another 429.
// The token is a parameter rather than a field read because Slack has two of
// them and they are not interchangeable: every method here takes the bot token,
// apps.connections.open takes the app-level one and refuses the other.
func (c *Client) call(ctx context.Context, httpMethod, apiMethod, token string, query url.Values, body, out any) error {
	var payload []byte
	if body != nil {
		var err error
		// Unreachable for the request structs this package sends; kept so a
		// future payload with a custom marshaller cannot fail silently.
		if payload, err = json.Marshal(body); err != nil {
			return fmt.Errorf("marshal %s body: %w", apiMethod, err)
		}
	}

	u := c.baseURL + "/" + apiMethod
	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	var raw []byte
	r := retry.New(
		retry.WithMaxAttempts(c.cfg.MaxAttempts),
		retry.WithPolicy(retry.PolicyBackoff),
		retry.WithDelay(c.retryDelay),
		retry.WithMaxDelay(10*time.Second),
		retry.WithContext(ctx),
	)
	err := r.Do(func() error {
		var reqBody io.Reader
		if payload != nil {
			reqBody = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, httpMethod, u, reqBody)
		if err != nil {
			return fmt.Errorf("%w: build request: %w", retry.ErrExit, err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/json")
		if payload != nil {
			req.Header.Set("Content-Type", "application/json; charset=utf-8")
		}

		resp, err := c.http.Do(req)
		if err != nil {
			return fmt.Errorf("request %s: %w", apiMethod, err) // network → retry
		}
		defer func() { _ = resp.Body.Close() }()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))

		if resp.StatusCode == http.StatusTooManyRequests {
			apiErr := &APIError{Method: apiMethod, Status: resp.StatusCode, Code: codeRateLimited}
			if err := c.waitRateLimit(ctx, resp.Header); err != nil {
				return fmt.Errorf("%w: %w", retry.ErrExit, err)
			}
			return apiErr
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			apiErr := &APIError{Method: apiMethod, Status: resp.StatusCode, Code: codeHTTP}
			if resp.StatusCode >= 500 {
				return apiErr
			}
			return fmt.Errorf("%w: %w", retry.ErrExit, apiErr)
		}

		// HTTP 200 does not mean success: Slack puts the verdict in the body.
		var env envelope
		if err := json.Unmarshal(data, &env); err != nil {
			return fmt.Errorf("%w: decode %s: %w", retry.ErrExit, apiMethod, err)
		}
		if !env.OK {
			apiErr := &APIError{
				Method: apiMethod, Status: resp.StatusCode,
				Code: env.Error, Needed: env.Needed, Provided: env.Provided,
			}
			if apiErr.Code == "" {
				apiErr.Code = "unknown_error"
			}
			if apiErr.Code == codeRateLimited {
				if err := c.waitRateLimit(ctx, resp.Header); err != nil {
					return fmt.Errorf("%w: %w", retry.ErrExit, err)
				}
			}
			if isRetryableCode(apiErr.Code) {
				return apiErr
			}
			return fmt.Errorf("%w: %w", retry.ErrExit, apiErr)
		}
		raw = data
		return nil
	})
	if err != nil {
		// Surface the APIError itself so callers can inspect the code.
		var ae *APIError
		if errors.As(err, &ae) {
			return ae
		}
		return err
	}

	// Every method here wants its response decoded; the guard is for a
	// future fire-and-forget call.
	if out == nil || len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode %s: %w", apiMethod, err)
	}
	return nil
}

// waitRateLimit honours a Retry-After header before the next attempt.
func (c *Client) waitRateLimit(ctx context.Context, h http.Header) error {
	return c.sleep(ctx, retryAfter(h, c.cfg.MaxRetryAfter))
}

// defaultRetryDelay is the transport's backoff base for transient failures.
const defaultRetryDelay = time.Second

// defaultRetryAfter applies when Slack rate-limits without a usable hint.
const defaultRetryAfter = 5 * time.Second

// retryAfter reads the Retry-After header (Slack sends whole seconds) and
// clamps it to maxWait.
func retryAfter(h http.Header, maxWait time.Duration) time.Duration {
	d := defaultRetryAfter
	if v := strings.TrimSpace(h.Get("Retry-After")); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
			d = time.Duration(secs) * time.Second
		}
	}
	if maxWait > 0 && d > maxWait {
		return maxWait
	}
	return d
}

// ListUsers returns every member of the workspace, following cursor pagination.
//
// It is the only bulk read the service makes against Slack; the result is
// cached in-process by Directory, never persisted.
func (c *Client) ListUsers(ctx context.Context) ([]User, error) {
	var out []User
	cursor := ""
	for range maxUserPages {
		q := url.Values{"limit": {strconv.Itoa(usersListPageSize)}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var resp struct {
			Members          []User `json:"members"`
			ResponseMetadata struct {
				NextCursor string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		if err := c.get(ctx, "users.list", q, &resp); err != nil {
			return nil, err
		}
		out = append(out, resp.Members...)
		cursor = strings.TrimSpace(resp.ResponseMetadata.NextCursor)
		if cursor == "" {
			return out, nil
		}
	}
	return nil, fmt.Errorf("users.list: pagination did not terminate after %d pages", maxUserPages)
}

// AuthTest verifies the token and identifies the bot. Used by doctor.
func (c *Client) AuthTest(ctx context.Context) (*AuthInfo, error) {
	var resp AuthInfo
	if err := c.get(ctx, "auth.test", nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ConversationInfo returns channel metadata. Used by doctor to check that the
// configured channel exists and the bot is a member of it.
func (c *Client) ConversationInfo(ctx context.Context, channel string) (*Conversation, error) {
	if channel == "" {
		return nil, fmt.Errorf("channel is required")
	}
	var resp struct {
		Channel Conversation `json:"channel"`
	}
	q := url.Values{"channel": {channel}}
	if err := c.get(ctx, "conversations.info", q, &resp); err != nil {
		return nil, err
	}
	return &resp.Channel, nil
}

// PostMessage posts one message. Every digest delivery goes through the
// slack_send job, never inline with digest assembly.
func (c *Client) PostMessage(ctx context.Context, req PostMessageRequest) (*PostMessageResult, error) {
	if req.Channel == "" {
		return nil, fmt.Errorf("channel is required")
	}
	var resp PostMessageResult
	if err := c.post(ctx, "chat.postMessage", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
