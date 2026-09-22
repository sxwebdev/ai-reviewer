package store_test

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sxwebdev/ai-reviewer/internal/dbtypes"
	"github.com/sxwebdev/ai-reviewer/internal/store/repos/repo_digestmessage"
	"github.com/sxwebdev/ai-reviewer/internal/store/repos/repo_digestrun"
	"github.com/sxwebdev/ai-reviewer/internal/store/storetest"
)

// The status columns carry no CHECK constraint, so nothing in Postgres rejects a
// bad value: a typo is stored happily, and for digest_messages that is a digest
// silently dropped (ClaimForSend reads any unknown status as "already
// delivered"). The application is therefore the only guard, and a status reaches
// the database by exactly two routes. These two tests cover both, so "every path
// is validated" is a fact the suite re-checks rather than a claim.
//
//   - a Go value bound as a query parameter → the four store wrappers, covered
//     by TestWrappersRejectInvalidStatus (and by TestNoDirectStatusWrites, which
//     is what stops a fifth route appearing);
//   - a literal written into a .sql file → TestSQLStatusLiterals.

var (
	statusLiteralRe = regexp.MustCompile(`(?i)\bstatus\s*(?:=|<>|!=|\bIN\b)\s*(\([^)]*\)|'[a-z_]*')`)
	quotedRe        = regexp.MustCompile(`'([a-z_]*)'`)
)

