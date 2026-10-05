package gitlab

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/sxwebdev/xutils/retry"
)

// Config configures the GitLab client.
type Config struct {
	Host               string // e.g. https://gitlab.example.com
	Token              string
	Timeout            time.Duration
	InsecureSkipVerify bool
	CACertPath         string
	MaxAttempts        int

	// MaxRetryAfter bounds how long a 429's Retry-After header is honoured.
	// A hostile or misconfigured proxy can ask for hours; we cap the wait and
	// let the attempt budget run out instead of parking a worker. Default 60s.
	MaxRetryAfter time.Duration

	// Observer, when non-nil, is called once per completed HTTP attempt
	// (including each retry) with a low-cardinality endpoint template, the
	// method, the status code (0 if the request never reached a response) and
	// the error, if any.
	//
	// It is a plain func on purpose: this package must stay dependency-light
	// and must not import internal/metrics. The composition root wires the
	// counters in — see the gitlab_requests_total / gitlab_request_errors_total
	// metrics in the plan. Implementations must be cheap and goroutine-safe.
	Observer func(endpoint, method string, status int, err error)
}

// normalized fills in the defaults so every transport built from a Config
// behaves identically regardless of which constructor was used.
func (c Config) normalized() Config {
	if c.Timeout <= 0 {
		c.Timeout = 30 * time.Second
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 4
	}
	if c.MaxRetryAfter <= 0 {
		c.MaxRetryAfter = 60 * time.Second
	}
	return c
}

// Client is a GitLab API v4 client.
type Client struct {
	tr *transport
}

// transport owns everything below the endpoint layer: URL building, auth
// header, retry/backoff and observation. The REST client and the GraphQL
// client are two different base paths over the same transport.
type transport struct {
	cfg     Config
	baseURL string
	http    *http.Client
}

// APIError is a non-2xx GitLab response.
type APIError struct {
	Status int
	Body   string
	Method string
	Path   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("gitlab %s %s: status %d: %s", e.Method, e.Path, e.Status, e.Body)
}

// IsNotFound reports whether err is a 404 APIError.
func IsNotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
}

// New builds a client from cfg.
func New(cfg Config) (*Client, error) {
	tr, err := newTransport(cfg, "/api/v4")
	if err != nil {
		return nil, err
	}
	return &Client{tr: tr}, nil
}

