package app

import (
	"context"
	"errors"
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

// errNotWired marks entrypoints whose subsystem is not yet online. Replaced as
// the build progresses (see plan build order).
var errNotWired = errors.New("subsystem not yet available")

// ServeOptions configures the serve command.
type ServeOptions struct {
	// RunWorker starts the background job worker alongside the web UI.
	RunWorker bool
}

// Serve starts the local web UI and blocks until the context is cancelled or an
// interrupt signal is received. (The background worker is wired in build step 11.)
func (a *App) Serve(ctx context.Context, opts ServeOptions) error {
	bundle, err := a.Services()
	if err != nil {
		return err
	}
	srv, err := server.New(bundle, a.Cfg.GitLab.Host, a.Log)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	if opts.RunWorker {
		worker := a.newWorker(bundle)
		go func() {
			if err := worker.Run(ctx); err != nil {
				a.Log.Error("worker stopped", "err", err)
			}
		}()
	}
	return srv.Run(ctx, a.Cfg.App.BindHost, a.Cfg.App.Port, a.Cfg.App.OpenBrowser)
}

// RunDaemon runs the background watch worker and scheduler without the web UI.
func (a *App) RunDaemon(ctx context.Context) error {
	bundle, err := a.Services()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	worker := a.newWorker(bundle)
	scheduler := jobs.NewScheduler(a.DB, bundle.Sync, a.Cfg.Watch.Interval, a.Cfg.Review.AutoReview, a.Log)

	a.Log.Info("daemon started",
		"interval", a.Cfg.Watch.Interval, "max_parallel", a.Cfg.Watch.MaxParallel,
		"auto_review", a.Cfg.Review.AutoReview, "auto_draft", a.Cfg.Review.AutoDraft, "auto_publish", a.Cfg.Review.AutoPublish)
	if a.Cfg.Review.AutoPublish {
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
	db, err := a.OpenState()
	if err != nil {
		return err
	}
	gl, err := a.GitLabClient()
	if err != nil {
		return err
	}
	svc := service.NewSyncService(gl, db, a.Cfg.GitLab.Host, a.Log)
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
	db, err := a.OpenState()
	if err != nil {
		return err
	}
	gl, err := a.GitLabClient()
	if err != nil {
		return err
	}
	mrRef, err := gitlab.ParseRef(ref, a.Cfg.GitLab.Host)
	if err != nil {
		return err
	}

	eng := review.NewEngine(a.LLMClient(), a.Log)
	svc := service.NewReviewService(gl, db, eng, service.ReviewConfig{
		Host:             a.Cfg.GitLab.Host,
		ReviewerUsername: a.Cfg.GitLab.Username,
		Model:            a.Cfg.LLM.Model,
		LLMProvider:      a.Cfg.LLM.Provider,
		AgentMode:        false, // repo worktree not yet wired (build step 7)
		AllowedTools:     a.Cfg.LLM.Claude.AllowedTools,
		Profile:          profileFromConfig(a.Cfg.Review),
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
