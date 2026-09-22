package store_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shopspring/decimal"

	"github.com/sxwebdev/ai-reviewer/internal/store/repos/repo_review"
	"github.com/sxwebdev/ai-reviewer/internal/store/storetest"
)

// The partial unique index is the whole state machine of section 6.5 in one
// constraint: any number of failed attempts per SHA, at most one that counts.
func TestReviewSuccessUniqueIndexAllowsManyFailures(t *testing.T) {
	st, _ := storetest.Store(t)

	for range 3 {
		p := reviewParams("failed")
		p.Error = "clone denied"
		createReview(t, st, p)
	}
	createReview(t, st, reviewParams("reviewed"))

	// A second non-failed row for the same SHA must be rejected: that is what
	// stops two replicas both reviewing (and paying for) one head SHA.
	_, err := st.Review().Create(t.Context(), reviewParams("dry_run"))
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("second non-failed review: got err %v, want a *pgconn.PgError", err)
	}
	if pgErr.Code != "23505" {
		t.Errorf("second non-failed review: got SQLSTATE %s, want 23505 (unique_violation)", pgErr.Code)
	}
	if pgErr.ConstraintName != "mr_reviews_success_uniq" {
		t.Errorf("got constraint %q, want mr_reviews_success_uniq", pgErr.ConstraintName)
	}
}

func TestReviewGetByHeadSHA(t *testing.T) {
	st, _ := storetest.Store(t)

	args := repo_review.GetByHeadSHAParams{ProjectID: testProject, MrIid: testMR, HeadSha: testSHA}

	// No row at all: the MR needs a review.
	if _, err := st.Review().GetByHeadSHA(t.Context(), args); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("unreviewed SHA: got err %v, want pgx.ErrNoRows", err)
	}

	// A failed attempt must not look like "already reviewed", otherwise a
	// transient failure would freeze the MR until the next push.
	failed := reviewParams("failed")
	failed.Error = "boom"
	createReview(t, st, failed)
	if _, err := st.Review().GetByHeadSHA(t.Context(), args); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("failed-only SHA: got err %v, want pgx.ErrNoRows", err)
	}

	// dry_run counts: it is what stops a re-review every scan_interval when
	// publishing is off.
	want := createReview(t, st, reviewParams("dry_run"))
	got, err := st.Review().GetByHeadSHA(t.Context(), args)
	if err != nil {
		t.Fatalf("reviewed SHA: %v", err)
	}
	if got.ID != want.ID {
		t.Errorf("got review %s, want %s", got.ID, want.ID)
	}
	if got.Status != "dry_run" {
		t.Errorf("got status %q, want dry_run", got.Status)
	}

	// A different head SHA is a different question.
	other := args
	other.HeadSha = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if _, err := st.Review().GetByHeadSHA(t.Context(), other); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("new head SHA: got err %v, want pgx.ErrNoRows", err)
	}
}

func TestReviewGetLatestByMR(t *testing.T) {
	st, _ := storetest.Store(t)

	if _, err := st.Review().GetLatestByMR(t.Context(), testProject, testMR); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("no reviews: got err %v, want pgx.ErrNoRows", err)
	}

	old := reviewParams("succeeded")
	createReview(t, st, old)

	newer := reviewParams("reviewed")
	newer.HeadSha = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	want := createReview(t, st, newer)

	// A later failure must not become "the latest review".
	failed := reviewParams("failed")
	failed.HeadSha = "cccccccccccccccccccccccccccccccccccccccc"
	createReview(t, st, failed)

	got, err := st.Review().GetLatestByMR(t.Context(), testProject, testMR)
	if err != nil {
		t.Fatalf("GetLatestByMR: %v", err)
	}
	if got.ID != want.ID {
		t.Errorf("got review %s (sha %s), want %s", got.ID, got.HeadSha, want.ID)
	}
}

