package store

import (
	"context"
	"database/sql/driver"
	"fmt"

	"github.com/sxwebdev/ai-reviewer/internal/dbtypes"
	"github.com/sxwebdev/ai-reviewer/internal/models"
	"github.com/sxwebdev/ai-reviewer/internal/store/repos"
	"github.com/sxwebdev/ai-reviewer/internal/store/repos/repo_digestmessage"
	"github.com/sxwebdev/ai-reviewer/internal/store/repos/repo_digestrun"
	"github.com/sxwebdev/ai-reviewer/internal/store/repos/repo_review"
)

// Status columns are plain `text`: the schema carries no CHECK, so the closed
// set of values is the application's to enforce. These wrappers are where that
// happens. They exist because pgxgen cannot type a single column — the generated
// params carry `Status string`, so a typo would otherwise reach Postgres and be
// stored happily, and for digest_messages that means a digest silently dropped
// (ClaimForSend reads any unknown status as "already delivered").
//
// Every write that takes its status from a Go value at run time goes through
// one of these four; there are no others, and TestNoDirectStatusWrites keeps it
// that way by failing if a generated write method is called from outside this
// package. Statuses written as SQL literals are covered by TestSQLStatusLiterals.

// CreateReview inserts a review row after validating its status.
func (s *Store) CreateReview(ctx context.Context, p repo_review.CreateParams, opts ...repos.Option) (*models.MrReview, error) {
	if err := validateStatus[dbtypes.ReviewStatus](p.Status); err != nil {
		return nil, err
	}
	return s.Review(opts...).Create(ctx, p)
}

// CreateDigestRun inserts a digest-run row after validating its status.
func (s *Store) CreateDigestRun(ctx context.Context, p repo_digestrun.CreateParams, opts ...repos.Option) (*models.DigestRun, error) {
	if err := validateStatus[dbtypes.DigestRunStatus](p.Status); err != nil {
		return nil, err
	}
	return s.DigestRun(opts...).Create(ctx, p)
}

// SetDigestRunStatus moves a digest run to a new status after validating it.
func (s *Store) SetDigestRunStatus(ctx context.Context, p repo_digestrun.SetStatusParams, opts ...repos.Option) error {
	if err := validateStatus[dbtypes.DigestRunStatus](p.Status); err != nil {
		return err
	}
	return s.DigestRun(opts...).SetStatus(ctx, p)
}

// CreateDigestMessage inserts one digest part after validating its status.
func (s *Store) CreateDigestMessage(ctx context.Context, p repo_digestmessage.CreateParams, opts ...repos.Option) (*models.DigestMessage, error) {
	if err := validateStatus[dbtypes.MessageStatus](p.Status); err != nil {
		return nil, err
	}
	return s.DigestMessage(opts...).Create(ctx, p)
}

// statusType is the constraint every status column's type satisfies: a string
// underneath, with the validating Value() that owns the closed set.
type statusType interface {
	~string
	Value() (driver.Value, error)
}

// validateStatus runs the type's own Value(), so the wrapper rejects exactly
// what the driver would reject and reports it with exactly the same wording.
// One source of truth for the set, one for the message.
func validateStatus[T statusType](raw string) error {
	if _, err := T(raw).Value(); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	return nil
}
