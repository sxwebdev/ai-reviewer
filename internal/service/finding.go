package service

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/sxwebdev/ai-reviewer/internal/state"
)

// FindingService handles human decisions on proposed findings. All mutations
// are local; nothing reaches GitLab here.
type FindingService struct {
	db *state.DB
}

// NewFindingService constructs a FindingService.
func NewFindingService(db *state.DB) *FindingService {
	return &FindingService{db: db}
}

// Approve marks a finding approved (eligible for draft creation).
func (s *FindingService) Approve(ctx context.Context, findingID string) error {
	return s.db.UpdateFindingStatus(ctx, findingID, state.FindingApproved, "")
}

// Reject marks a finding rejected with a reason. If saveAsFalsePositive is set,
// a false_positive memory item is recorded so similar findings are suppressed.
func (s *FindingService) Reject(ctx context.Context, findingID, reason string, saveAsFalsePositive bool) error {
	f, err := s.db.GetFinding(ctx, findingID)
	if err != nil {
		return err
	}
	if err := s.db.UpdateFindingStatus(ctx, findingID, state.FindingRejected, reason); err != nil {
		return err
	}
	if saveAsFalsePositive {
		return s.db.UpsertReviewMemory(ctx, &state.ReviewMemory{
			ID:      uuid.NewString(),
			Scope:   state.ScopeProject,
			Type:    state.MemFalsePositive,
			Title:   "False positive: " + f.Title,
			Body:    fmt.Sprintf("Rejected finding in %s (%s). Reason: %s", f.FilePath, f.Category, reason),
			Enabled: true,
			Source:  state.SourceLearned,
		})
	}
	return nil
}

// Edit replaces a finding's comment body.
func (s *FindingService) Edit(ctx context.Context, findingID, body string) error {
	return s.db.SetFindingBody(ctx, findingID, body)
}
