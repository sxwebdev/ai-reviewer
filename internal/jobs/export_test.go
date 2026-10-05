package jobs

import (
	"context"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// This file exposes the workers to the black-box test package. The workers are
// built exactly as NewService builds them — from the service's own config and
// dependencies — so a test drives the production object, not a rehearsal of it.

// testJob wraps args in the minimal JobRow a worker reads: kind (for metrics)
// and the attempt counters (for the terminal-state label).
func testJob[T river.JobArgs](args T) *river.Job[T] {
	return &river.Job[T]{
		JobRow: &rivertype.JobRow{Kind: args.Kind(), Attempt: 1, MaxAttempts: 3},
		Args:   args,
	}
}

// WorkScan runs the scan dispatcher.
func WorkScan(ctx context.Context, s *Service, args ScanArgs) error {
	w := &ScanWorker{log: s.log, svc: s}
	return w.Work(ctx, testJob(args))
}

// WorkScanRepo runs one repository's pass.
func WorkScanRepo(ctx context.Context, s *Service, args ScanRepoArgs) error {
	w := &ScanRepoWorker{log: s.log, svc: s, scanner: s.deps.Scanner}
	return w.Work(ctx, testJob(args))
}

// WorkReview runs one review through the worker.
func WorkReview(ctx context.Context, s *Service, args ReviewArgs) error {
	w := &ReviewWorker{log: s.log, svc: s, reviewer: s.deps.Reviewer}
	return w.Work(ctx, testJob(args))
}

// WorkPublishReview runs the publication job.
func WorkPublishReview(ctx context.Context, s *Service, args PublishReviewArgs) error {
	w := &PublishReviewWorker{log: s.log, reviewer: s.deps.Reviewer}
	return w.Work(ctx, testJob(args))
}

// WorkDigest runs the digest builder.
func WorkDigest(ctx context.Context, s *Service, args DigestArgs) error {
	w := &DigestWorker{log: s.log, svc: s, digester: s.deps.Digester}
	return w.Work(ctx, testJob(args))
}

// WorkSlackSend runs one delivery.
func WorkSlackSend(ctx context.Context, s *Service, args SlackSendArgs) error {
	w := &SlackSendWorker{log: s.log, digester: s.deps.Digester}
	return w.Work(ctx, testJob(args))
}

// WorkCleanup runs the workdir sweep.
func WorkCleanup(ctx context.Context, s *Service) error {
	w := &CleanupWorker{log: s.log, workdir: s.cfg.WorkDir}
	return w.Work(ctx, testJob(CleanupArgs{}))
}

// WorkSlackCommand runs one in-chat command answer.
func WorkSlackCommand(ctx context.Context, s *Service, args SlackCommandArgs) error {
	w := &SlackCommandWorker{log: s.log, svc: s, commander: s.deps.Commander}
	return w.Work(ctx, testJob(args))
}
