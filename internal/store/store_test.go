package store_test

import (
	"encoding/binary"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sxwebdev/ai-reviewer/internal/store"
	"github.com/sxwebdev/ai-reviewer/internal/store/storetest"
)

var errRollback = errors.New("rollback")

// Plan section 10.4: the review row, its findings and the publish_review job go
// in as one commit. RunInTx hands out the pgx.Tx that river.Client.InsertTx
// wants, so a rollback must leave nothing behind — not a review, not a finding,
// not a job.
func TestRunInTxRollsBackEverything(t *testing.T) {
	st, _ := storetest.Store(t)

	err := st.RunInTx(t.Context(), func(tx pgx.Tx) error {
		rev, err := st.Review(store.WithTx(tx)).Create(t.Context(), reviewParams("reviewed"))
		if err != nil {
			return err
		}
		if _, err := st.Finding(store.WithTx(tx)).Insert(t.Context(), findingParams(rev.ID, "fp-1")); err != nil {
			return err
		}
		// Stand-in for river.Client.InsertTx: the callback gets the raw pgx.Tx,
		// so anything the queue driver does joins the same commit.
		if _, err := tx.Exec(t.Context(), `SELECT 1`); err != nil {
			return err
		}
		return errRollback
	})
	if !errors.Is(err, errRollback) {
		t.Fatalf("RunInTx: got err %v, want errRollback", err)
	}

	var reviews, findings int64
	if err := st.Pool().QueryRow(t.Context(),
		`SELECT (SELECT count(*) FROM mr_reviews), (SELECT count(*) FROM mr_findings)`,
	).Scan(&reviews, &findings); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if reviews != 0 || findings != 0 {
		t.Errorf("after rollback: %d reviews and %d findings survived, want 0/0", reviews, findings)
	}
}

func TestRunInTxCommitsEverything(t *testing.T) {
	st, _ := storetest.Store(t)

	var reviewID uuid.UUID
	err := st.RunInTx(t.Context(), func(tx pgx.Tx) error {
		rev, err := st.Review(store.WithTx(tx)).Create(t.Context(), reviewParams("reviewed"))
		if err != nil {
			return err
		}
		reviewID = rev.ID
		_, err = st.Finding(store.WithTx(tx)).Insert(t.Context(), findingParams(rev.ID, "fp-1"))
		return err
	})
	if err != nil {
		t.Fatalf("RunInTx: %v", err)
	}

	if _, err := st.Review().GetByID(t.Context(), reviewID); err != nil {
		t.Fatalf("review after commit: %v", err)
	}
	findings, err := st.Finding().ListUnpublishedByReview(t.Context(), reviewID)
	if err != nil {
		t.Fatalf("findings after commit: %v", err)
	}
	if len(findings) != 1 {
		t.Errorf("got %d findings after commit, want 1", len(findings))
	}
}

// Repos bound to a transaction must not be visible outside it before the commit.
func TestWithTxIsolatesUncommittedWrites(t *testing.T) {
	st, _ := storetest.Store(t)

	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)

	go func() {
		done <- st.RunInTx(t.Context(), func(tx pgx.Tx) error {
			if _, err := st.Review(store.WithTx(tx)).Create(t.Context(), reviewParams("reviewed")); err != nil {
				return err
			}
			close(started)
			<-release
			return nil
		})
	}()

	<-started
	var n int64
	if err := st.Pool().QueryRow(t.Context(), `SELECT count(*) FROM mr_reviews`).Scan(&n); err != nil {
		t.Fatalf("count from outside the tx: %v", err)
	}
	if n != 0 {
		t.Errorf("got %d reviews visible before commit, want 0", n)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("RunInTx: %v", err)
	}

	if err := st.Pool().QueryRow(t.Context(), `SELECT count(*) FROM mr_reviews`).Scan(&n); err != nil {
		t.Fatalf("count after commit: %v", err)
	}
	if n != 1 {
		t.Errorf("got %d reviews after commit, want 1", n)
	}
}

// uuidv7Time extracts the 48-bit big-endian Unix-ms prefix of a v7 UUID.
func uuidv7Time(id uuid.UUID) time.Time {
	var b [8]byte
	copy(b[2:], id[:6])
	return time.UnixMilli(int64(binary.BigEndian.Uint64(b[:])))
}