func TestReviewFailureStats(t *testing.T) {
	st, _ := storetest.Store(t)

	args := repo_review.FailureStatsParams{ProjectID: testProject, MrIid: testMR, HeadSha: testSHA}

	stats, err := st.Review().FailureStats(t.Context(), args)
	if err != nil {
		t.Fatalf("FailureStats on empty table: %v", err)
	}
	if stats.Failures != 0 {
		t.Errorf("got %d failures, want 0", stats.Failures)
	}
	if !stats.LastFailureAt.IsZero() {
		t.Errorf("got last failure %v, want the zero time", stats.LastFailureAt)
	}
	if stats.LastError != "" {
		t.Errorf("got last error %q, want empty", stats.LastError)
	}

	before := time.Now().Add(-time.Second)
	for _, msg := range []string{"clone denied", "schema-invalid JSON"} {
		p := reviewParams("failed")
		p.Error = msg
		createReview(t, st, p)
	}
	// A succeeded row for the same SHA must not inflate the counter, and its
	// (empty) error must not be mistaken for the last failure's.
	createReview(t, st, reviewParams("reviewed"))

	stats, err = st.Review().FailureStats(t.Context(), args)
	if err != nil {
		t.Fatalf("FailureStats: %v", err)
	}
	if stats.Failures != 2 {
		t.Errorf("got %d failures, want 2", stats.Failures)
	}
	// Section 6.5 needs this in the warning it logs once the ladder gives up:
	// an MR that stopped costing money still has to say why it is poisoned.
	if stats.LastError != "schema-invalid JSON" {
		t.Errorf("got last error %q, want the most recent failure's", stats.LastError)
	}
	if stats.LastFailureAt.Before(before) || stats.LastFailureAt.After(time.Now().Add(time.Second)) {
		t.Errorf("last failure %v is outside the window the rows were written in", stats.LastFailureAt)
	}

	// A new push changes head_sha, which is what resets the ladder.
	fresh := args
	fresh.HeadSha = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	stats, err = st.Review().FailureStats(t.Context(), fresh)
	if err != nil {
		t.Fatalf("FailureStats for a fresh SHA: %v", err)
	}
	if stats.Failures != 0 {
		t.Errorf("new head SHA: got %d failures, want 0", stats.Failures)
	}
	if stats.LastError != "" {
		t.Errorf("new head SHA: got last error %q, want empty", stats.LastError)
	}
}

