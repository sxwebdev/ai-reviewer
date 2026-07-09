package jobs

import (
	"context"
	"log/slog"
	"time"

	"github.com/sxwebdev/ai-reviewer/internal/gitlab"
	"github.com/sxwebdev/ai-reviewer/internal/service"
	"github.com/sxwebdev/ai-reviewer/internal/state"
)

// Scheduler periodically syncs assigned MRs and enqueues review jobs when an
// MR's head sha has not yet been reviewed.
type Scheduler struct {
	db         *state.DB
	sync       *service.SyncService
	interval   time.Duration
	autoReview bool
	log        *slog.Logger
}

// NewScheduler builds a Scheduler.
func NewScheduler(db *state.DB, sync *service.SyncService, interval time.Duration, autoReview bool, log *slog.Logger) *Scheduler {
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	return &Scheduler{db: db, sync: sync, interval: interval, autoReview: autoReview, log: log}
}

// Run ticks until ctx is cancelled, running once immediately.
func (s *Scheduler) Run(ctx context.Context) error {
	s.tick(ctx)
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			s.tick(ctx)
		}
	}
}

func (s *Scheduler) tick(ctx context.Context) {
	if _, err := s.sync.SyncAssignedMRs(ctx); err != nil {
		s.log.Warn("scheduled sync failed", "err", err)
		return
	}
	if !s.autoReview {
		return
	}
	if paused, err := s.db.JobsPaused(ctx); err != nil {
		s.log.Warn("check pause state failed", "err", err)
		return
	} else if paused {
		return // queue paused; don't enqueue new reviews
	}
	mrs, err := s.db.ListMergeRequests(ctx)
	if err != nil {
		s.log.Warn("list MRs failed", "err", err)
		return
	}
	for _, mr := range mrs {
		if !gitlab.IsOpenState(mr.State) {
			continue // never auto-review a merged/closed MR (reconcile refreshes its head sha)
		}
		if mr.HeadSHA == "" {
			continue // can't review (or checkout) without a head sha — avoid an endless enqueue/fail loop
		}
		if s.reviewedCurrentHead(ctx, mr) {
			continue
		}
		id, err := EnqueueReview(ctx, s.db, mr.ID, mr.ProjectID, mr.IID, ReviewRequest{})
		if err != nil {
			s.log.Warn("enqueue review failed", "mr", mr.IID, "err", err)
			continue
		}
		if id != "" {
			s.log.Info("enqueued review", "mr", mr.IID, "head", mr.HeadSHA, "job", id)
		}
	}
}

// reviewedCurrentHead reports whether the MR's current head sha already has a
// review.
func (s *Scheduler) reviewedCurrentHead(ctx context.Context, mr *state.MergeRequest) bool {
	reviews, err := s.db.ListReviewsByMR(ctx, mr.ID)
	if err != nil || len(reviews) == 0 {
		return false
	}
	return reviews[0].HeadSHA == mr.HeadSHA && mr.HeadSHA != ""
}
