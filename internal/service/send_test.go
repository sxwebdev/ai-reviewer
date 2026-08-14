package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/sxwebdev/ai-reviewer/internal/metrics"
	"github.com/sxwebdev/ai-reviewer/internal/models"
	"github.com/sxwebdev/ai-reviewer/internal/slack"
)

// slackServer is an httptest stand-in for the Slack Web API. Slack reports most
// failures with HTTP 200 and {"ok":false,...}, so the fixture does too.
type slackServer struct {
	mu       sync.Mutex
	posts    []slack.PostMessageRequest
	respond  func(n int) (status int, body string, header http.Header)
	srv      *httptest.Server
	baseURL  string
	callsGot int
}

func newSlackServer(t *testing.T) *slackServer {
	t.Helper()
	s := &slackServer{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req slack.PostMessageRequest
		_ = json.Unmarshal(body, &req)

		s.mu.Lock()
		s.posts = append(s.posts, req)
		s.callsGot++
		n := s.callsGot
		respond := s.respond
		s.mu.Unlock()

		status, payload, header := http.StatusOK,
			`{"ok":true,"channel":"C123","ts":"1700000000.0001"}`, http.Header{}
		if respond != nil {
			status, payload, header = respond(n)
		}
		for k, vs := range header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(s.srv.Close)
	s.baseURL = s.srv.URL
	return s
}

func (s *slackServer) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.callsGot
}

func (s *slackServer) client(t *testing.T) *slack.Client {
	t.Helper()
	c, err := slack.New(slack.Config{Token: "xoxb-test-token", BaseURL: s.baseURL, MaxAttempts: 2})
	if err != nil {
		t.Fatalf("slack.New: %v", err)
	}
	return c
}

// sendHarness builds a digest, then swaps in a real Slack client pointed at an
// httptest server so delivery goes over HTTP the way it does in production.
func sendHarness(t *testing.T) (*harness, *slackServer, uuid.UUID) {
	t.Helper()
	h := digestHarness(t)
	srv := newSlackServer(t)
	h.svc.slack = srv.client(t)

	out, err := h.svc.BuildDigest(t.Context(), testTeamConfig(), "09:00", digestDay, 0)
	if err != nil {
		t.Fatalf("BuildDigest: %v", err)
	}
	if len(out.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(out.Messages))
	}
	return h, srv, out.Messages[0]
}

func (h *harness) messageRow(t *testing.T, id uuid.UUID) (status, slackTS, errText string) {
	t.Helper()
	if err := h.pool.QueryRow(t.Context(),
		`SELECT status, slack_ts, error FROM digest_messages WHERE id = $1`, id).
		Scan(&status, &slackTS, &errText); err != nil {
		t.Fatalf("read digest message: %v", err)
	}
	return status, slackTS, errText
}

func (h *harness) setMessageStatus(t *testing.T, id uuid.UUID, status string) {
	t.Helper()
	if _, err := h.pool.Exec(t.Context(),
		`UPDATE digest_messages SET status = $1 WHERE id = $2`, status, id); err != nil {
		t.Fatalf("set message status: %v", err)
	}
}

func TestSendMessageDelivers(t *testing.T) {
	h, srv, id := sendHarness(t)

	if err := h.svc.SendMessage(t.Context(), id); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if srv.calls() != 1 {
		t.Errorf("chat.postMessage calls = %d, want 1", srv.calls())
	}
	status, ts, _ := h.messageRow(t, id)
	if status != MessageSent {
		t.Errorf("status = %q, want %q", status, MessageSent)
	}
	if ts != "1700000000.0001" {
		t.Errorf("slack_ts = %q, want the ts Slack returned", ts)
	}
	if got := srv.posts[0].Channel; got != "C123" {
		t.Errorf("channel = %q, want the team's channel", got)
	}
	if len(srv.posts[0].Blocks) == 0 || srv.posts[0].Text == "" {
		t.Error("the stored Block Kit payload must be delivered verbatim, text fallback included")
	}
}

