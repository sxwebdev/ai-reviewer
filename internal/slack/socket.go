package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/tkcrm/mx/logger"
)

// Socket Mode is how this service receives slash commands.
//
// The alternative — Slack POSTing to a Request URL — needs the service to be
// reachable from the internet: an ingress, a certificate and a public endpoint
// in front of a process that holds a GitLab PAT, a Slack token and the database
// password. Socket Mode inverts that: the service dials *out* over a WebSocket
// authenticated by the app-level token, so nothing has to be exposed and there
// is no request signature to verify, because there is no unauthenticated
// request. The cost is a connection to keep alive, which is what this file is.

// slashCommandsType is the only envelope type this service acts on. Slack sends
// several others over the same socket (events_api, interactive,
// block_suggestion) and they are acknowledged and dropped: acknowledging is not
// optional — an envelope left unacknowledged is redelivered.
const (
	socketTypeHello         = "hello"
	socketTypeDisconnect    = "disconnect"
	socketTypeSlashCommands = "slash_commands"
	// socketReasonLinkDisabled is the one disconnect reason that will not fix
	// itself. The other two — "warning" and "refresh_requested" — are Slack
	// recycling a connection every few hours and mean "dial again".
	socketReasonLinkDisabled = "link_disabled"
)

// Socket-loop timings.
const (
	// socketPingEvery is how often the connection is proved alive. Slack sends
	// its own pings and the library answers them transparently, which is exactly
	// why this exists: on a connection that is dead in a way TCP has not noticed
	// — a NAT dropping an idle mapping is the ordinary case — a read blocks
	// forever and commands stop working with nothing in the log saying so.
	socketPingEvery = 30 * time.Second
	// socketPingTimeout bounds one ping's wait for its pong.
	socketPingTimeout = 10 * time.Second
	// socketReadLimit caps one inbound frame. Slack's envelopes are a few KB; a
	// megabyte is generous and stops a malformed stream from allocating freely.
	socketReadLimit = 1 << 20
	// socketAckTimeout bounds writing one acknowledgement.
	socketAckTimeout = 5 * time.Second
	// socketRetryMin / socketRetryMax bound the reconnect backoff after a
	// failure. A clean refresh does not use them: Slack asked for the reconnect,
	// so waiting before obeying only widens the window in which a command is
	// lost.
	socketRetryMin = time.Second
	socketRetryMax = 30 * time.Second
)

// SlashCommand is one slash command as Slack delivers it.
type SlashCommand struct {
	// Command is the command as typed, leading slash included: "/all".
	Command string `json:"command"`
	// Text is everything after the command, already trimmed by Slack.
	Text string `json:"text"`
	// ChannelID is where it was typed — a channel, a group or a DM. It is what
	// resolves the team, so a command typed outside a team's channel can be
	// refused by name rather than answered with somebody else's digest.
	ChannelID   string `json:"channel_id"`
	ChannelName string `json:"channel_name"`
	// UserID is the caller. It is the whole input of the personal digest: the
	// digest already carries the Slack id of every person it names, so "mine"
	// is a filter over the same data rather than a second matching pass.
	UserID   string `json:"user_id"`
	UserName string `json:"user_name"`
	TeamID   string `json:"team_id"`
	// ResponseURL is a capability: whoever holds it can post into that
	// conversation as this app for 30 minutes. It is never logged. It travels
	// through the job args, which is a database column, and that is the whole
	// reason the socket acknowledges immediately and the work happens on the
	// queue — see MaxCommandResponses for the budget it comes with.
	ResponseURL string `json:"response_url"`
	TriggerID   string `json:"trigger_id"`
}

// CommandAck is the immediate answer to a slash command, delivered inside the
// envelope acknowledgement itself.
//
// It costs no extra HTTP call and it is the only response guaranteed to arrive
// within Slack's three-second window, so it carries what the caller needs to
// know right now — "on it" or "this channel is not a team's" — while the digest
// itself arrives later through the response URL.
type CommandAck struct {
	Text string
	// InChannel posts the acknowledgement to everyone rather than only to the
	// caller. Off for every ack this service sends: an "on it" line visible to
	// the channel is noise, and the digest that follows is the message worth
	// seeing.
	InChannel bool
}

