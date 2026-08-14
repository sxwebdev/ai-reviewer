// Package store is the persistence facade: repositories plus the transaction
// boundary they share.
package store

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sxwebdev/ai-reviewer/internal/store/repos"
)

type Store struct {
	*repos.Repos
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) (*Store, error) {
	return &Store{
		Repos: repos.New(pool),
		pool:  pool,
	}, nil
}

// Pool exposes the underlying pool for the components that need the connection
// rather than a query — River's riverpgxv5 driver and the migrator.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// RunInTx runs fn in a single transaction (commit on nil, rollback on error).
// Bind any repo to the transaction with WithTx, e.g.
// s.Finding(store.WithTx(tx)).Insert(...), so related writes across repos commit
// atomically.
//
// fn receives the pgx.Tx itself, which is exactly what
// river.Client[pgx.Tx].InsertTx expects: that is how the review job writes
// mr_reviews + mr_findings and enqueues publish_review in one commit
// (plan section 10.4). A crash between the two would otherwise leave a review
// that scan_repo considers done and nobody ever publishes.
func (s *Store) RunInTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.pool, fn)
}

// WithTx binds a repo accessor to tx; pass it to any Store repo accessor.
func WithTx(tx pgx.Tx) repos.Option { return repos.WithTx(tx) }
