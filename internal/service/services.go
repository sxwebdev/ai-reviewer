package service

import (
	"log/slog"

	"github.com/sxwebdev/ai-reviewer/internal/gitlab"
	"github.com/sxwebdev/ai-reviewer/internal/review"
	"github.com/sxwebdev/ai-reviewer/internal/state"
)

// Bundle groups the application services so the web server and CLI share one
// wiring. Read paths use DB directly; write/action paths go through services.
type Bundle struct {
	DB      *state.DB
	Sync    *SyncService
	Review  *ReviewService
	Finding *FindingService
	Publish *PublishService
	Host    string
}

// NewBundle wires all services from their dependencies.
func NewBundle(gl gitlab.API, db *state.DB, eng *review.Engine, rc ReviewConfig, log *slog.Logger) *Bundle {
	return &Bundle{
		DB:      db,
		Sync:    NewSyncService(gl, db, rc.Host, log),
		Review:  NewReviewService(gl, db, eng, rc, log),
		Finding: NewFindingService(db),
		Publish: NewPublishService(gl, db, log),
		Host:    rc.Host,
	}
}