// CommandFunc handles one slash command.
//
// It must return quickly: Slack expects an acknowledgement within three
// seconds, and the digest behind these commands takes dozens of GitLab requests.
// Implementations enqueue and return.
type CommandFunc func(ctx context.Context, cmd SlashCommand) CommandAck

// SocketConfig configures the Socket Mode listener.
type SocketConfig struct {
	// Client opens the connection (apps.connections.open) and must therefore
	// carry an app-level token.
	Client *Client
	// Handle receives every slash command. Required.
	Handle CommandFunc
	Log    logger.Logger
}

// Socket is the Socket Mode listener: an mx service (Name/Start/Stop) that
// keeps one WebSocket to Slack open and hands every slash command to Handle.
//
// One connection per replica, which is exactly right: Slack load-balances
// payloads across an app's open connections and allows up to ten, so N replicas
// share the traffic and any one of them can answer. It is deliberately not
// leader-only — a listener that lived on the leader would go deaf for the
// length of every leadership handover.
type Socket struct {
	cfg  SocketConfig
	log  logger.Logger
	done chan struct{}
	// stop cancels the run loop. Set by Start, called by Stop.
	stop context.CancelFunc
}

// NewSocket builds the listener. It does not connect; Start does.
func NewSocket(cfg SocketConfig) (*Socket, error) {
	if cfg.Client == nil {
		return nil, errors.New("slack socket: client is required")
	}
	if strings.TrimSpace(cfg.Client.cfg.AppToken) == "" {
		return nil, errors.New("slack socket: an app-level token (xapp-…) is required")
	}
	if cfg.Handle == nil {
		return nil, errors.New("slack socket: handler is required")
	}
	log := cfg.Log
	if log == nil {
		log = logger.Default()
	}
	return &Socket{cfg: cfg, log: log, done: make(chan struct{})}, nil
}

// Name identifies the service to the mx launcher.
func (s *Socket) Name() string { return "slack-socket" }

// Start connects and returns; the connection is maintained by a goroutine that
// lives until Stop.
//
// A goroutine rather than a River job, and the distinction is the one §6.2
// draws: a job is a unit of deferred *work*, which must survive a crash and be
// retried. This is a transport, like the database pool or the River client
// itself — it owns no work, and what it receives it hands to the queue within
// milliseconds. What it must not do is die quietly, which is why it is a
// registered service with a lifecycle rather than a `go` in a constructor.
func (s *Socket) Start(ctx context.Context) error {
	// Detached from the caller's context on purpose, the same way the jobs
	// service is: mx cancels the start context once every service is up.
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.stop = cancel

	// The first connection is opened synchronously so a token this app can never
	// use fails startup with the reason, instead of becoming a warning in a loop
	// that nobody reads until somebody asks why /all is silent.
	//
	// Dialled on the *caller's* context, so a startup that is being aborted does
	// not have to wait out a handshake. That is safe because the connection does
	// not belong to that context once the handshake is done — verified against
	// coder/websocket v1.8.15 and Go's transport: after the 101 the socket is
	// hijacked, and cancelling the dial context leaves reads working.
	conn, err := s.dial(ctx)
	switch {
	case err == nil:
	case ctx.Err() != nil:
		// Startup itself is being abandoned; there is nothing to keep alive.
		cancel()
		close(s.done)
		return err
	case permanentSocketError(err):
		cancel()
		// Stop must not block on a loop that was never started. Closing here is
		// what makes the failed-Start path finish Stop immediately instead of
		// hanging on <-s.done until the shutdown context expires.
		close(s.done)
		return err
	default:
		// Only a credential this app cannot fix is worth failing startup with,
		// and the reason is what mx does with the error: on a Start failure it
		// returns from Run() *before* its stop block, so no service is stopped —
		// River is never drained, an in-flight review is cut mid-pass and the
		// next digest slot is simply missed. A DNS blip or one 5xx from Slack
		// must not cost that. The run loop dials the connection with the same
		// backoff a mid-life disconnect gets, and commands start working when
		// Slack does.
		s.log.Warnw("slack socket: first connection failed, retrying in the background",
			"retry_in", socketRetryMin, "err", err)
	}

	go func() {
		defer close(s.done)
		s.run(runCtx, conn)
	}()
	return nil
}

