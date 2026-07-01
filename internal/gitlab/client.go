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
}

// Client is a GitLab API v4 client.
type Client struct {
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
	if cfg.Host == "" {
		return nil, fmt.Errorf("gitlab host is required")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 4
	}
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
	return &Client{
		cfg:     cfg,
		baseURL: strings.TrimRight(cfg.Host, "/") + "/api/v4",
		http:    &http.Client{Timeout: cfg.Timeout, Transport: transport},
	}, nil
}

// rawResponse holds a completed HTTP response's essentials.
type rawResponse struct {
	status int
	header http.Header
	body   []byte
}

// doRaw performs a request with retry/backoff. Transient failures (429, 5xx,
// network) are retried; 4xx (except 429) stop immediately via retry.ErrExit.
func (c *Client) doRaw(ctx context.Context, method, path string, query url.Values, body any) (*rawResponse, error) {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return nil, fmt.Errorf("marshal body: %w", err)
		}
	}

	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	var result *rawResponse
	r := retry.New(
		retry.WithMaxAttempts(c.cfg.MaxAttempts),
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
		req.Header.Set("PRIVATE-TOKEN", c.cfg.Token)
		req.Header.Set("Accept", "application/json")
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.http.Do(req)
		if err != nil {
			return fmt.Errorf("request: %w", err) // network error → retry
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			result = &rawResponse{status: resp.StatusCode, header: resp.Header, body: data}
			return nil
		}
		apiErr := &APIError{Status: resp.StatusCode, Body: string(data), Method: method, Path: path}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
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
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	resp, err := c.doRaw(ctx, method, path, query, body)
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
