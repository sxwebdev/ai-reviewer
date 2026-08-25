package dbtypes

import (
	"database/sql/driver"
	"fmt"
	"slices"
)

// Status columns are plain `text` in the schema, with no CHECK constraint. The
// closed set of values is enforced here instead, and enforced where it cannot
// be bypassed: each type implements driver.Valuer, and pgx calls Value() while
// encoding every query parameter. A write therefore fails at the driver — the
// same for a generated CRUD method, a hand-written query, inside a transaction
// or out of one — rather than relying on every call site to remember a check.
//
// Values written as SQL literals (`SET status = 'succeeded'`) never pass through
// Value(); they are fixed when the query is authored, so a typo there is a
// compile-time-ish mistake rather than a runtime one. TestSQLStatusLiterals in
// internal/store scans the .sql files and asserts every literal is a value
// declared below — that is what replaces the CHECK for those paths.
//
// Scan is deliberately permissive: a value already in the database is a fact,
// and refusing to read it would turn one bad row into a broken sweep. Validate
// on the way in; report, do not explode, on the way out.

// ReviewStatus is the stage of one review attempt (mr_reviews.status).
// The ladder is reviewed → succeeded; dry_run, failed and abandoned terminate.
type ReviewStatus string

const (
	// ReviewReviewed means the review is persisted and publication is pending.
	ReviewReviewed ReviewStatus = "reviewed"
	// ReviewSucceeded is set last, after the summary note carrying the review
	// marker has been posted — so it can never be a false success.
	ReviewSucceeded ReviewStatus = "succeeded"
	// ReviewDryRun means the review ran and was persisted but must never reach
	// GitLab.
	ReviewDryRun ReviewStatus = "dry_run"
	// ReviewFailed records an attempt that did not produce a result. Failed rows
	// are what the section 6.5 backoff ladder counts.
	ReviewFailed ReviewStatus = "failed"
	// ReviewAbandoned is the terminal state of a review that was computed and
	// persisted but can never be published — the MR was deleted, the project
	// archived, the token lost its scope. It keeps the SHA out of re-review (the
	// unique index excludes only 'failed') without keeping the row in the
	// section 6.3 sweep, and it is not a section 6.5 strike: the review worked,
	// GitLab did not.
	ReviewAbandoned ReviewStatus = "abandoned"
)

// ReviewStatuses lists every valid value, newest-state-last.
func ReviewStatuses() []ReviewStatus {
	return []ReviewStatus{ReviewReviewed, ReviewSucceeded, ReviewDryRun, ReviewFailed, ReviewAbandoned}
}

func (s ReviewStatus) Valid() bool { return slices.Contains(ReviewStatuses(), s) }

func (s ReviewStatus) String() string { return string(s) }

// Value implements driver.Valuer — the choke point that makes an invalid status
// unwritable rather than merely discouraged.
func (s ReviewStatus) Value() (driver.Value, error) {
	return statusValue("mr_reviews.status", s, ReviewStatuses())
}

// Scan implements sql.Scanner. See the package note: reads are permissive.
func (s *ReviewStatus) Scan(src any) error { return statusScan(src, s) }

// DigestRunStatus is the outcome of one digest run (digest_runs.status).
type DigestRunStatus string

const (
	// DigestBuilt means the payload was assembled and the delivery jobs queued.
	DigestBuilt DigestRunStatus = "built"
	// DigestSent means every part was confirmed delivered.
	DigestSent DigestRunStatus = "sent"
	// DigestPartial means some part never landed, or some repository could not
	// be inspected while building.
	DigestPartial DigestRunStatus = "partial"
	// DigestDryRun means the digest was built but deliberately not delivered.
	DigestDryRun DigestRunStatus = "dry_run"
	// DigestFailed means the run never got as far as assembling a payload.
	DigestFailed DigestRunStatus = "failed"
)

// DigestRunStatuses lists every valid value.
func DigestRunStatuses() []DigestRunStatus {
	return []DigestRunStatus{DigestBuilt, DigestSent, DigestPartial, DigestDryRun, DigestFailed}
}

func (s DigestRunStatus) Valid() bool { return slices.Contains(DigestRunStatuses(), s) }

func (s DigestRunStatus) String() string { return string(s) }

func (s DigestRunStatus) Value() (driver.Value, error) {
	return statusValue("digest_runs.status", s, DigestRunStatuses())
}

func (s *DigestRunStatus) Scan(src any) error { return statusScan(src, s) }

// MessageStatus is the delivery state of one digest part (digest_messages.status).
//
// This set is load-bearing beyond storage: the claim protocol branches on all
// five values, and ClaimForSend maps anything outside ('pending','sending') to
// "not claimed", which the worker reads as "already delivered, nothing to do".
// An unrecognised value would therefore drop a digest silently rather than fail.
type MessageStatus string

const (
	// MessagePending is a part that has not been claimed for delivery yet.
	MessagePending MessageStatus = "pending"
	// MessageSending is claimed: a POST may be in flight, or may have been lost
	// with its result unrecorded.
	MessageSending MessageStatus = "sending"
	// MessageSent is confirmed delivered, with slack_ts recorded.
	MessageSent MessageStatus = "sent"
	// MessageFailed is a terminal delivery failure.
	MessageFailed MessageStatus = "failed"
	// MessageDryRun is a part that was built but must never be delivered.
	MessageDryRun MessageStatus = "dry_run"
)

// MessageStatuses lists every valid value.
func MessageStatuses() []MessageStatus {
	return []MessageStatus{MessagePending, MessageSending, MessageSent, MessageFailed, MessageDryRun}
}

func (s MessageStatus) Valid() bool { return slices.Contains(MessageStatuses(), s) }

func (s MessageStatus) String() string { return string(s) }

func (s MessageStatus) Value() (driver.Value, error) {
	return statusValue("digest_messages.status", s, MessageStatuses())
}

func (s *MessageStatus) Scan(src any) error { return statusScan(src, s) }

// statusValue is the shared body of every Value(): reject anything outside the
// closed set, and name both the column and the accepted values so the error is
// actionable without opening the schema.
func statusValue[T ~string](column string, v T, valid []T) (driver.Value, error) {
	if slices.Contains(valid, v) {
		return string(v), nil
	}
	names := make([]string, len(valid))
	for i, s := range valid {
		names[i] = string(s)
	}
	return nil, fmt.Errorf("invalid %s %q: want one of %v", column, string(v), names)
}

// statusScan accepts the text and []byte forms pgx may hand back. A NULL leaves
// the zero value, which Valid() rejects — the columns are NOT NULL, so a NULL
// here means someone changed the schema underneath us.
func statusScan[T ~string](src any, dst *T) error {
	switch v := src.(type) {
	case nil:
		*dst = ""
	case string:
		*dst = T(v)
	case []byte:
		*dst = T(v)
	default:
		return fmt.Errorf("cannot scan %T into a status", src)
	}
	return nil
}
