package slack

import (
	"context"
	"net"
	"time"
)

// SetSleepForTest replaces the Retry-After wait so tests can assert the delay
// the client honoured without spending it.
func (c *Client) SetSleepForTest(f func(context.Context, time.Duration) error) { c.sleep = f }

// SetRetryDelayForTest shrinks the transport's own backoff so retry tests run
// instantly. It does not affect the Retry-After wait.
func (c *Client) SetRetryDelayForTest(d time.Duration) { c.retryDelay = d }

// AllowResponseHostForTest points Respond at a local test server.
//
// The host pin is a production rule with no configuration knob on purpose — a
// deployment has no reason to POST a command answer anywhere but Slack — so the
// only way to exercise the request itself is from inside the package.
func (c *Client) AllowResponseHostForTest(hostport string) {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	c.responseHost = host
	c.insecureResponse = true
}