// newHTTPClient builds the shared http.Client, honouring the custom CA and the
// explicit insecure opt-in.
func newHTTPClient(cfg Config) (*http.Client, error) {
	transport := &http.Transport{}
	if cfg.InsecureSkipVerify || cfg.CACertPath != "" {
		tlsCfg := &tls.Config{InsecureSkipVerify: cfg.InsecureSkipVerify} //nolint:gosec // explicit opt-in
		if cfg.CACertPath != "" {
			pem, err := os.ReadFile(cfg.CACertPath)
			if err != nil {
				return nil, fmt.Errorf("read ca cert: %w", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("no certificates found in %s", cfg.CACertPath)
			}
			tlsCfg.RootCAs = pool
		}
		transport.TLSClientConfig = tlsCfg
	}
	return &http.Client{Timeout: cfg.Timeout, Transport: transport}, nil
}

// newTransport builds a transport rooted at host+basePath (e.g. "/api/v4").
func newTransport(cfg Config, basePath string) (*transport, error) {
	if cfg.Host == "" {
		return nil, fmt.Errorf("gitlab host is required")
	}
	cfg = cfg.normalized()
	hc, err := newHTTPClient(cfg)
	if err != nil {
		return nil, err
	}
	return &transport{
		cfg:     cfg,
		baseURL: strings.TrimRight(cfg.Host, "/") + basePath,
		http:    hc,
	}, nil
}

// sibling returns a transport for another base path that shares this one's
// connection pool and configuration.
func (t *transport) sibling(basePath string) *transport {
	return &transport{
		cfg:     t.cfg,
		baseURL: strings.TrimRight(t.cfg.Host, "/") + basePath,
		http:    t.http,
	}
}

// rawResponse holds a completed HTTP response's essentials.
type rawResponse struct {
	status int
	header http.Header
	body   []byte
}

// doRaw performs a request with retry/backoff. Transient failures (429, 5xx,
// network) are retried; 4xx (except 429) stop immediately via retry.ErrExit.
func (t *transport) doRaw(ctx context.Context, method, path string, query url.Values, body any) (*rawResponse, error) {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return nil, fmt.Errorf("marshal body: %w", err)
		}
	}

	u := t.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	endpoint := endpointLabel(path)

	var result *rawResponse
	r := retry.New(
		retry.WithMaxAttempts(t.cfg.MaxAttempts),
		retry.WithPolicy(retry.PolicyBackoff),
		retry.WithDelay(500*time.Millisecond),
		retry.WithMaxDelay(10*time.Second),
		retry.WithContext(ctx),
	)
	err := r.Do(func() error {
		var reqBody io.Reader
		if payload != nil {
			reqBody = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, u, reqBody)
		if err != nil {
			return fmt.Errorf("%w: build request: %w", retry.ErrExit, err)
		}
		req.Header.Set("PRIVATE-TOKEN", t.cfg.Token)
		req.Header.Set("Accept", "application/json")
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := t.http.Do(req)
		if err != nil {
			t.observe(endpoint, method, 0, err)
			return fmt.Errorf("request: %w", err) // network error → retry
		}
		defer resp.Body.Close() //nolint:errcheck
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			t.observe(endpoint, method, resp.StatusCode, nil)
			result = &rawResponse{status: resp.StatusCode, header: resp.Header, body: data}
			return nil
		}
		apiErr := &APIError{Status: resp.StatusCode, Body: string(data), Method: method, Path: path}
		t.observe(endpoint, method, resp.StatusCode, apiErr)

		if resp.StatusCode == http.StatusTooManyRequests {
			// The server told us when it is willing to talk again; the fixed
			// backoff ladder (500ms…10s) is usually far shorter than that and
			// would just burn the attempt budget against a closed door. Wait
			// out the header here, inside the retry loop, so the retry
			// bookkeeping and context handling stay in one place. The loop's
			// own backoff is then added on top: overshooting a rate limit is
			// harmless, undershooting risks a longer ban.
			if d, ok := retryAfterDelay(resp.Header.Get("Retry-After"), time.Now()); ok {
				if err := sleepCtx(ctx, min(d, t.cfg.MaxRetryAfter)); err != nil {
					return fmt.Errorf("%w: %w", retry.ErrExit, err)
				}
			}
			return apiErr // retry
		}
		if resp.StatusCode >= 500 {
			return apiErr // retry
		}
		return fmt.Errorf("%w: %w", retry.ErrExit, apiErr) // 4xx → stop
	})
	if err != nil {
		// Unwrap to the APIError if present for caller inspection.
		var ae *APIError
		if errors.As(err, &ae) {
			return nil, ae
		}
		return nil, err
	}
	return result, nil
}

// do performs a request and unmarshals a 2xx JSON body into out (may be nil).
func (t *transport) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	resp, err := t.doRaw(ctx, method, path, query, body)
	if err != nil {
		return err
	}
	if out == nil || len(resp.body) == 0 {
		return nil
	}
	if err := json.Unmarshal(resp.body, out); err != nil {
		return fmt.Errorf("decode %s %s: %w", method, path, err)
	}
	return nil
}

func (t *transport) observe(endpoint, method string, status int, err error) {
	if t.cfg.Observer == nil {
		return
	}
	t.cfg.Observer(endpoint, method, status, err)
}

// endpointLabel collapses a concrete path into a low-cardinality template so it
// can be used as a metric label: /projects/42/merge_requests/7/diffs becomes
// /projects/:key/merge_requests/:id/diffs. Without this, every MR and every
// repository file would spawn its own time series.
func endpointLabel(path string) string {
	segs := strings.Split(path, "/")
	for i, s := range segs {
		if s == "" || i == 0 {
			continue
		}
		switch {
		case segs[i-1] == "projects":
			segs[i] = ":key" // numeric id or url-encoded full path
		case segs[i-1] == "files":
			segs[i] = ":path" // url-encoded repository file path
		case isAllDigits(s):
			segs[i] = ":id"
		}
	}
	return strings.Join(segs, "/")
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// retryAfterDelay parses a Retry-After header in either form RFC 9110 allows:
// delay-seconds ("120") or an HTTP-date ("Wed, 21 Oct 2015 07:28:00 GMT").
// Absent, malformed or already-elapsed values report false, which leaves the
// caller on the normal backoff ladder.
func retryAfterDelay(header string, now time.Time) (time.Duration, bool) {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(header); err == nil {
		if secs <= 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	ts, err := http.ParseTime(header)
	if err != nil {
		return 0, false
	}
	d := ts.Sub(now)
	if d <= 0 {
		return 0, false
	}
	return d, true
}

// sleepCtx waits for d, or returns the context error if it is cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
