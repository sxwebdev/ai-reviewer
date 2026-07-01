package app

import (
	"context"

	"github.com/sxwebdev/ai-reviewer/internal/gitlab"
	"github.com/sxwebdev/ai-reviewer/internal/jobs"
	"github.com/sxwebdev/ai-reviewer/internal/service"
	"github.com/sxwebdev/ai-reviewer/internal/state"
)

// newWorker builds a job worker with the review and sync handlers registered
// against the given service bundle.
func (a *App) newWorker(bundle *service.Bundle) *jobs.Worker {
	w := jobs.NewWorker(a.DB, a.Cfg.Watch.MaxParallel, a.Log)

	w.Register(state.JobReview, func(ctx context.Context, job *state.Job) error {
		p, err := jobs.DecodeReviewPayload(job)
		if err != nil {
			return err
		}
		mr, err := bundle.DB.GetMergeRequest(ctx, p.MRLocalID)
		if err != nil {
			return err
		}
		_, err = bundle.Review.RunReview(ctx, gitlab.MRRef{
			Host: mr.GitLabHost, ProjectID: mr.ProjectID, IID: mr.IID,
		})
		return err
	})

	w.Register(state.JobSync, func(ctx context.Context, _ *state.Job) error {
		_, err := bundle.Sync.SyncAssignedMRs(ctx)
		return err
	})

	return w
}
