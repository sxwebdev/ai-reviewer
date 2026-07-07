package app

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/sxwebdev/ai-reviewer/internal/gitlab"
	"github.com/sxwebdev/ai-reviewer/internal/jobs"
	"github.com/sxwebdev/ai-reviewer/internal/review"
	"github.com/sxwebdev/ai-reviewer/internal/server"
	"github.com/sxwebdev/ai-reviewer/internal/service"
)

// ServeOptions configures the serve command.
type ServeOptions struct {
	// RunWorker starts the background job worker alongside the web UI.
	RunWorker bool
}

// Serve starts the local web UI and blocks until the context is cancelled or an
// interrupt signal is received. It works without any prior configuration: the
// setup gate in the web UI walks the user through the required fields.
func (a *App) Serve(ctx context.Context, opts ServeOptions) error {
	if _, err := a.Services(); err != nil {
		return err
	}
	// Adapt the doctor checks into the server's health type so the web UI can
	// surface them without importing internal/app (which would be a cycle).
	health := func(ctx context.Context) []server.HealthCheck {
		checks := a.Doctor(ctx)
		out := make([]server.HealthCheck, 0, len(checks))
		for _, c := range checks {
			out = append(out, server.HealthCheck{Name: c.Name, Status: string(c.Status), Detail: c.Detail})
		}
		return out
	}
	srv, err := server.New(server.Deps{
		Bundle:         a.Bundle,
		UI:             a.uiConfig,
		Health:         health,
		NeedsSetup:     a.NeedsSetup,
		SetupStatus:    a.SetupStatus,
		ValidateGitLab: a.ValidateGitLab,
		ApplySetup:     a.ApplySetup,
		ApplySettings:  a.ApplySettings,
	}, a.Log)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	if opts.RunWorker {
		worker := a.newWorker()
		go func() {
			if err := worker.Run(ctx); err != nil {
				a.Log.Error("worker stopped", "err", err)
			}
		}()
	}
	cfg := a.Config()
	return srv.Run(ctx, cfg.App.BindHost, cfg.App.Port, cfg.App.OpenBrowser)
}

// RunDaemon runs the background watch worker and scheduler without the web UI.
func (a *App) RunDaemon(ctx context.Context) error {
	if err := a.requireGitLab(); err != nil {
		return err
	}
	bundle, err := a.Services()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg := a.Config()
	worker := a.newWorker()
	scheduler := jobs.NewScheduler(a.DB, bundle.Sync, cfg.Watch.Interval, cfg.Review.AutoReview, a.Log)

	a.Log.Info("daemon started",
		"interval", cfg.Watch.Interval, "max_parallel", cfg.Watch.MaxParallel,
		"auto_review", cfg.Review.AutoReview, "auto_draft", cfg.Review.AutoDraft, "auto_publish", cfg.Review.AutoPublish)
	if cfg.Review.AutoPublish {
		a.Log.Warn("AUTO-PUBLISH IS ENABLED — reviews may be posted to GitLab without manual approval")
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = worker.Run(ctx) }()
	go func() { defer wg.Done(); _ = scheduler.Run(ctx) }()
	wg.Wait()
	return nil
}

// SyncOnce performs a one-shot sync of assigned merge requests.
func (a *App) SyncOnce(ctx context.Context) error {
	if err := a.requireGitLab(); err != nil {
		return err
	}
	db, err := a.OpenState()
	if err != nil {
		return err
	}
	gl, err := a.GitLabClient()
	if err != nil {
		return err
	}
	svc := service.NewSyncService(gl, db, a.Config().GitLab.Host, a.Log)
	res, err := svc.SyncAssignedMRs(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("Synced %d assigned MR(s), tracked %d.\n", res.Total, res.Tracked)
	return nil
}

// ReviewOnce performs a one-shot review of a single MR by reference and prints
// the report. It creates only a local report — no drafts, no publishing.
func (a *App) ReviewOnce(ctx context.Context, ref string) error {
	if err := a.requireGitLab(); err != nil {
		return err
	}
	db, err := a.OpenState()
	if err != nil {
		return err
	}
	gl, err := a.GitLabClient()
	if err != nil {
		return err
	}
	cfg := a.Config()
	mrRef, err := gitlab.ParseRef(ref, cfg.GitLab.Host)
	if err != nil {
		return err
	}

	eng := review.NewEngine(a.LLMClient(), a.Log)
	svc := service.NewReviewService(gl, db, eng, service.ReviewConfig{
		Host:             cfg.GitLab.Host,
		ReviewerUsername: cfg.GitLab.Username,
		Model:            cfg.LLM.Model,
		LLMProvider:      cfg.LLM.Provider,
		AgentMode:        false, // repo worktree not yet wired (build step 7)
		AllowedTools:     cfg.LLM.Claude.AllowedTools,
		Profile:          profileFromConfig(cfg.Review),
		Context:          contextBudgetFromConfig(cfg.Review),
		Pipeline:         pipelineFromConfig(cfg.Review),
		Risk:             riskSettingsFromConfig(cfg.Review),
	}, a.Log)

	reviewID, err := svc.RunReview(ctx, mrRef)
	if err != nil {
		return err
	}
	return a.printReport(ctx, reviewID)
}

// printReport renders a review and its findings to stdout.
func (a *App) printReport(ctx context.Context, reviewID string) error {
	rv, err := a.DB.GetReview(ctx, reviewID)
	if err != nil {
		return err
	}
	findings, err := a.DB.ListFindingsByReview(ctx, reviewID)
	if err != nil {
		return err
	}
	fmt.Printf("\nReview %s\n", rv.ID)
	fmt.Printf("Risk: %s | Recommendation: %s | Findings: %d\n", rv.RiskLevel, rv.OverallRecommendation, len(findings))
	if rv.CostUSD > 0 {
		fmt.Printf("Cost: $%.4f\n", rv.CostUSD)
	}
	fmt.Printf("\nSummary: %s\n\n", rv.Summary)
	for i, f := range findings {
		loc := f.FilePath
		if f.NewLine != nil {
			loc = fmt.Sprintf("%s:%d", f.FilePath, *f.NewLine)
		}
		fmt.Printf("%d. [%s/%s] %s (%s)\n   %s\n", i+1, f.Severity, f.Category, f.Title, loc, f.Body)
		if f.ValidationError != "" {
			fmt.Printf("   note: %s\n", f.ValidationError)
		}
		fmt.Println()
	}
	fmt.Println("Findings are proposed only. Approve them in the web UI to create GitLab draft notes.")
	return nil
}