// TestSendMessageBranchesOnTheClaimedStatus is the §6.4 table. The four
// no-op statuses and the two that send are the whole protocol.
func TestSendMessageBranchesOnTheClaimedStatus(t *testing.T) {
	cases := []struct {
		name       string
		before     string
		wantPosts  int
		wantStatus string
	}{
		{"pending is an ordinary first delivery", MessagePending, 1, MessageSent},
		// A crash between the POST and the result write: the outcome is
		// genuinely unknown, and a duplicate in the channel beats a digest the
		// team never sees.
		{"sending is resent on purpose", MessageSending, 1, MessageSent},
		{"sent is a no-op", MessageSent, 0, MessageSent},
		{"failed is a no-op", MessageFailed, 0, MessageFailed},
		{"dry_run is a no-op", MessageDryRun, 0, MessageDryRun},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, srv, id := sendHarness(t)
			h.setMessageStatus(t, id, c.before)

			if err := h.svc.SendMessage(t.Context(), id); err != nil {
				t.Fatalf("SendMessage: %v", err)
			}
			if srv.calls() != c.wantPosts {
				t.Errorf("chat.postMessage calls = %d, want %d", srv.calls(), c.wantPosts)
			}
			if status, _, _ := h.messageRow(t, id); status != c.wantStatus {
				t.Errorf("status = %q, want %q", status, c.wantStatus)
			}
		})
	}
}

// TestSendMessageResendIsCounted pins the observability of the deliberate
// duplicate: without the counter a resend is invisible.
func TestSendMessageResendIsCounted(t *testing.T) {
	h, _, id := sendHarness(t)
	h.setMessageStatus(t, id, MessageSending)

	before := counterValue(t, metrics.SlackResendUncertainTotal.WithLabelValues(testTeam))
	if err := h.svc.SendMessage(t.Context(), id); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if got := counterValue(t, metrics.SlackResendUncertainTotal.WithLabelValues(testTeam)); got != before+1 {
		t.Errorf("slack_resend_uncertain_total = %v, want %v", got, before+1)
	}
}

// TestSendMessageRateLimitLeavesTheRowRetryable is the §17 bullet: a rate limit
// is retried without rebuilding the digest, and the row must stay claimable.
//
// It also pins which claim state the retry finds. `ratelimited` is Slack saying
// "I did not accept this", so the row goes back to 'pending' and the retry is an
// ordinary first delivery. Leaving it at 'sending' made the next attempt log
// "previous delivery outcome is unknown" and bump slack_resend_uncertain_total —
// on the most common answer Slack gives, which made the one metric that should
// mean "a human may see a duplicate" impossible to alert on.
func TestSendMessageRateLimitLeavesTheRowRetryable(t *testing.T) {
	h, srv, id := sendHarness(t)
	srv.respond = func(n int) (int, string, http.Header) {
		if n <= 2 { // exhausts the client's own retry budget
			return http.StatusTooManyRequests, `{"ok":false,"error":"ratelimited"}`,
				http.Header{"Retry-After": []string{"0"}}
		}
		return http.StatusOK, `{"ok":true,"channel":"C123","ts":"1700000000.0002"}`, nil
	}

	err := h.svc.SendMessage(t.Context(), id)
	if err == nil {
		t.Fatal("a rate limit that outlasts the client's retries must reach the job")
	}
	if !slack.IsRateLimited(err) {
		t.Errorf("error = %v, want a rate-limit error", err)
	}
	status, _, _ := h.messageRow(t, id)
	if status != MessagePending {
		t.Errorf("status = %q, want %q: Slack refused, so nothing is in the channel and the "+
			"retry must not be reported as an uncertain resend", status, MessagePending)
	}

	// The retry delivers without the digest being rebuilt, and without counting
	// an uncertain resend.
	before := counterValue(t, metrics.SlackResendUncertainTotal.WithLabelValues(testTeam))
	if err := h.svc.SendMessage(t.Context(), id); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if status, ts, _ := h.messageRow(t, id); status != MessageSent || ts != "1700000000.0002" {
		t.Errorf("after the retry status=%q ts=%q, want a delivered message", status, ts)
	}
	if got := counterValue(t, metrics.SlackResendUncertainTotal.WithLabelValues(testTeam)); got != before {
		t.Errorf("slack_resend_uncertain_total = %v, want %v: an ordinary retry is not an uncertain resend", got, before)
	}
}