// TestSQLStatusLiterals is what replaces the CHECK constraint for statuses that
// are fixed when a query is authored. It reads every .sql file and asserts each
// literal compared against a status column is a value some status type declares.
//
// The allowed set is derived from the tables the file mentions rather than from
// its directory: SettleDelivery lives beside the digest_runs queries but counts
// digest_messages rows, so a per-directory mapping would have to special-case it
// and would rot the first time another query joined across tables.
func TestSQLStatusLiterals(t *testing.T) {
	root := filepath.Join("..", "..", "sql")

	var files []string
	if err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".sql") {
			files = append(files, path)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if len(files) == 0 {
		t.Fatalf("found no .sql files under %s — the guard would pass vacuously", root)
	}

	var checked int
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		body := string(raw)

		var allowed []string
		if strings.Contains(body, "mr_reviews") {
			allowed = append(allowed, strs(dbtypes.ReviewStatuses())...)
		}
		if strings.Contains(body, "digest_runs") {
			allowed = append(allowed, strs(dbtypes.DigestRunStatuses())...)
		}
		if strings.Contains(body, "digest_messages") {
			allowed = append(allowed, strs(dbtypes.MessageStatuses())...)
		}

		for _, m := range statusLiteralRe.FindAllStringSubmatch(body, -1) {
			for _, lit := range quotedRe.FindAllStringSubmatch(m[1], -1) {
				value := lit[1]
				checked++
				if len(allowed) == 0 {
					t.Errorf("%s: compares a status against %q but names none of the status tables — the guard cannot tell which set applies", path, value)
					continue
				}
				if !slices.Contains(allowed, value) {
					t.Errorf("%s: status literal %q is not a value any status type declares (allowed here: %v)", path, value, allowed)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("matched no status literals at all — the regex has stopped matching and this guard is dead")
	}
	t.Logf("checked %d status literals across %d .sql files", checked, len(files))
}

// TestNoDirectStatusWrites keeps the four wrappers the only parameterised way in.
// Without it the guard is a convention: the generated Queries are reachable from
// anywhere holding a *store.Store, and calling Create directly would skip
// validation while still compiling and still passing every behavioural test.
func TestNoDirectStatusWrites(t *testing.T) {
	root := filepath.Join("..", "..")
	// Only these repo packages have a status column; a file that does not import
	// one cannot call the generated writers we care about.
	repoImport := regexp.MustCompile(`internal/store/repos/repo_(review|digestrun|digestmessage)"`)
	writeCall := regexp.MustCompile(`\.(Create|SetStatus)\(ctx\b`)

	var scanned, offenders int
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "bin", "node_modules":
				return filepath.SkipDir
			}
			// internal/store owns the wrappers; the generated repos live under it.
			if path == filepath.Join(root, "internal", "store") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		body := string(raw)
		if !repoImport.MatchString(body) {
			return nil
		}
		scanned++
		for line := range strings.SplitSeq(body, "\n") {
			if writeCall.MatchString(line) {
				offenders++
				t.Errorf("%s: %s\n\tcalls a generated writer directly; route status writes through the store wrappers (CreateReview, CreateDigestRun, SetDigestRunStatus, CreateDigestMessage) so the value is validated",
					path, strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if scanned == 0 {
		t.Fatal("no file outside internal/store imports a status-bearing repo package — the guard is dead and would not notice a new bypass")
	}
	t.Logf("scanned %d files importing a status-bearing repo package, %d offenders", scanned, offenders)
}

func strs[T ~string](in []T) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = string(v)
	}
	return out
}

// TestWrappersRejectInvalidStatus is the run-time half of the guard: with no
// CHECK in the schema, a bad status is rejected here or not at all. It drives
// each wrapper with a value that is plausible-looking but wrong — the shape of
// mistake that actually happens — and then asserts nothing was written, because
// an error that still inserts the row is worse than no error at all.
func TestWrappersRejectInvalidStatus(t *testing.T) {
	st, pool := storetest.Store(t)
	ctx := t.Context()

	run := createDigestRun(t, st, "09:00", 0)

	cases := []struct {
		name  string
		table string
		bad   string
		call  func(bad string) error
	}{
		{
			name:  "CreateReview",
			table: "mr_reviews",
			bad:   "reviewd", // a typo of a real value
			call: func(bad string) error {
				_, err := st.CreateReview(ctx, reviewParams(bad))
				return err
			},
		},
		{
			name:  "CreateDigestRun",
			table: "digest_runs",
			bad:   "pending", // valid for a message, not for a run
			call: func(bad string) error {
				_, err := st.CreateDigestRun(ctx, repo_digestrun.CreateParams{
					Team:    "core",
					Slot:    "23:59",
					RunDate: time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC),
					Status:  bad,
				})
				return err
			},
		},
		{
			name:  "SetDigestRunStatus",
			table: "digest_runs",
			bad:   "sending", // valid for a message, not for a run
			call: func(bad string) error {
				return st.SetDigestRunStatus(ctx, repo_digestrun.SetStatusParams{
					ID: run.ID, Status: bad, Parts: 1, MrCount: 1,
				})
			},
		},
		{
			name:  "CreateDigestMessage",
			table: "digest_messages",
			bad:   "built", // valid for a run, not for a message
			call: func(bad string) error {
				_, err := st.CreateDigestMessage(ctx, repo_digestmessage.CreateParams{
					DigestRunID: run.ID,
					PartNo:      7,
					PartsTotal:  1,
					Channel:     "#reviews",
					Payload:     dbtypes.JSON(`{"blocks":[]}`),
					Status:      bad,
				})
				return err
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := countStatus(t, pool, tc.table, tc.bad)

			err := tc.call(tc.bad)
			if err == nil {
				t.Fatalf("%s accepted status %q — with no CHECK constraint this reaches the database", tc.name, tc.bad)
			}
			if !strings.Contains(err.Error(), tc.bad) {
				t.Errorf("error does not name the offending value %q: %v", tc.bad, err)
			}

			if after := countStatus(t, pool, tc.table, tc.bad); after != before {
				t.Fatalf("%s returned an error but still wrote to %s: rows with status=%q went %d -> %d",
					tc.name, tc.table, tc.bad, before, after)
			}
		})
	}

	// The counterweight: every declared value must be accepted, or the guard is
	// just a way to break the product.
	for _, s := range dbtypes.ReviewStatuses() {
		p := reviewParams(string(s))
		p.HeadSha = strings.Repeat("b", 39) + string(s[0])
		if _, err := st.CreateReview(ctx, p); err != nil {
			t.Errorf("CreateReview rejected the declared status %q: %v", s, err)
		}
	}
}

func countStatus(t *testing.T, pool interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}, table, status string,
) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM `+table+` WHERE status = $1`, status).Scan(&n); err != nil {
		t.Fatalf("count %s rows with status=%q: %v", table, status, err)
	}
	return n
}
