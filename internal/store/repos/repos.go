package repos

import (
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sxwebdev/ai-reviewer/internal/store/repos/repo_digestmessage"
	"github.com/sxwebdev/ai-reviewer/internal/store/repos/repo_digestrun"
	"github.com/sxwebdev/ai-reviewer/internal/store/repos/repo_finding"
	"github.com/sxwebdev/ai-reviewer/internal/store/repos/repo_review"
)

type Repos struct {
	review        *repo_review.Queries
	finding       *repo_finding.Queries
	digestRun     *repo_digestrun.Queries
	digestMessage *repo_digestmessage.Queries
}

func New(pool *pgxpool.Pool) *Repos {
	return &Repos{
		review:        repo_review.New(pool),
		finding:       repo_finding.New(pool),
		digestRun:     repo_digestrun.New(pool),
		digestMessage: repo_digestmessage.New(pool),
	}
}

// Every accessor takes Option so any repo can be bound to a caller's
// transaction (WithTx) — writing mr_reviews, mr_findings and the publish_review
// job in one commit is the whole point (plan section 10.4).

func (s *Repos) Review(opts ...Option) *repo_review.Queries {
	if tx := parseOptions(opts...).Tx; tx != nil {
		return s.review.WithTx(tx)
	}
	return s.review
}

func (s *Repos) Finding(opts ...Option) *repo_finding.Queries {
	if tx := parseOptions(opts...).Tx; tx != nil {
		return s.finding.WithTx(tx)
	}
	return s.finding
}

func (s *Repos) DigestRun(opts ...Option) *repo_digestrun.Queries {
	if tx := parseOptions(opts...).Tx; tx != nil {
		return s.digestRun.WithTx(tx)
	}
	return s.digestRun
}

func (s *Repos) DigestMessage(opts ...Option) *repo_digestmessage.Queries {
	if tx := parseOptions(opts...).Tx; tx != nil {
		return s.digestMessage.WithTx(tx)
	}
	return s.digestMessage
}