// TestSendMessageTransportFailureKeepsTheClaim is the other side of the same
// rule: a connection that dies mid-POST is genuinely ambiguous — Slack may have
// accepted the message before the wire broke — so the row must stay 'sending'
// and the retry must pay for the uncertain-resend warning.
func TestSendMessageTransportFailureKeepsTheClaim(t *testing.T) {
	h, srv, id := sendHarness(t)
	// A dead listener is the cheapest honest transport failure: the request
	// never produces a Slack answer, so nothing can say whether it arrived.
	srv.srv.Close()

	if err := h.svc.SendMessage(t.Context(), id); err == nil {
		t.Fatal("a transport failure must reach the job")
	}
	if status, _, _ := h.messageRow(t, id); status != MessageSending {
		t.Errorf("status = %q, want %q: the outcome is unknown, so the retry must be flagged",
			status, MessageSending)
	}
}

// TestSendMessageSettlesTheRun implements §13.5's delivery half: 'sent' when
// every part is confirmed, 'partial' when some are not.
//
// Nothing wrote either value. The build-time statuses were the only ones a run
// ever got, so a digest whose parts all failed was byte-identical to one that
// was delivered — and `SELECT * FROM digest_runs WHERE status <> 'sent'`, the
// query the schema invites, returned every row that ever existed.
func TestSendMessageSettlesTheRun(t *testing.T) {
	cases := []struct {
		name string
		// outcomes is the delivery result of each part, in order.
		outcomes []bool
		build    string // digest_runs.status before delivery
		want     string
	}{
		{"every part delivered", []bool{true, true}, DigestBuilt, DigestSent},
		{"one part lost", []bool{true, false}, DigestBuilt, DigestPartial},
		{"nothing delivered", []bool{false, false}, DigestBuilt, DigestFailed},
		// A run built on incomplete repository data stays partial even when every
		// part lands: promoting it to 'sent' would erase the one fact the §6.6
		// journal exists to record.
		{"partial data survives a full delivery", []bool{true, true}, DigestPartial, DigestPartial},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, srv, _ := sendHarness(t)
			runID := h.onlyDigestRun(t)
			if _, err := h.pool.Exec(t.Context(),
				`UPDATE digest_runs SET status = $1 WHERE id = $2`, c.build, runID); err != nil {
				t.Fatalf("set build status: %v", err)
			}
			ids := h.splitIntoParts(t, runID, len(c.outcomes))

			for i, ok := range c.outcomes {
				if ok {
					srv.respond = nil
				} else {
					// A configuration error is terminal, so the part settles as
					// 'failed' rather than staying claimable.
					srv.respond = func(int) (int, string, http.Header) {
						return http.StatusOK, `{"ok":false,"error":"channel_not_found"}`, nil
					}
				}
				err := h.svc.SendMessage(t.Context(), ids[i])
				if ok != (err == nil) {
					t.Fatalf("part %d: err = %v, want delivered=%v", i, err, ok)
				}

				// Until the last part settles the run keeps its build status: a
				// half-delivered digest is not an answer yet.
				if i < len(c.outcomes)-1 {
					if got := h.digestRunStatus(t, runID); got != c.build {
						t.Errorf("after part %d the run is %q, want it to stay %q", i, got, c.build)
					}
				}
			}

			if got := h.digestRunStatus(t, runID); got != c.want {
				t.Errorf("digest_runs.status = %q, want %q", got, c.want)
			}
		})
	}
}

func (h *harness) onlyDigestRun(t *testing.T) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := h.pool.QueryRow(t.Context(), `SELECT id FROM digest_runs`).Scan(&id); err != nil {
		t.Fatalf("read digest run: %v", err)
	}
	return id
}

