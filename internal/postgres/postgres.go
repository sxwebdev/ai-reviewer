// Package postgres owns the pgxpool that the whole service shares.
package postgres

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Postgres holds the pgxpool used by both the store and the River job queue
// (riverpgxv5). It is deliberately one pool and not two: sharing it is what lets
// a producer write its rows and enqueue a job in the same transaction
// (plan section 10.4).
type Postgres struct {
	Pool *pgxpool.Pool
}

type options struct {
	maxConns        int32
	minConns        int32
	maxConnLifetime time.Duration
	maxConnIdleTime time.Duration
	pingTimeout     time.Duration
}

type Option func(*options)

// WithMaxConns caps the pool. It has to cover the River worker pools plus the
// notifier and leader-election connections River keeps open, plus whatever the
// HTTP handlers do — sizing it below `sum(jobs.queues) + review.max_parallel + 4`
// turns a busy queue into query timeouts.
func WithMaxConns(n int32) Option { return func(o *options) { o.maxConns = n } }

// WithMinConns keeps n connections warm.
func WithMinConns(n int32) Option { return func(o *options) { o.minConns = n } }

// WithMaxConnLifetime recycles connections after d, so a rolling Postgres
// upgrade or a pgbouncer restart does not pin us to dead backends.
func WithMaxConnLifetime(d time.Duration) Option {
	return func(o *options) { o.maxConnLifetime = d }
}

// WithMaxConnIdleTime closes idle connections after d.
func WithMaxConnIdleTime(d time.Duration) Option {
	return func(o *options) { o.maxConnIdleTime = d }
}

// WithPingTimeout bounds the connectivity check New performs before returning.
func WithPingTimeout(d time.Duration) Option {
	return func(o *options) { o.pingTimeout = d }
}

func defaultOptions() options {
	return options{
		maxConns:        20,
		minConns:        2,
		maxConnLifetime: time.Hour,
		maxConnIdleTime: 30 * time.Minute,
		pingTimeout:     10 * time.Second,
	}
}

// New parses dsn, opens the pool and verifies connectivity. Failing here rather
// than on the first query is deliberate: a service that cannot reach Postgres
// has no state, no queue and no scheduler, so it must not report itself started.
func New(ctx context.Context, dsn string, opts ...Option) (*Postgres, error) {
	o := defaultOptions()
	for _, opt := range opts {
		opt(&o)
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to parse postgres dsn: %w", err)
	}
	cfg.MaxConns = o.maxConns
	cfg.MinConns = o.minConns
	cfg.MaxConnLifetime = o.maxConnLifetime
	cfg.MaxConnIdleTime = o.maxConnIdleTime

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create postgres pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, o.pingTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("failed to ping postgres: %w", err)
	}

	return &Postgres{Pool: pool}, nil
}

// Name implements the mx service contract.
func (p *Postgres) Name() string { return "postgres" }

// Start re-pings: New already connected, so this only re-asserts liveness at the
// point the launcher considers the service up.
func (p *Postgres) Start(ctx context.Context) error {
	return p.Pool.Ping(ctx)
}

// Stop closes the pool. Register Postgres so it stops *after* the River client
// has drained — outstanding jobs still need connections to record their result.
func (p *Postgres) Stop(_ context.Context) error {
	p.Pool.Close()
	return nil
}

// Interval and Healthy make *Postgres an mx health checker as well as a service,
// so /health reflects real connectivity rather than "the process is alive".
func (p *Postgres) Interval() time.Duration { return 30 * time.Second }

func (p *Postgres) Healthy(ctx context.Context) error { return p.Pool.Ping(ctx) }

// DSNParams are the connection fields as they appear in config.yaml
// (plan section 7.2).
type DSNParams struct {
	Host     string
	Port     string
	Database string
	Username string
	Password string
	SSLMode  string
}

// BuildDSN assembles a libpq URL. It exists so the password is escaped by
// net/url exactly once: a password with an `@` or `/` in it silently produces a
// DSN pointing at the wrong host when built with fmt.Sprintf.
func BuildDSN(p DSNParams) string {
	u := &url.URL{
		Scheme: "postgres",
		Host:   net.JoinHostPort(p.Host, p.Port),
		Path:   "/" + p.Database,
	}
	if p.Username != "" {
		u.User = url.UserPassword(p.Username, p.Password)
	}
	if p.SSLMode != "" {
		u.RawQuery = url.Values{"sslmode": []string{p.SSLMode}}.Encode()
	}
	return u.String()
}
