package slack_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/tkcrm/mx/logger"

	"github.com/sxwebdev/ai-reviewer/internal/slack"
)

const testAppToken = "xapp-1-A00000000-0000000000000-secretsecretsecret"

// fakeSlackSocket is Slack's half of Socket Mode: the apps.connections.open
// endpoint and the WebSocket it points at.
//
// It is a real WebSocket server rather than a stub of the loop, because
// everything worth testing here is protocol behaviour — that an envelope is
// acknowledged by id, that an acknowledgement carries the handler's answer, that
// a disconnect produces a new connection — and none of that is observable
// through an interface seam.
type fakeSlackSocket struct {
	t   *testing.T
	srv *httptest.Server

	mu sync.Mutex
	// acks are the acknowledgements received, in order.
	acks []socketAck
	// conns counts accepted WebSocket connections: reconnect behaviour is
	// visible here and nowhere else.
	conns atomic.Int32
	// openCalls counts apps.connections.open requests.
	openCalls atomic.Int32
	// failOpen is how many further apps.connections.open calls answer 503. It
	// models Slack being briefly unreachable, which is a different thing from a
	// token it will never accept.
	failOpen atomic.Int32
	// appTokens are the Authorization values apps.connections.open saw.
	appTokens []string

	// cur is the live connection, or nil between them. Frames are written to it
	// directly rather than through a channel: with a channel the goroutine of a
	// connection Slack has just dropped can still win the receive and write the
	// frame into a socket nobody is reading — which is a bug in the fake that
	// reads exactly like a listener that failed to reconnect.
	cur *websocket.Conn
}

// socketAck mirrors the acknowledgement shape the listener writes back.
type socketAck struct {
	EnvelopeID string                 `json:"envelope_id"`
	Payload    *slack.ResponseMessage `json:"payload"`
}

func newFakeSlackSocket(t *testing.T) *fakeSlackSocket {
	t.Helper()
	f := &fakeSlackSocket{t: t}
	mux := http.NewServeMux()
	mux.HandleFunc("/apps.connections.open", func(w http.ResponseWriter, r *http.Request) {
		f.openCalls.Add(1)
		f.mu.Lock()
		f.appTokens = append(f.appTokens, r.Header.Get("Authorization"))
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if f.failOpen.Load() > 0 {
			f.failOpen.Add(-1)
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"ok":false,"error":"service_unavailable"}`))
			return
		}
		// The URL Slack hands back is single-use; the fake simply points every
		// caller at its own socket endpoint.
		_, _ = w.Write([]byte(`{"ok":true,"url":"` + f.wsURL() + `"}`))
	})
	mux.HandleFunc("/socket", f.serveWS)

	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSlackSocket) wsURL() string {
	return strings.Replace(f.srv.URL, "http://", "ws://", 1) + "/socket"
}

func (f *fakeSlackSocket) serveWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		f.t.Errorf("accept websocket: %v", err)
		return
	}
	f.conns.Add(1)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// Reads run in their own goroutine so the writer below is never blocked by a
	// listener that is busy handling a command.
	go func() {
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var ack socketAck
			if err := json.Unmarshal(data, &ack); err != nil {
				f.t.Errorf("decode ack: %v", err)
				continue
			}
			f.mu.Lock()
			f.acks = append(f.acks, ack)
			f.mu.Unlock()
		}
	}()

	// Published only after the greeting, so the test's writer and this one never
	// hold the connection at the same time.
	_ = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"hello"}`))
	f.mu.Lock()
	f.cur = conn
	f.mu.Unlock()

	<-ctx.Done()
	f.mu.Lock()
	if f.cur == conn {
		f.cur = nil
	}
	f.mu.Unlock()
}

// push writes one frame to the live connection, waiting for one to exist.
func (f *fakeSlackSocket) push(frame string) {
	f.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		conn := f.cur
		f.mu.Unlock()
		if conn != nil {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			err := conn.Write(ctx, websocket.MessageText, []byte(frame))
			cancel()
			if err == nil {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.t.Fatal("no live connection to push a frame to")
}

// drop kills the live connection without a disconnect envelope, the way a lost
// network does.
func (f *fakeSlackSocket) drop() {
	f.mu.Lock()
	conn := f.cur
	f.cur = nil
	f.mu.Unlock()
	if conn != nil {
		_ = conn.CloseNow()
	}
}

// gotAcks returns the acknowledgements received so far.
func (f *fakeSlackSocket) gotAcks() []socketAck {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]socketAck(nil), f.acks...)
}

