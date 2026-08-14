package slack

import (
	"context"
	"time"
)

// SetSleepForTest replaces the Retry-After wait so tests can assert the delay
// the client honoured without spending it.
func (c *Client) SetSleepForTest(f func(context.Context, time.Duration) error) { c.sleep = f }

// SetRetryDelayForTest shrinks the transport's own backoff so retry tests run
// instantly. It does not affect the Retry-After wait.
func (c *Client) SetRetryDelayForTest(d time.Duration) { c.retryDelay = d }