// permanentSocketAuthCodes are the apps.connections.open failures no retry can
// fix: the app-level token is wrong, revoked, or not a token that may open a
// socket at all (a bot token answers not_allowed_token_type — the likely
// mistake, which is why `doctor` names it too).
//
// The list is deliberately an allowlist rather than "everything that is not
// retryable": an unfamiliar code means Slack said something this service has not
// seen, and guessing "permanent" there exits the whole process. Guessing
// "transient" only keeps a warning in the log.
var permanentSocketAuthCodes = map[string]bool{
	"invalid_auth":           true,
	"not_authed":             true,
	"not_allowed_token_type": true,
	"account_inactive":       true,
	"token_revoked":          true,
	"token_expired":          true,
	"missing_scope":          true,
	"no_permission":          true,
}

// permanentSocketError reports whether a failed dial is a configuration problem
// rather than Slack being briefly unreachable.
func permanentSocketError(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		// A transport failure: DNS, a refused connection, a handshake that timed
		// out. None of those say anything about the token.
		return false
	}
	// Slack answers apps.connections.open with HTTP 200 and ok:false, so the code
	// is the usual signal; 401/403 covers a gateway answering before Slack does.
	return permanentSocketAuthCodes[apiErr.Code] ||
		apiErr.Status == http.StatusUnauthorized ||
		apiErr.Status == http.StatusForbidden
}

// Stop closes the connection and waits for the loop to finish.
func (s *Socket) Stop(ctx context.Context) error {
	if s.stop == nil {
		return nil
	}
	s.stop()
	select {
	case <-s.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// run keeps a connection open until ctx is done, reconnecting as needed.
func (s *Socket) run(ctx context.Context, conn *websocket.Conn) {
	backoff := socketRetryMin
	for {
		if conn == nil {
			var err error
			conn, err = s.dial(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				s.log.Warnw("slack socket: reconnect failed", "retry_in", backoff, "err", err)
				if !sleepUntil(ctx, backoff) {
					return
				}
				backoff = min(backoff*2, socketRetryMax)
				continue
			}
			backoff = socketRetryMin
		}

		err := s.serve(ctx, conn)
		_ = conn.CloseNow()
		conn = nil

		switch {
		case ctx.Err() != nil:
			return
		case err == nil:
			// Slack asked for the reconnect (a refresh every few hours, or the
			// ten-second warning before one). Reconnecting immediately is the point:
			// a command that lands in the gap is not retried by Slack — the caller
			// sees a timeout — so the gap is kept to one dial rather than a backoff.
			s.log.Debugw("slack socket: reconnecting at Slack's request")
		default:
			s.log.Warnw("slack socket: connection lost", "retry_in", backoff, "err", err)
			if !sleepUntil(ctx, backoff) {
				return
			}
			backoff = min(backoff*2, socketRetryMax)
		}
	}
}

// dial opens a fresh WebSocket. Each URL apps.connections.open returns is a
// one-shot ticket, so every reconnect asks for a new one.
func (s *Socket) dial(ctx context.Context) (*websocket.Conn, error) {
	url, err := s.cfg.Client.OpenSocketURL(ctx)
	if err != nil {
		return nil, fmt.Errorf("open slack socket connection: %w", err)
	}
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPClient: &http.Client{Timeout: s.cfg.Client.cfg.Timeout},
	})
	if err != nil {
		// The URL carries a single-use ticket; it must not reach a log.
		return nil, fmt.Errorf("dial slack socket: %w", err)
	}
	conn.SetReadLimit(socketReadLimit)
	s.log.Infow("slack socket: connected")
	return conn, nil
}

// serve reads one connection until Slack asks for a reconnect (nil), the
// connection fails (error) or ctx is done.
func (s *Socket) serve(ctx context.Context, conn *websocket.Conn) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// The heartbeat cancels the read below by cancelling their shared context,
	// which is what turns a silently dead socket into a reconnect.
	go s.heartbeat(ctx, conn, cancel)

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
				return ctx.Err()
			}
			return err
		}

		var env socketEnvelope
		if err := json.Unmarshal(data, &env); err != nil {
			// A frame we cannot parse is not a reason to drop a working
			// connection, and it carries no envelope id to acknowledge.
			s.log.Warnw("slack socket: unreadable envelope", "err", err)
			continue
		}

		switch env.Type {
		case socketTypeHello:
			s.log.Debugw("slack socket: hello")
			continue
		case socketTypeDisconnect:
			s.log.Infow("slack socket: Slack asked to reconnect", "reason", env.Reason)
			if env.Reason == socketReasonLinkDisabled {
				// Socket Mode was switched off in the app's settings. Reconnecting
				// would fail every time; say so once, in words an operator can act on,
				// and let the backoff do the rest.
				return fmt.Errorf("slack socket: Socket Mode is disabled for this app (reason %q)", env.Reason)
			}
			return nil
		}

		s.dispatch(ctx, conn, env)
	}
}

