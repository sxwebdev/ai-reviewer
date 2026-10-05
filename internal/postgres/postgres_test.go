package postgres_test

import (
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sxwebdev/ai-reviewer/internal/postgres"
)

const dsnEnv = "AI_REVIEWER_TEST_PG_DSN"

func TestBuildDSN(t *testing.T) {
	tests := []struct {
		name string
		in   postgres.DSNParams
		want string
	}{
		{
			name: "full",
			in: postgres.DSNParams{
				Host: "db.internal", Port: "5432", Database: "ai_reviewer",
				Username: "ai_reviewer", Password: "s3cret", SSLMode: "require",
			},
			want: "postgres://ai_reviewer:s3cret@db.internal:5432/ai_reviewer?sslmode=require",
		},
		{
			// The reason this helper exists: fmt.Sprintf would produce a DSN
			// pointing at host "example.com" here.
			name: "password with an at sign is escaped",
			in: postgres.DSNParams{
				Host: "localhost", Port: "5433", Database: "ai_reviewer",
				Username: "ai_reviewer", Password: "p@ss/word", SSLMode: "disable",
			},
			want: "postgres://ai_reviewer:p%40ss%2Fword@localhost:5433/ai_reviewer?sslmode=disable",
		},
		{
			name: "no credentials, no ssl mode",
			in:   postgres.DSNParams{Host: "localhost", Port: "5432", Database: "ai_reviewer"},
			want: "postgres://localhost:5432/ai_reviewer",
		},
		{
			name: "ipv6 host is bracketed",
			in: postgres.DSNParams{
				Host: "::1", Port: "5432", Database: "ai_reviewer", SSLMode: "disable",
			},
			want: "postgres://[::1]:5432/ai_reviewer?sslmode=disable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := postgres.BuildDSN(tt.in)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
			// Whatever we build must be parseable by the driver that consumes it.
			if _, err := pgxpool.ParseConfig(got); err != nil {
				t.Errorf("pgxpool cannot parse %q: %v", got, err)
			}
		})
	}
}

func TestNewRejectsAMalformedDSN(t *testing.T) {
	t.Parallel()
	if _, err := postgres.New(t.Context(), "://nope"); err == nil {
		t.Fatal("New with a malformed DSN succeeded, want an error")
	}
}

// The mx launcher discovers Name/Start/Stop (and Interval/Healthy) structurally,
// so a rename here silently drops Postgres out of the lifecycle. Pin the shape.
func TestServiceLifecycle(t *testing.T) {
	dsn := os.Getenv(dsnEnv)
	if dsn == "" {
		t.Skipf("set %s to run the lifecycle test", dsnEnv)
	}

	pg, err := postgres.New(t.Context(), dsn,
		postgres.WithMaxConns(7),
		postgres.WithMinConns(1),
		postgres.WithMaxConnLifetime(42*time.Minute),
		postgres.WithMaxConnIdleTime(11*time.Minute),
		postgres.WithPingTimeout(5*time.Second),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	cfg := pg.Pool.Config()
	if cfg.MaxConns != 7 || cfg.MinConns != 1 {
		t.Errorf("got MaxConns=%d MinConns=%d, want 7/1", cfg.MaxConns, cfg.MinConns)
	}
	if cfg.MaxConnLifetime != 42*time.Minute || cfg.MaxConnIdleTime != 11*time.Minute {
		t.Errorf("got lifetime=%v idle=%v, want 42m/11m", cfg.MaxConnLifetime, cfg.MaxConnIdleTime)
	}

	if pg.Name() != "postgres" {
		t.Errorf("got name %q, want postgres", pg.Name())
	}
	if err := pg.Start(t.Context()); err != nil {
		t.Errorf("Start: %v", err)
	}
	if err := pg.Healthy(t.Context()); err != nil {
		t.Errorf("Healthy: %v", err)
	}
	if pg.Interval() <= 0 {
		t.Errorf("got health interval %v, want a positive duration", pg.Interval())
	}
	if err := pg.Stop(t.Context()); err != nil {
		t.Errorf("Stop: %v", err)
	}
	// After Stop the pool is closed, so health must now report a failure rather
	// than keeping a dead service marked ready.
	if err := pg.Healthy(t.Context()); err == nil {
		t.Error("Healthy succeeded after Stop")
	}
}

// A service that cannot reach Postgres has no state, no queue and no scheduler,
// so New must fail rather than hand back a pool that dies on first use.
func TestNewFailsWhenUnreachable(t *testing.T) {
	t.Parallel()
	// Port 1 is reserved and never listening.
	_, err := postgres.New(t.Context(),
		"postgres://u:p@127.0.0.1:1/db?sslmode=disable",
		postgres.WithPingTimeout(2*time.Second),
		postgres.WithMinConns(0),
	)
	if err == nil {
		t.Fatal("New against a dead address succeeded, want an error")
	}
}
