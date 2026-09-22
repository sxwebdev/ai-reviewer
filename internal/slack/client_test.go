package slack_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sxwebdev/ai-reviewer/internal/slack"
)

const testToken = "xoxb-test-0000000000-secret-value"

// newTestClient wires a client at srv with instant retries and a recording
// Retry-After sleeper, so retry behaviour is asserted without waiting.
func newTestClient(t *testing.T, srv *httptest.Server, cfg slack.Config) (*slack.Client, *[]time.Duration) {
	t.Helper()
	cfg.Token = testToken
	cfg.BaseURL = srv.URL
	if cfg.MaxAttempts == 0 {
		cfg.MaxAttempts = 3
	}
	c, err := slack.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var slept []time.Duration
	c.SetRetryDelayForTest(time.Millisecond)
	c.SetSleepForTest(func(ctx context.Context, d time.Duration) error {
		slept = append(slept, d)
		return ctx.Err()
	})
	return c, &slept
}

func TestNewRequiresToken(t *testing.T) {
	t.Parallel()
	if _, err := slack.New(slack.Config{}); err == nil {
		t.Fatal("want error for empty token")
	}
}

func TestListUsersFollowsCursor(t *testing.T) {
	t.Parallel()

	var gotLimits, gotCursors []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/users.list" {
			t.Errorf("path = %q, want /users.list", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+testToken {
			t.Errorf("Authorization = %q", got)
		}
		if strings.Contains(r.URL.RawQuery, testToken) {
			t.Errorf("token leaked into the query string: %q", r.URL.RawQuery)
		}
		gotLimits = append(gotLimits, r.URL.Query().Get("limit"))
		cursor := r.URL.Query().Get("cursor")
		gotCursors = append(gotCursors, cursor)
		if cursor == "" {
			writeJSON(t, w, `{"ok":true,"members":[{"id":"U1","name":"ann"}],
				"response_metadata":{"next_cursor":"page2"}}`)
			return
		}
		writeJSON(t, w, `{"ok":true,"members":[{"id":"U2","name":"bob"}],
			"response_metadata":{"next_cursor":""}}`)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv, slack.Config{})
	users, err := c.ListUsers(t.Context())
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != 2 || users[0].ID != "U1" || users[1].ID != "U2" {
		t.Fatalf("users = %+v, want U1 then U2", users)
	}
	if want := []string{"200", "200"}; !equalStrings(gotLimits, want) {
		t.Errorf("limits = %v, want %v", gotLimits, want)
	}
	if want := []string{"", "page2"}; !equalStrings(gotCursors, want) {
		t.Errorf("cursors = %v, want %v", gotCursors, want)
	}
}

func TestOKFalseOnHTTP200IsAnError(t *testing.T) {
	t.Parallel()

	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK) // Slack reports failures with a 200
		writeJSON(t, w, `{"ok":false,"error":"invalid_auth"}`)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv, slack.Config{})
	_, err := c.ListUsers(t.Context())
	if err == nil {
		t.Fatal("want an error for ok:false")
	}
	if got := slack.ErrorCode(err); got != "invalid_auth" {
		t.Errorf("ErrorCode = %q, want invalid_auth", got)
	}
	var ae *slack.APIError
	if !errors.As(err, &ae) {
		t.Fatalf("error %v is not an *APIError", err)
	}
	if ae.Status != http.StatusOK || ae.Method != "users.list" {
		t.Errorf("APIError = %+v, want status 200 method users.list", ae)
	}
	// A bad token is not transient: retrying it only burns rate limit.
	if got := hits.Load(); got != 1 {
		t.Errorf("requests = %d, want 1 (no retry of a fatal Slack error)", got)
	}
}

func TestMissingScopeCarriesScopeDetail(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, `{"ok":false,"error":"missing_scope","needed":"users:read.email","provided":"users:read"}`)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv, slack.Config{})
	_, err := c.ListUsers(t.Context())
	var ae *slack.APIError
	if !errors.As(err, &ae) {
		t.Fatalf("error %v is not an *APIError", err)
	}
	if ae.Needed != "users:read.email" || ae.Provided != "users:read" {
		t.Errorf("scopes = %q/%q", ae.Needed, ae.Provided)
	}
	if msg := ae.Error(); !strings.Contains(msg, "users:read.email") {
		t.Errorf("Error() = %q, want the needed scope", msg)
	}
}