// Migration 0001 is the reason `DEFAULT uuidv7()` works on PostgreSQL 16/17.
// Whichever implementation the server resolves to, the ids must be real v7:
// time-ordered, correctly versioned, and stamped with the current time.
func TestUUIDv7Default(t *testing.T) {
	st, pool := storetest.Store(t)

	// Exactly one zero-argument uuidv7() exists on any supported server: the
	// migration installs its shim only where pg_proc says the built-in is
	// missing. Two would not be an error the server reports — pg_catalog is
	// searched before the search_path schemas, so the shim would simply become
	// dead weight that reads as load-bearing.
	var (
		builtins int
		shims    int
	)
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*) FILTER (WHERE n.nspname = 'pg_catalog'),
		       count(*) FILTER (WHERE n.nspname = 'public')
		  FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		 WHERE p.proname = 'uuidv7' AND p.pronargs = 0`).Scan(&builtins, &shims); err != nil {
		t.Fatalf("count uuidv7 implementations: %v", err)
	}
	t.Logf("uuidv7(): %d built-in, %d shim", builtins, shims)
	if builtins+shims != 1 {
		t.Fatalf("got %d built-in + %d shim uuidv7() implementations, want exactly 1", builtins, shims)
	}
	// And the one that exists is the one an unqualified call reaches.
	var schema string
	if err := pool.QueryRow(t.Context(), `
		SELECT n.nspname
		  FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		 WHERE p.oid = 'uuidv7()'::regprocedure`).Scan(&schema); err != nil {
		t.Fatalf("resolve uuidv7(): %v", err)
	}
	if (schema == "pg_catalog") != (builtins == 1) {
		t.Errorf("uuidv7() resolves to %s.uuidv7() but built-in count is %d", schema, builtins)
	}

	var direct uuid.UUID
	if err := pool.QueryRow(t.Context(), `SELECT uuidv7()`).Scan(&direct); err != nil {
		t.Fatalf("call uuidv7(): %v", err)
	}
	assertUUIDv7(t, schema+".uuidv7()", direct)

	// Ordering inside a single millisecond: the shim spends the 12 rand_a bits
	// on sub-millisecond precision exactly as PG18's built-in does, so this must
	// hold on every supported version. Without it, dev (18) and production (17)
	// would differ in B-tree insert locality and in `ORDER BY created_at, id`
	// tiebreaks. No sleep here — the point is the same-millisecond case.
	var ordered bool
	if err := pool.QueryRow(t.Context(), `
		WITH x AS (SELECT uuidv7()::text u, row_number() OVER () r FROM generate_series(1, 50))
		SELECT bool_and(u > prev)
		  FROM (SELECT u, lag(u) OVER (ORDER BY r) prev FROM x) t
		 WHERE prev IS NOT NULL`).Scan(&ordered); err != nil {
		t.Fatalf("check intra-millisecond ordering: %v", err)
	}
	if !ordered {
		t.Errorf("uuidv7() is not strictly increasing within one millisecond: rand_a must carry sub-millisecond precision")
	}

	// And the same through the column DEFAULT, which is what actually runs.
	var ids []uuid.UUID
	for i := range 3 {
		p := reviewParams("reviewed")
		p.HeadSha = testSHA[:len(testSHA)-1] + string(rune('a'+i))
		ids = append(ids, createReview(t, st, p).ID)
	}
	for i, id := range ids {
		assertUUIDv7(t, "DEFAULT uuidv7()", id)
		if i > 0 && id.String() <= ids[i-1].String() {
			t.Errorf("id %s is not after %s — ids are not time-ordered", id, ids[i-1])
		}
	}
}

func assertUUIDv7(t *testing.T, source string, id uuid.UUID) {
	t.Helper()
	if got := id.Version(); got != 7 {
		t.Errorf("%s: got UUID version %d, want 7", source, got)
	}
	if got := id.Variant(); got != uuid.RFC4122 {
		t.Errorf("%s: got variant %v, want RFC4122", source, got)
	}
	stamp := uuidv7Time(id)
	if delta := time.Since(stamp); delta < -time.Minute || delta > time.Minute {
		t.Errorf("%s: timestamp %v is %v away from now — not a time-ordered v7", source, stamp, delta)
	}
}
