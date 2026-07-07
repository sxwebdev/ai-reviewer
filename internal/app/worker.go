package app

import (
	"context"

	"github.com/sxwebdev/ai-reviewer/internal/gitlab"
	"github.com/sxwebdev/ai-reviewer/internal/jobs"
	"github.com/sxwebdev/ai-reviewer/internal/state"
)

// newWorker builds a job worker with the review and sync handlers registered.
// Handlers resolve the service bundle per job (not at registration time) so
// hot-applied config changes reach queued jobs too. The pool size, however, is
// fixed at construction: a watch.max_parallel change needs a process restart
// (reloadAndRebuild logs a warning when it detects one).
func (a *App) newWorker() *jobs.Worker {
	w := jobs.NewWorker(a.DB, a.Config().Watch.MaxParallel, a.Log)

	w.Register(state.JobReview, func(ctx context.Context, job *state.Job) error {
		p, err := jobs.DecodeReviewPayload(job)
		if err != nil {
			return err
		}
		bundle := a.Bundle()
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
		_, err := a.Bundle().Sync.SyncAssignedMRs(ctx)
		return err
	})

	return w
}