func TestErrorNeverCarriesTheToken(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(t, w, `{"ok":false,"error":"not_authed"}`)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv, slack.Config{})
	_, err := c.AuthTest(t.Context())
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("token leaked into the error: %v", err)
	}
}

func TestRateLimitedRetriesHonouringRetryAfter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		status     int
		body       string
		retryAfter string
		cfg        slack.Config
		want       time.Duration
	}{
		{
			name:       "http 429",
			status:     http.StatusTooManyRequests,
			body:       `{"ok":false,"error":"ratelimited"}`,
			retryAfter: "30",
			want:       30 * time.Second,
		},
		{
			// Slack also rate-limits with a 200 body on some methods.
			name:       "ok false on 200",
			status:     http.StatusOK,
			body:       `{"ok":false,"error":"ratelimited"}`,
			retryAfter: "12",
			want:       12 * time.Second,
		},
		{
			name:       "no header falls back",
			status:     http.StatusTooManyRequests,
			body:       `{"ok":false,"error":"ratelimited"}`,
			retryAfter: "",
			want:       5 * time.Second,
		},
		{
			name:       "clamped to MaxRetryAfter",
			status:     http.StatusTooManyRequests,
			body:       `{"ok":false,"error":"ratelimited"}`,
			retryAfter: "600",
			cfg:        slack.Config{MaxRetryAfter: 60 * time.Second},
			want:       60 * time.Second,
		},
		{
			name:       "garbage header falls back",
			status:     http.StatusTooManyRequests,
			body:       `{"ok":false,"error":"ratelimited"}`,
			retryAfter: "soon",
			want:       5 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var hits atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if hits.Add(1) == 1 {
					if tt.retryAfter != "" {
						w.Header().Set("Retry-After", tt.retryAfter)
					}
					w.WriteHeader(tt.status)
					writeJSON(t, w, tt.body)
					return
				}
				writeJSON(t, w, `{"ok":true,"members":[{"id":"U1","name":"ann"}]}`)
			}))
			defer srv.Close()

			c, slept := newTestClient(t, srv, tt.cfg)
			users, err := c.ListUsers(t.Context())
			if err != nil {
				t.Fatalf("ListUsers: %v", err)
			}
			if len(users) != 1 {
				t.Fatalf("users = %d, want 1", len(users))
			}
			if got := hits.Load(); got != 2 {
				t.Errorf("requests = %d, want 2 (one rate-limited, one served)", got)
			}
			if got := *slept; len(got) != 1 || got[0] != tt.want {
				t.Errorf("honoured delays = %v, want exactly [%v]", got, tt.want)
			}
		})
	}
}

func TestRateLimitExhaustionReturnsRateLimitedError(t *testing.T) {
	t.Parallel()

	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
		writeJSON(t, w, `{"ok":false,"error":"ratelimited"}`)
	}))
	defer srv.Close()

	c, slept := newTestClient(t, srv, slack.Config{MaxAttempts: 3})
	_, err := c.ListUsers(t.Context())
	if !slack.IsRateLimited(err) {
		t.Fatalf("err = %v, want a ratelimited APIError", err)
	}
	if got := hits.Load(); got != 3 {
		t.Errorf("requests = %d, want 3 (MaxAttempts)", got)
	}
	if got := len(*slept); got != 3 {
		t.Errorf("Retry-After waits = %d, want 3", got)
	}
}

func TestServerErrorsRetryClientErrorsDoNot(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		status       int
		wantRequests int64
	}{
		{name: "500 retries", status: http.StatusInternalServerError, wantRequests: 3},
		{name: "502 retries", status: http.StatusBadGateway, wantRequests: 3},
		{name: "404 stops", status: http.StatusNotFound, wantRequests: 1},
		{name: "403 stops", status: http.StatusForbidden, wantRequests: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var hits atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				hits.Add(1)
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()

			c, _ := newTestClient(t, srv, slack.Config{MaxAttempts: 3})
			if _, err := c.ListUsers(t.Context()); err == nil {
				t.Fatal("want an error")
			}
			if got := hits.Load(); got != tt.wantRequests {
				t.Errorf("requests = %d, want %d", got, tt.wantRequests)
			}
		})
	}
}