// splitIntoParts clones the run's single built part into n parts so the settle
// matrix can exercise a multi-part digest without a fixture large enough to
// split on its own.
func (h *harness) splitIntoParts(t *testing.T, runID uuid.UUID, n int) []uuid.UUID {
	t.Helper()
	msgs := h.digestMessages(t, runID)
	if len(msgs) != 1 {
		t.Fatalf("fixture has %d parts, want 1 to clone", len(msgs))
	}
	ids := []uuid.UUID{msgs[0].ID}
	for i := 2; i <= n; i++ {
		var id uuid.UUID
		if err := h.pool.QueryRow(t.Context(),
			`INSERT INTO digest_messages (digest_run_id, part_no, parts_total, channel, payload, status)
			 VALUES ($1, $2, $3, $4, $5, 'pending') RETURNING id`,
			runID, i, n, msgs[0].Channel, msgs[0].Payload).Scan(&id); err != nil {
			t.Fatalf("clone part %d: %v", i, err)
		}
		ids = append(ids, id)
	}
	if _, err := h.pool.Exec(t.Context(),
		`UPDATE digest_messages SET parts_total = $1 WHERE digest_run_id = $2`, n, runID); err != nil {
		t.Fatalf("set parts_total: %v", err)
	}
	return ids
}

// TestSendMessageWithoutASlackClientDoesNotClaimTheRow: a precondition no retry
// can satisfy must not be checked after taking durable state. Claiming first
// flipped the row to 'sending', so every retry took the resend branch, logged
// "previous delivery outcome is unknown" and bumped slack_resend_uncertain_total
// before failing on the same guard — then left the row stuck at 'sending' with a
// metric reporting a duplicate that never happened.
func TestSendMessageWithoutASlackClientDoesNotClaimTheRow(t *testing.T) {
	h, _, id := sendHarness(t)
	h.svc.slack = nil

	before := counterValue(t, metrics.SlackResendUncertainTotal.WithLabelValues(testTeam))
	if err := h.svc.SendMessage(t.Context(), id); err == nil {
		t.Fatal("a missing Slack client must be reported")
	}
	if status, _, _ := h.messageRow(t, id); status != MessageFailed {
		t.Errorf("status = %q, want %q: retrying cannot conjure a client", status, MessageFailed)
	}
	if got := counterValue(t, metrics.SlackResendUncertainTotal.WithLabelValues(testTeam)); got != before {
		t.Errorf("slack_resend_uncertain_total = %v, want %v: nothing was ever posted", got, before)
	}
	// And the run is settled rather than left looking like a delivery in flight.
	if got := h.digestRunStatus(t, h.onlyDigestRun(t)); got != DigestFailed {
		t.Errorf("digest_runs.status = %q, want %q", got, DigestFailed)
	}
}

// TestSendMessageConfigurationErrorIsTerminal keeps a bad token or an unknown
// channel from burning ten attempts and the rate-limit budget with them.
func TestSendMessageConfigurationErrorIsTerminal(t *testing.T) {
	cases := []string{"invalid_auth", "channel_not_found", "not_in_channel"}
	for _, code := range cases {
		t.Run(code, func(t *testing.T) {
			h, srv, id := sendHarness(t)
			srv.respond = func(int) (int, string, http.Header) {
				return http.StatusOK, fmt.Sprintf(`{"ok":false,"error":%q}`, code), nil
			}

			err := h.svc.SendMessage(t.Context(), id)
			if err == nil {
				t.Fatal("a configuration error must be reported")
			}
			if srv.calls() != 1 {
				t.Errorf("chat.postMessage calls = %d, want 1: a config error is not retried", srv.calls())
			}
			status, _, errText := h.messageRow(t, id)
			if status != MessageFailed {
				t.Errorf("status = %q, want %q", status, MessageFailed)
			}
			if errText == "" {
				t.Error("the failure must record why")
			}

			// A later attempt is a clean no-op rather than a second POST.
			if err := h.svc.SendMessage(t.Context(), id); err != nil {
				t.Fatalf("second SendMessage: %v", err)
			}
			if srv.calls() != 1 {
				t.Errorf("chat.postMessage calls = %d after the retry, want 1", srv.calls())
			}
		})
	}
}