// dispatch acknowledges one envelope and, when it is a slash command, hands it
// to the handler.
//
// The acknowledgement is what the handler's answer rides on, so the two cannot
// be reordered: Slack accepts exactly one ack per envelope, and the ack is the
// only response that reliably beats the three-second deadline.
func (s *Socket) dispatch(ctx context.Context, conn *websocket.Conn, env socketEnvelope) {
	if env.EnvelopeID == "" {
		return
	}
	ack := socketAck{EnvelopeID: env.EnvelopeID}

	if env.Type == socketTypeSlashCommands {
		var cmd SlashCommand
		if err := json.Unmarshal(env.Payload, &cmd); err != nil {
			s.log.Warnw("slack socket: unreadable slash command payload", "err", err)
			ack.Payload = &ResponseMessage{Text: "Slack sent a command this service could not read."}
		} else {
			answer := s.cfg.Handle(ctx, cmd)
			if answer.Text != "" {
				payload := &ResponseMessage{Text: answer.Text}
				if answer.InChannel {
					payload.ResponseType = ResponseInChannel
				}
				ack.Payload = payload
			}
		}
	}

	body, err := json.Marshal(ack)
	if err != nil {
		// Unreachable: the ack is two strings and a struct of strings.
		s.log.Errorw("slack socket: could not build acknowledgement", "err", err)
		return
	}
	writeCtx, cancel := context.WithTimeout(ctx, socketAckTimeout)
	defer cancel()
	if err := conn.Write(writeCtx, websocket.MessageText, body); err != nil {
		// The envelope will be redelivered, on this connection or another.
		s.log.Warnw("slack socket: acknowledgement failed", "type", env.Type, "err", err)
	}
}

// heartbeat proves the connection is alive and cancels ctx when it is not.
func (s *Socket) heartbeat(ctx context.Context, conn *websocket.Conn, cancel context.CancelFunc) {
	t := time.NewTicker(socketPingEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pingCtx, pingCancel := context.WithTimeout(ctx, socketPingTimeout)
			err := conn.Ping(pingCtx)
			pingCancel()
			if err != nil {
				if ctx.Err() == nil {
					s.log.Warnw("slack socket: ping failed; reconnecting", "err", err)
				}
				cancel()
				return
			}
		}
	}
}

// socketEnvelope is the wrapper every Socket Mode message arrives in.
type socketEnvelope struct {
	Type string `json:"type"`
	// EnvelopeID is absent on hello and disconnect, which are not acknowledged.
	EnvelopeID string          `json:"envelope_id"`
	Payload    json.RawMessage `json:"payload"`
	// Reason is set on disconnect: warning, refresh_requested or link_disabled.
	Reason string `json:"reason"`
}

// socketAck is the acknowledgement, optionally carrying the immediate response.
type socketAck struct {
	EnvelopeID string `json:"envelope_id"`
	Payload    any    `json:"payload,omitempty"`
}

// sleepUntil waits for d and reports whether the wait completed rather than
// being cut short by ctx.
func sleepUntil(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// OpenSocketURL calls apps.connections.open and returns the one-shot WebSocket
// URL to dial.
//
// It is the only method that takes the app-level token, and the only one that
// token can call: a bot token here answers not_allowed_token_type.
func (c *Client) OpenSocketURL(ctx context.Context) (string, error) {
	token := strings.TrimSpace(c.cfg.AppToken)
	if token == "" {
		return "", errors.New("slack: an app-level token is required to open a Socket Mode connection")
	}
	var resp struct {
		URL string `json:"url"`
	}
	if err := c.call(ctx, http.MethodPost, "apps.connections.open", token, nil, nil, &resp); err != nil {
		return "", err
	}
	if strings.TrimSpace(resp.URL) == "" {
		return "", errors.New("apps.connections.open returned no URL")
	}
	return resp.URL, nil
}