func TestRetryableSlackCodeRecovers(t *testing.T) {
	t.Parallel()

	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			writeJSON(t, w, `{"ok":false,"error":"internal_error"}`)
			return
		}
		writeJSON(t, w, `{"ok":true,"members":[]}`)
	}))
	defer srv.Close()

	c, slept := newTestClient(t, srv, slack.Config{})
	if _, err := c.ListUsers(t.Context()); err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("requests = %d, want 2", got)
	}
	if got := len(*slept); got != 0 {
		t.Errorf("Retry-After waits = %d, want 0 (not a rate limit)", got)
	}
}

func TestMalformedJSONIsNotRetried(t *testing.T) {
	t.Parallel()

	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		writeJSON(t, w, `{"ok":true,`)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv, slack.Config{})
	if _, err := c.ListUsers(t.Context()); err == nil {
		t.Fatal("want a decode error")
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("requests = %d, want 1", got)
	}
}

func TestUnnamedSlackErrorStillReported(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, `{"ok":false}`)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv, slack.Config{})
	_, err := c.ListUsers(t.Context())
	if got := slack.ErrorCode(err); got != "unknown_error" {
		t.Fatalf("ErrorCode = %q, want unknown_error", got)
	}
}

func TestPostMessage(t *testing.T) {
	t.Parallel()

	var got slack.PostMessageRequest
	var method, contentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		contentType = r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode body: %v", err)
		}
		writeJSON(t, w, `{"ok":true,"channel":"C1","ts":"1503435956.000247"}`)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv, slack.Config{})
	msg := slack.Message{Text: "MR Digest — payments", Blocks: []slack.Block{{Type: "divider"}}}
	res, err := c.PostMessage(t.Context(), msg.Post("C1"))
	if err != nil {
		t.Fatalf("PostMessage: %v", err)
	}
	if res.TS != "1503435956.000247" || res.Channel != "C1" {
		t.Errorf("result = %+v", res)
	}
	if method != http.MethodPost {
		t.Errorf("method = %q, want POST", method)
	}
	if !strings.HasPrefix(contentType, "application/json") {
		t.Errorf("content-type = %q", contentType)
	}
	if got.Channel != "C1" || got.Text != "MR Digest — payments" || len(got.Blocks) != 1 {
		t.Errorf("posted payload = %+v", got)
	}
	if got.UnfurlLinks || got.UnfurlMedia {
		t.Error("digest links must not unfurl")
	}
}

func TestPostMessageRequiresChannel(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("no request should be made")
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv, slack.Config{})
	if _, err := c.PostMessage(t.Context(), slack.PostMessageRequest{}); err == nil {
		t.Fatal("want an error for an empty channel")
	}
	if _, err := c.ConversationInfo(t.Context(), ""); err == nil {
		t.Fatal("want an error for an empty channel")
	}
}

func TestAuthTestAndConversationInfo(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth.test":
			writeJSON(t, w, `{"ok":true,"team":"acme","user":"ai-reviewer","team_id":"T1","user_id":"U9","bot_id":"B1"}`)
		case "/conversations.info":
			if got := r.URL.Query().Get("channel"); got != "C1" {
				t.Errorf("channel = %q", got)
			}
			writeJSON(t, w, `{"ok":true,"channel":{"id":"C1","name":"payments","is_channel":true,"is_member":true}}`)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv, slack.Config{})
	info, err := c.AuthTest(t.Context())
	if err != nil {
		t.Fatalf("AuthTest: %v", err)
	}
	if info.UserID != "U9" || info.BotID != "B1" || info.Team != "acme" {
		t.Errorf("auth info = %+v", info)
	}

	ch, err := c.ConversationInfo(t.Context(), "C1")
	if err != nil {
		t.Fatalf("ConversationInfo: %v", err)
	}
	if ch.ID != "C1" || ch.Name != "payments" || !ch.IsMember {
		t.Errorf("channel = %+v", ch)
	}
}

