package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/sxwebdev/ai-reviewer/internal/gitlab"
	"github.com/sxwebdev/ai-reviewer/internal/state"
)

// PublishService creates GitLab draft notes from approved findings and, only on
// explicit confirmation, publishes them. This is the single code path to
// GitLab writes; nothing else publishes.
type PublishService struct {
	gl  gitlab.API
	db  *state.DB
	log *slog.Logger
}

// NewPublishService constructs a PublishService.
func NewPublishService(gl gitlab.API, db *state.DB, log *slog.Logger) *PublishService {
	return &PublishService{gl: gl, db: db, log: log}
}

// ConfirmPhrase is the exact phrase required to publish n comments.
func ConfirmPhrase(n int) string { return fmt.Sprintf("PUBLISH %d COMMENTS", n) }

// CreateDrafts turns every approved finding of a review into a GitLab draft
// note (inline where a position exists, overview otherwise). It never publishes.
func (s *PublishService) CreateDrafts(ctx context.Context, reviewID string) (int, error) {
	rv, err := s.db.GetReview(ctx, reviewID)
	if err != nil {
		return 0, err
	}
	approved, err := s.db.ListFindingsByReviewStatus(ctx, reviewID, state.FindingApproved)
	if err != nil {
		return 0, err
	}
	projectKey := strconv.FormatInt(rv.ProjectID, 10)

	created := 0
	for _, f := range approved {
		var pos *gitlab.Position
		if f.GitLabPositionJSON != "" {
			pos = &gitlab.Position{}
			if err := json.Unmarshal([]byte(f.GitLabPositionJSON), pos); err != nil {
				s.log.Warn("bad stored position; posting as overview", "finding", f.ID, "err", err)
				pos = nil
			}
		}
		dn, err := s.gl.CreateDraftNote(ctx, projectKey, rv.MRIID, f.Body, pos)
		if err != nil {
			s.log.Warn("create draft failed", "finding", f.ID, "err", err)
			continue
		}
		// The GitLab draft now exists; recording its id is load-bearing. If it
		// fails, mark the finding failed (using a cancellation-detached context)
		// so a retry does NOT create a duplicate draft — it would otherwise stay
		// 'approved' and be re-selected on the next CreateDrafts.
		if err := s.db.SetFindingDraftNote(ctx, f.ID, dn.ID, state.FindingDrafted); err != nil {
			s.log.Error("draft created but id not recorded; marking finding failed to avoid a duplicate",
				"finding", f.ID, "draft_note", dn.ID, "err", err)
			_ = s.db.UpdateFindingStatus(context.WithoutCancel(ctx), f.ID, state.FindingFailed,
				"draft created on GitLab but id not recorded: "+err.Error())
			continue
		}
		_ = s.db.InsertAuditEvent(ctx, state.AuditCreateDraft, reviewID, rv.MRID,
			fmt.Sprintf("draft note %d for finding %s", dn.ID, f.ID))
		created++
	}
	if created > 0 {
		_ = s.db.UpdateReviewStatus(ctx, reviewID, state.ReviewDrafted)
	}
	return created, nil
}

// PublishDrafts publishes the review's drafted findings, but ONLY when confirm
// exactly matches ConfirmPhrase(count). This is the hard human gate.
func (s *PublishService) PublishDrafts(ctx context.Context, reviewID, confirm string) (int, error) {
	rv, err := s.db.GetReview(ctx, reviewID)
	if err != nil {
		return 0, err
	}
	drafted, err := s.db.ListFindingsByReviewStatus(ctx, reviewID, state.FindingDrafted)
	if err != nil {
		return 0, err
	}
	want := ConfirmPhrase(len(drafted))
	if confirm != want {
		return 0, fmt.Errorf("publish not confirmed: type exactly %q to publish %d comment(s)", want, len(drafted))
	}
	projectKey := strconv.FormatInt(rv.ProjectID, 10)

	published := 0
	for _, f := range drafted {
		if f.GitLabDraftNoteID == nil {
			continue
		}
		if err := s.gl.PublishDraftNote(ctx, projectKey, rv.MRIID, *f.GitLabDraftNoteID); err != nil {
			s.log.Warn("publish draft failed", "finding", f.ID, "err", err)
			continue
		}
		// The comment is now live on GitLab; record it with a detached context
		// so a shutdown mid-publish doesn't leave it stuck 'drafted' (which
		// would re-attempt publish on the already-published note next time).
		if err := s.db.UpdateFindingStatus(context.WithoutCancel(ctx), f.ID, state.FindingPublished, ""); err != nil {
			s.log.Error("published to GitLab but failed to record status", "finding", f.ID, "err", err)
		}
		published++
	}
	if published > 0 {
		_ = s.db.InsertAuditEvent(ctx, state.AuditPublish, reviewID, rv.MRID,
			fmt.Sprintf("published %d comment(s)", published))
		_ = s.db.UpdateReviewStatus(ctx, reviewID, state.ReviewPublished)
	}
	return published, nil
}