func TestReviewClaimStalePublications(t *testing.T) {
	st, pool := storetest.Store(t)

	stale := createReview(t, st, reviewParams("reviewed"))

	fresh := reviewParams("reviewed")
	fresh.HeadSha = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	createReview(t, st, fresh)

	// Already published, and a dry-run that must never be published: neither is
	// waiting for delivery.
	done := reviewParams("succeeded")
	done.HeadSha = "cccccccccccccccccccccccccccccccccccccccc"
	createReview(t, st, done)
	dry := reviewParams("dry_run")
	dry.HeadSha = "dddddddddddddddddddddddddddddddddddddddd"
	createReview(t, st, dry)

	// Another project's stuck review belongs to another scan_repo job.
	otherProject := reviewParams("reviewed")
	otherProject.ProjectID = testProject + 1
	otherProject.HeadSha = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	otherStale := createReview(t, st, otherProject)

	// Age the two by 20 minutes; the cutoff below is 15.
	for _, id := range []any{stale.ID, otherStale.ID} {
		if _, err := pool.Exec(t.Context(),
			`UPDATE mr_reviews SET created_at = now() - interval '20 minutes' WHERE id = $1`, id,
		); err != nil {
			t.Fatalf("age review: %v", err)
		}
	}

	// Exhausted: eligible in every other respect, but it has used its budget.
	spent := reviewParams("reviewed")
	spent.HeadSha = "ffffffffffffffffffffffffffffffffffffffff"
	spentRev := createReview(t, st, spent)
	if _, err := pool.Exec(t.Context(),
		`UPDATE mr_reviews SET created_at = now() - interval '20 minutes', publish_attempts = 3 WHERE id = $1`,
		spentRev.ID,
	); err != nil {
		t.Fatalf("age review: %v", err)
	}

	claim := func() []uuid.UUID {
		t.Helper()
		got, err := st.Review().ClaimStalePublications(t.Context(), repo_review.ClaimStalePublicationsParams{
			ProjectID:   testProject,
			OlderThan:   time.Now().Add(-15 * time.Minute),
			MaxAttempts: 3,
			MaxRows:     100,
		})
		if err != nil {
			t.Fatalf("ClaimStalePublications: %v", err)
		}
		return got
	}

	got := claim()
	if len(got) != 1 || got[0] != stale.ID {
		t.Fatalf("got %v, want exactly the stale review %s", got, stale.ID)
	}

	// The claim is what bounds the loop, so it must be recorded by the same
	// statement that hands the row out.
	row, err := st.Review().GetByID(t.Context(), stale.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if row.PublishAttempts != 1 {
		t.Errorf("publish_attempts = %d after one claim, want 1", row.PublishAttempts)
	}

	// Two more claims exhaust the budget; the fourth pass must find nothing.
	claim()
	claim()
	if got := claim(); len(got) != 0 {
		t.Errorf("claimed %v after the attempt ceiling, want nothing", got)
	}

	// The exhausted rows — the one seeded that way and the one just spent — are
	// the sweep's terminal state, and only they.
	abandoned, err := st.Review().AbandonExhaustedPublications(t.Context(), repo_review.AbandonExhaustedPublicationsParams{
		ErrorText:   "gave up",
		ProjectID:   testProject,
		OlderThan:   time.Now().Add(-15 * time.Minute),
		MaxAttempts: 3,
	})
	if err != nil {
		t.Fatalf("AbandonExhaustedPublications: %v", err)
	}
	if len(abandoned) != 2 {
		t.Fatalf("abandoned %d reviews, want 2 (%s and %s)", len(abandoned), stale.ID, spentRev.ID)
	}
	for _, id := range abandoned {
		row, err := st.Review().GetByID(t.Context(), id)
		if err != nil {
			t.Fatalf("GetByID: %v", err)
		}
		if row.Status != "abandoned" || row.Error != "gave up" {
			t.Errorf("review %s is %q/%q, want abandoned/\"gave up\"", id, row.Status, row.Error)
		}
	}

	// An abandoned review still occupies mr_reviews_success_uniq, which is what
	// keeps the scanner from re-reviewing the SHA at full LLM price.
	if _, err := st.Review().GetByHeadSHA(t.Context(), repo_review.GetByHeadSHAParams{
		ProjectID: testProject, MrIid: stale.MrIid, HeadSha: stale.HeadSha,
	}); err != nil {
		t.Errorf("GetByHeadSHA after abandoning: %v, want the row to still be found", err)
	}

	// And it is not a §6.5 strike.
	stats, err := st.Review().FailureStats(t.Context(), repo_review.FailureStatsParams{
		ProjectID: testProject, MrIid: stale.MrIid, HeadSha: stale.HeadSha,
	})
	if err != nil {
		t.Fatalf("FailureStats: %v", err)
	}
	if stats.Failures != 0 {
		t.Errorf("abandoning counted %d failures, want 0", stats.Failures)
	}

	// The other project's stuck review belongs to another scan_repo job and was
	// never touched.
	other, err := st.Review().GetByID(t.Context(), otherStale.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if other.Status != "reviewed" || other.PublishAttempts != 0 {
		t.Errorf("other project's review is %q with %d attempts, want reviewed/0", other.Status, other.PublishAttempts)
	}
}

func TestReviewMarkSucceeded(t *testing.T) {
	st, _ := storetest.Store(t)

	rev := createReview(t, st, reviewParams("reviewed"))
	if err := st.Review().MarkSucceeded(t.Context(), rev.ID); err != nil {
		t.Fatalf("MarkSucceeded: %v", err)
	}

	got, err := st.Review().GetByID(t.Context(), rev.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Status != "succeeded" {
		t.Errorf("got status %q, want succeeded", got.Status)
	}
}

// cost_usd is numeric(12,6) in SQL and decimal.Decimal in Go — a pgxgen type
// override, and one pgx supports only through the driver.Valuer/sql.Scanner
// fallback (no codec is registered on the pool). So pin the whole path: that a
// Decimal can be bound as a parameter at all, that it survives RETURNING and a
// later SELECT, and that it comes back at the column's full precision.
//
// Compare with Equal, never with ==. Decimal is a value+exponent pair and ==
// compares both: 12.345678 read back from numeric(12,6) carries exponent -6,
// while the same number built by decimal.NewFromFloat carries whatever exponent
// the shortest representation needed. They are the same amount and different
// structs.
func TestReviewCostRoundTrip(t *testing.T) {
	st, _ := storetest.Store(t)

	want := decimal.RequireFromString("12.345678")
	p := reviewParams("reviewed")
	p.CostUsd = want
	p.DurationMs = 1234
	rev := createReview(t, st, p)
	if !rev.CostUsd.Equal(want) {
		t.Errorf("RETURNING gave cost %v, want %v", rev.CostUsd, want)
	}

	got, err := st.Review().GetByID(t.Context(), rev.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if !got.CostUsd.Equal(want) {
		t.Errorf("got cost %v, want %v", got.CostUsd, want)
	}
	// The digits themselves, not just the numeric value: a float64 hop anywhere
	// on this path would show up here as 12.345677999999999 or 12.3457.
	if s := got.CostUsd.String(); s != "12.345678" {
		t.Errorf("stored cost renders as %q, want exactly %q", s, "12.345678")
	}
	if got.DurationMs != p.DurationMs {
		t.Errorf("got duration %v, want %v", got.DurationMs, p.DurationMs)
	}

	// The zero Decimal is what every review that cost nothing writes (the struct
	// literal leaves the field unset), and an uninitialised big.Int inside it
	// would be an encode error rather than a 0.
	zero := reviewParams("dry_run")
	zero.HeadSha = strings.Repeat("c", 40)
	free := createReview(t, st, zero)
	if !free.CostUsd.IsZero() {
		t.Errorf("the zero Decimal stored as %v, want 0", free.CostUsd)
	}
}
