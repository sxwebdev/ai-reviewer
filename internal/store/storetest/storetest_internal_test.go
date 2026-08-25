package storetest

import (
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The isolation guarantee rests entirely on these two functions: the wrong name
// or the wrong DSN puts a truncating suite back on a shared database, where the
// damage is invisible until another package's test fails at random.

func TestSwapDatabase(t *testing.T) {
	tests := []struct {
		name    string
		dsn     string
		want    string
		wantDB  string
		wantErr bool
	}{
		{
			name:   "url form",
			dsn:    "postgres://ai_reviewer:ai_reviewer@localhost:5433/ai_reviewer?sslmode=disable",
			want:   "postgres://ai_reviewer:ai_reviewer@localhost:5433/ai_reviewer_test_store?sslmode=disable",
			wantDB: "ai_reviewer_test_store",
		},
		{
			name:   "postgresql scheme",
			dsn:    "postgresql://u:p@db:5432/app",
			want:   "postgresql://u:p@db:5432/ai_reviewer_test_store",
			wantDB: "ai_reviewer_test_store",
		},
		{
			// libpq keyword/value form: a later dbname wins, so appending is
			// enough and the original stays untouched.
			name:   "keyword value form",
			dsn:    "host=localhost port=5433 user=ai_reviewer password=ai_reviewer dbname=ai_reviewer sslmode=disable",
			want:   "host=localhost port=5433 user=ai_reviewer password=ai_reviewer dbname=ai_reviewer sslmode=disable dbname=ai_reviewer_test_store",
			wantDB: "ai_reviewer_test_store",
		},
		{
			name:    "unparseable dsn",
			dsn:     "host=localhost port=not-a-number",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := swapDatabase(tt.dsn, "ai_reviewer_test_store")
			if tt.wantErr {
				if err == nil {
					t.Fatalf("swapDatabase(%q) = %q, want an error", tt.dsn, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("swapDatabase(%q): %v", tt.dsn, err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
			// What the driver actually resolves is the only thing that matters.
			cfg, err := pgxpool.ParseConfig(got)
			if err != nil {
				t.Fatalf("ParseConfig(%q): %v", got, err)
			}
			if cfg.ConnConfig.Database != tt.wantDB {
				t.Errorf("driver resolved database %q, want %q", cfg.ConnConfig.Database, tt.wantDB)
			}
		})
	}
}

// Credentials must survive the swap untouched — a dropped password turns into a
// confusing auth failure rather than an obvious one.
func TestSwapDatabaseKeepsCredentials(t *testing.T) {
	t.Parallel()
	const dsn = "postgres://user:p%40ss%2Fword@localhost:5433/app?sslmode=require"
	got, err := swapDatabase(dsn, "ai_reviewer_test_store")
	if err != nil {
		t.Fatalf("swapDatabase: %v", err)
	}
	cfg, err := pgxpool.ParseConfig(got)
	if err != nil {
		t.Fatalf("ParseConfig(%q): %v", got, err)
	}
	if cfg.ConnConfig.User != "user" || cfg.ConnConfig.Password != "p@ss/word" {
		t.Errorf("got user=%q password=%q, want user/p@ss/word", cfg.ConnConfig.User, cfg.ConnConfig.Password)
	}
	if cfg.ConnConfig.Host != "localhost" || cfg.ConnConfig.Port != 5433 {
		t.Errorf("got %s:%d, want localhost:5433", cfg.ConnConfig.Host, cfg.ConnConfig.Port)
	}
	if cfg.ConnConfig.Database != "ai_reviewer_test_store" {
		t.Errorf("got database %q, want ai_reviewer_test_store", cfg.ConnConfig.Database)
	}
}

// These cases used to assign os.Args[0] — the same global DSN() derives this
// binary's private database from, memoised by prepareOnce. Leaving
// "/tmp/go-build123/b001/service.test" in place for the duration of a subtest
// meant that any DB-backed test running concurrently in storetest.test would
// have created, migrated and TRUNCATEd ai_reviewer_test_service, which is
// another package's database. It was safe only because of file ordering.
// databaseName takes arg0 as a parameter now, so nothing here touches process
// state and the cases can run in parallel.
func TestDatabaseName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		arg0 string
		want string
	}{
		{name: "go test binary", arg0: "/tmp/go-build123/b001/store.test", want: "ai_reviewer_test_store"},
		{name: "another package", arg0: "/tmp/go-build123/b001/service.test", want: "ai_reviewer_test_service"},
		// filepath.Base is OS-specific, so this uses a slash-separated path; the
		// .exe trim is the part under test.
		{name: "executable suffix", arg0: "/tmp/jobs.test.exe", want: "ai_reviewer_test_jobs"},
		{name: "hyphens are not identifiers", arg0: "/tmp/my-pkg.test", want: "ai_reviewer_test_my_pkg"},
		{name: "no name at all", arg0: "/tmp/.test", want: "ai_reviewer_test_unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := databaseName(tt.arg0); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}

	t.Run("stays a legal identifier length", func(t *testing.T) {
		t.Parallel()
		got := databaseName("/tmp/" + strings.Repeat("x", 200) + ".test")
		if len(got) > 63 {
			t.Errorf("got a %d-byte name, want <= 63 (Postgres limit)", len(got))
		}
		if !strings.HasPrefix(got, dbPrefix) {
			t.Errorf("got %q, want the %q prefix", got, dbPrefix)
		}
	})
}

// The two packages that share this harness must never resolve to one database —
// that is the whole bug this file exists to prevent.
func TestDatabaseNameDiffersPerPackage(t *testing.T) {
	t.Parallel()

	store := databaseName("/tmp/go-build/b001/store.test")
	service := databaseName("/tmp/go-build/b002/service.test")
	if store == service {
		t.Fatalf("store and service both resolved to %q", store)
	}
}

// TestDatabaseNameReadsNoProcessState is the guard that keeps the parameter a
// parameter: reading os.Args[0] again would restore the hazard above, and the
// symptom (another package's rows vanishing mid-test) surfaces nowhere near
// this file.
func TestDatabaseNameReadsNoProcessState(t *testing.T) {
	// Not parallel: it mutates the global exactly once, to prove it is ignored.
	original := os.Args[0]
	t.Cleanup(func() { os.Args[0] = original })

	os.Args[0] = "/tmp/go-build/b001/service.test"
	if got := databaseName("/tmp/go-build/b001/store.test"); got != "ai_reviewer_test_store" {
		t.Errorf("got %q; databaseName is reading os.Args[0] instead of its argument", got)
	}
}