// waitForAcks blocks until n acknowledgements have arrived.
func (f *fakeSlackSocket) waitForAcks(n int) []socketAck {
	f.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if acks := f.gotAcks(); len(acks) >= n {
			return acks
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.t.Fatalf("timed out waiting for %d acknowledgement(s); got %d", n, len(f.gotAcks()))
	return nil
}

// waitForConns blocks until n connections have been accepted.
func (f *fakeSlackSocket) waitForConns(n int32) {
	f.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if f.conns.Load() >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.t.Fatalf("timed out waiting for %d connection(s); got %d", n, f.conns.Load())
}

// startSocket wires a listener to the fake and starts it.
func startSocket(t *testing.T, f *fakeSlackSocket, handle slack.CommandFunc) *slack.Socket {
	t.Helper()
	client, err := slack.New(slack.Config{
		Token: testToken, AppToken: testAppToken, BaseURL: f.srv.URL,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sock, err := slack.NewSocket(slack.SocketConfig{
		Client: client, Handle: handle, Log: logger.ForTests(t),
	})
	if err != nil {
		t.Fatalf("NewSocket: %v", err)
	}
	if err := sock.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := sock.Stop(ctx); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
	return sock
}

const slashCommandFrame = `{
  "type": "slash_commands",
  "envelope_id": "env-1",
  "accepts_response_payload": true,
  "payload": {
    "command": "/all",
    "channel_id": "C123",
    "user_id": "U42",
    "user_name": "rita",
    "response_url": "https://hooks.slack.com/commands/T1/B2/C3"
  }
}`

func TestSocketDeliversCommandsAndAcknowledges(t *testing.T) {
	t.Parallel()

	f := newFakeSlackSocket(t)
	got := make(chan slack.SlashCommand, 1)
	startSocket(t, f, func(_ context.Context, cmd slack.SlashCommand) slack.CommandAck {
		got <- cmd
		return slack.CommandAck{Text: "on it"}
	})

	f.push(slashCommandFrame)

	select {
	case cmd := <-got:
		if cmd.Command != "/all" || cmd.UserID != "U42" || cmd.ChannelID != "C123" {
			t.Errorf("command = %+v, want the payload as sent", cmd)
		}
		if cmd.ResponseURL != "https://hooks.slack.com/commands/T1/B2/C3" {
			t.Errorf("response URL = %q", cmd.ResponseURL)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the command never reached the handler")
	}

	acks := f.waitForAcks(1)
	if acks[0].EnvelopeID != "env-1" {
		t.Errorf("envelope_id = %q, want env-1 — an unacknowledged envelope is redelivered", acks[0].EnvelopeID)
	}
	// The acknowledgement is the answer: it is the only response that reliably
	// arrives inside Slack's three-second window.
	if acks[0].Payload == nil || acks[0].Payload.Text != "on it" {
		t.Errorf("ack payload = %+v, want the handler's answer", acks[0].Payload)
	}
	if acks[0].Payload.ResponseType != "" {
		t.Errorf("ack response_type = %q, want ephemeral: an 'on it' line is for whoever typed it",
			acks[0].Payload.ResponseType)
	}

	// The app-level token, not the bot token: apps.connections.open is the only
	// method that takes it and it rejects the other.
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.appTokens) == 0 || f.appTokens[0] != "Bearer "+testAppToken {
		t.Errorf("apps.connections.open Authorization = %v, want the app token", f.appTokens)
	}
}

// TestSocketAcknowledgesEnvelopesItIgnores: events_api and interactive arrive on
// the same socket. Slack redelivers anything unacknowledged, so dropping them
// silently would turn one stray event into a redelivery loop.
func TestSocketAcknowledgesEnvelopesItIgnores(t *testing.T) {
	t.Parallel()

	f := newFakeSlackSocket(t)
	var handled atomic.Int32
	startSocket(t, f, func(context.Context, slack.SlashCommand) slack.CommandAck {
		handled.Add(1)
		return slack.CommandAck{}
	})

	f.push(`{"type":"events_api","envelope_id":"env-evt","payload":{"event":{"type":"message"}}}`)

	acks := f.waitForAcks(1)
	if acks[0].EnvelopeID != "env-evt" {
		t.Errorf("envelope_id = %q, want env-evt", acks[0].EnvelopeID)
	}
	if acks[0].Payload != nil {
		t.Errorf("payload = %+v, want none for an envelope this service ignores", acks[0].Payload)
	}
	if n := handled.Load(); n != 0 {
		t.Errorf("handler ran %d time(s) for a non-command envelope", n)
	}
}

// TestSocketReconnectsWhenSlackAsks: Slack recycles a connection every few
// hours and warns first. A listener that treated that as a failure would sit out
// its backoff, and a command arriving in the gap is not retried by Slack — the
// caller just sees a timeout.
func TestSocketReconnectsWhenSlackAsks(t *testing.T) {
	t.Parallel()

	f := newFakeSlackSocket(t)
	got := make(chan slack.SlashCommand, 1)
	startSocket(t, f, func(_ context.Context, cmd slack.SlashCommand) slack.CommandAck {
		got <- cmd
		return slack.CommandAck{Text: "on it"}
	})
	f.waitForConns(1)

	f.push(`{"type":"disconnect","reason":"refresh_requested"}`)
	f.waitForConns(2)

	if n := f.openCalls.Load(); n < 2 {
		t.Errorf("apps.connections.open calls = %d, want a fresh ticket per connection", n)
	}

	// The new connection is a working one, which is the only claim that matters.
	f.push(slashCommandFrame)
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("the reconnected socket does not deliver commands")
	}
}

// TestSocketReconnectsAfterAnAbruptClose covers the other half: the connection
// simply dying, with no disconnect envelope.
func TestSocketReconnectsAfterAnAbruptClose(t *testing.T) {
	t.Parallel()

	f := newFakeSlackSocket(t)
	got := make(chan slack.SlashCommand, 1)
	startSocket(t, f, func(_ context.Context, cmd slack.SlashCommand) slack.CommandAck {
		got <- cmd
		return slack.CommandAck{}
	})
	f.waitForConns(1)

	f.drop()
	f.waitForConns(2)

	f.push(slashCommandFrame)
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("the socket did not recover from an abrupt close")
	}
}

// TestSocketStartFailsOnABadToken: the alternative is a service that starts
// green and answers nothing, with the reason buried in a reconnect loop.
func TestSocketStartFailsOnABadToken(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":false,"error":"not_allowed_token_type"}`))
	}))
	t.Cleanup(srv.Close)

	client, err := slack.New(slack.Config{Token: testToken, AppToken: testAppToken, BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sock, err := slack.NewSocket(slack.SocketConfig{
		Client: client,
		Handle: func(context.Context, slack.SlashCommand) slack.CommandAck { return slack.CommandAck{} },
		Log:    logger.ForTests(t),
	})
	if err != nil {
		t.Fatalf("NewSocket: %v", err)
	}
	err = sock.Start(t.Context())
	if err == nil {
		t.Fatal("Start must fail when the app token is refused")
	}
	if !strings.Contains(err.Error(), "not_allowed_token_type") {
		t.Errorf("error does not name Slack's reason: %v", err)
	}
}

// TestNewSocketRequiresItsPieces: the app token in particular, because the bot
// token is the plausible wrong answer and it cannot open a socket.
func TestNewSocketRequiresItsPieces(t *testing.T) {
	t.Parallel()

	withApp, err := slack.New(slack.Config{Token: testToken, AppToken: testAppToken})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	botOnly, err := slack.New(slack.Config{Token: testToken})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	handle := func(context.Context, slack.SlashCommand) slack.CommandAck { return slack.CommandAck{} }

	cases := map[string]slack.SocketConfig{
		"no client":    {Handle: handle},
		"no app token": {Client: botOnly, Handle: handle},
		"no handler":   {Client: withApp},
	}
	for name, cfg := range cases {
		if _, err := slack.NewSocket(cfg); err == nil {
			t.Errorf("NewSocket(%s) = nil error, want a refusal", name)
		}
	}
}

// TestSocketStartSurvivesATransientFailure: mx returns from Run() on a Start
// error *before* its stop block, so a service that fails to start does not stop
// the ones that did — River is never drained, an in-flight review is cut and the
// next digest slot is missed. A Slack outage of a few seconds must not cost
// that, so only a credential this app can never use fails startup; everything
// else becomes the run loop's problem.
func TestSocketStartSurvivesATransientFailure(t *testing.T) {
	t.Parallel()

	fake := newFakeSlackSocket(t)
	fake.failOpen.Store(1)

	// One attempt per dial, so the first failure is the socket's to handle rather
	// than being absorbed by the client's own retry.
	client, err := slack.New(slack.Config{
		Token: testToken, AppToken: testAppToken, BaseURL: fake.srv.URL, MaxAttempts: 1,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sock, err := slack.NewSocket(slack.SocketConfig{
		Client: client,
		Handle: func(context.Context, slack.SlashCommand) slack.CommandAck { return slack.CommandAck{} },
		Log:    logger.ForTests(t),
	})
	if err != nil {
		t.Fatalf("NewSocket: %v", err)
	}
	if err := sock.Start(t.Context()); err != nil {
		t.Fatalf("Start must survive a transient failure, got: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := sock.Stop(ctx); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})

	waitFor(t, 5*time.Second, "the socket never connected after the outage", func() bool {
		return fake.conns.Load() >= 1
	})
}

// TestSocketStopAfterAFailedStartDoesNotBlock: Start sets the cancel function
// before it dials, so a Stop after a refused token passes the nil check and then
// waits on a loop that was never started. Under mx that is latent only because a
// failed Start skips the stop block entirely — which is exactly the thing above
// no longer happens for transient failures.
func TestSocketStopAfterAFailedStartDoesNotBlock(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":false,"error":"not_allowed_token_type"}`))
	}))
	t.Cleanup(srv.Close)

	client, err := slack.New(slack.Config{Token: testToken, AppToken: testAppToken, BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sock, err := slack.NewSocket(slack.SocketConfig{
		Client: client,
		Handle: func(context.Context, slack.SlashCommand) slack.CommandAck { return slack.CommandAck{} },
		Log:    logger.ForTests(t),
	})
	if err != nil {
		t.Fatalf("NewSocket: %v", err)
	}
	if err := sock.Start(t.Context()); err == nil {
		t.Fatal("Start must fail when the app token is refused")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- sock.Stop(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Stop after a failed Start = %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Stop blocked after a failed Start")
	}
}

// waitFor polls until cond holds or the deadline passes. Polling rather than a
// channel because what is being waited on lives in the fake's counters.
func waitFor(t *testing.T, limit time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(msg)
}