// TestSendMessageOnePartFailingDoesNotCancelTheOthers is the §17 bullet: parts
// are independent deliveries.
func TestSendMessageOnePartFailingDoesNotCancelTheOthers(t *testing.T) {
	h, srv, first := sendHarness(t)

	// A second part of the same run, delivered independently.
	second := uuid.UUID{}
	if err := h.pool.QueryRow(t.Context(),
		`INSERT INTO digest_messages (digest_run_id, part_no, parts_total, channel, payload, status)
		 SELECT digest_run_id, 2, 2, channel, payload, 'pending' FROM digest_messages WHERE id = $1
		 RETURNING id`, first).Scan(&second); err != nil {
		t.Fatalf("seed the second part: %v", err)
	}

	srv.respond = func(n int) (int, string, http.Header) {
		if n == 1 {
			return http.StatusOK, `{"ok":false,"error":"channel_not_found"}`, nil
		}
		return http.StatusOK, `{"ok":true,"channel":"C123","ts":"1700000000.0003"}`, nil
	}

	if err := h.svc.SendMessage(t.Context(), first); err == nil {
		t.Fatal("the first part must report its failure")
	}
	if err := h.svc.SendMessage(t.Context(), second); err != nil {
		t.Fatalf("the second part must still be delivered: %v", err)
	}
	if status, _, _ := h.messageRow(t, first); status != MessageFailed {
		t.Errorf("first part status = %q, want %q", status, MessageFailed)
	}
	if status, _, _ := h.messageRow(t, second); status != MessageSent {
		t.Errorf("second part status = %q, want %q", status, MessageSent)
	}
}

func TestSendMessageWithAnUnreadablePayloadFailsTerminally(t *testing.T) {
	h, srv, id := sendHarness(t)
	if _, err := h.pool.Exec(t.Context(),
		`UPDATE digest_messages SET payload = '[]' WHERE id = $1`, id); err != nil {
		t.Fatalf("corrupt the payload: %v", err)
	}

	if err := h.svc.SendMessage(t.Context(), id); err == nil {
		t.Fatal("an undecodable payload must be reported")
	}
	if srv.calls() != 0 {
		t.Errorf("chat.postMessage calls = %d, want 0", srv.calls())
	}
	if status, _, _ := h.messageRow(t, id); status != MessageFailed {
		t.Errorf("status = %q, want %q: a payload that cannot decode never will", status, MessageFailed)
	}
}

func TestSendMessageWithoutASlackClient(t *testing.T) {
	h, _, id := sendHarness(t)
	h.svc.slack = nil
	if err := h.svc.SendMessage(t.Context(), id); err == nil {
		t.Fatal("SendMessage without a Slack client must fail loudly")
	}
}

func TestSendErrorReason(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"rate limited", &slack.APIError{Method: "chat.postMessage", Code: "ratelimited"}, metrics.SlackErrRateLimited},
		{"api error", &slack.APIError{Method: "chat.postMessage", Code: "channel_not_found"}, metrics.SlackErrAPI},
		{"transport", errors.New("dial tcp: connection refused"), metrics.SlackErrNetwork},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := sendErrorReason(c.err); got != c.want {
				t.Errorf("sendErrorReason(%v) = %q, want %q", c.err, got, c.want)
			}
			retryable := retryableSendError(c.err)
			wantRetryable := c.want != metrics.SlackErrAPI
			if retryable != wantRetryable {
				t.Errorf("retryableSendError(%v) = %v, want %v", c.err, retryable, wantRetryable)
			}
		})
	}
}

func TestPostRequestRebuildsTheStoredPayload(t *testing.T) {
	t.Parallel()
	payload, err := json.Marshal(slack.Message{
		Part: 1, Parts: 2, Text: "fallback",
		Blocks: []slack.Block{{Type: "section", Text: &slack.Text{Type: "mrkdwn", Text: "body"}}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := postRequest(&models.DigestMessage{Payload: payload, Channel: "C777"})
	if err != nil {
		t.Fatalf("postRequest: %v", err)
	}
	if req.Channel != "C777" || req.Text != "fallback" || len(req.Blocks) != 1 {
		t.Errorf("post request = %+v, want the stored blocks addressed to the channel column", req)
	}
}

// counterValue reads a prometheus counter directly so a metric assertion does
// not depend on scraping.
func counterValue(t *testing.T, c prometheus.Collector) float64 {
	t.Helper()
	return testutil.ToFloat64(c)
}