func TestContextCancellationStopsRetries(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		cancel()
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
		writeJSON(t, w, `{"ok":false,"error":"ratelimited"}`)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv, slack.Config{})
	if _, err := c.ListUsers(ctx); err == nil {
		t.Fatal("want an error once the context is cancelled")
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, body string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write([]byte(body)); err != nil {
		t.Errorf("write response: %v", err)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestErrorCodeIgnoresForeignErrors(t *testing.T) {
	t.Parallel()

	if got := slack.ErrorCode(errors.New("not a slack error")); got != "" {
		t.Errorf("ErrorCode = %q, want empty", got)
	}
	if slack.IsRateLimited(nil) {
		t.Error("IsRateLimited(nil) = true")
	}
}

func TestNewAppliesDefaults(t *testing.T) {
	t.Parallel()

	// Only a token: the client must be usable against the real Slack host.
	if _, err := slack.New(slack.Config{Token: testToken}); err != nil {
		t.Fatalf("New: %v", err)
	}
}

func TestUnbuildableRequestFailsFast(t *testing.T) {
	t.Parallel()

	c, err := slack.New(slack.Config{Token: testToken, BaseURL: "http://\x7f-bad-host"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.AuthTest(t.Context()); err == nil {
		t.Fatal("want an error for an unbuildable request")
	}
}

func TestUnexpectedResponseShapeIsAnError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// ok:true, but members is not an array.
		writeJSON(t, w, `{"ok":true,"members":"nope"}`)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv, slack.Config{})
	if _, err := c.ListUsers(t.Context()); err == nil {
		t.Fatal("want a decode error")
	}
}

func TestListUsersStopsOnAnEndlessCursor(t *testing.T) {
	t.Parallel()

	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		writeJSON(t, w, `{"ok":true,"members":[{"id":"U1"}],"response_metadata":{"next_cursor":"more"}}`)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv, slack.Config{})
	_, err := c.ListUsers(t.Context())
	if err == nil {
		t.Fatal("want an error when pagination never terminates")
	}
	if !strings.Contains(err.Error(), "pagination") {
		t.Errorf("err = %v, want it to name the pagination guard", err)
	}
	if got := hits.Load(); got != 200 {
		t.Errorf("requests = %d, want the 200-page guard to stop it", got)
	}
}

func TestCallErrorsPropagateToEveryEndpoint(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, `{"ok":false,"error":"channel_not_found"}`)
	}))
	defer srv.Close()

	c, _ := newTestClient(t, srv, slack.Config{})
	if _, err := c.ConversationInfo(t.Context(), "C1"); slack.ErrorCode(err) != "channel_not_found" {
		t.Errorf("ConversationInfo err = %v", err)
	}
	if _, err := c.PostMessage(t.Context(), slack.PostMessageRequest{Channel: "C1", Text: "x"}); slack.ErrorCode(err) != "channel_not_found" {
		t.Errorf("PostMessage err = %v", err)
	}
	if _, err := c.AuthTest(t.Context()); slack.ErrorCode(err) != "channel_not_found" {
		t.Errorf("AuthTest err = %v", err)
	}
}

// TestInterruptedRateLimitWaitStopsRetrying covers the shutdown path: if the
// wait for a Retry-After is cut short, the call gives up instead of hammering
// Slack with the attempt it never waited for.
func TestInterruptedRateLimitWaitStopsRetrying(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status int
	}{
		{name: "http 429", status: http.StatusTooManyRequests},
		{name: "ok false on 200", status: http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var hits atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				hits.Add(1)
				w.Header().Set("Retry-After", "30")
				w.WriteHeader(tt.status)
				writeJSON(t, w, `{"ok":false,"error":"ratelimited"}`)
			}))
			defer srv.Close()

			c, _ := newTestClient(t, srv, slack.Config{})
			stop := errors.New("shutting down")
			c.SetSleepForTest(func(context.Context, time.Duration) error { return stop })

			_, err := c.ListUsers(t.Context())
			if !errors.Is(err, stop) {
				t.Fatalf("err = %v, want %v", err, stop)
			}
			if got := hits.Load(); got != 1 {
				t.Errorf("requests = %d, want 1", got)
			}
		})
	}
}
