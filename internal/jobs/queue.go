package jobs

import (
	"context"
	"encoding/json"

	"github.com/sxwebdev/ai-reviewer/internal/state"
)

// ReviewPayload identifies the MR to review by its local id, plus any
// per-trigger inputs the user supplied when starting the review.
type ReviewPayload struct {
	MRLocalID int64 `json:"mr_local_id"`
	// UserContext is free-form context typed at run time (rendered into the
	// prompt). Skills are the Claude skills selected for this run.
	// RememberContext asks that UserContext be saved to project review memory.
	UserContext     string   `json:"user_context,omitempty"`
	Skills          []string `json:"skills,omitempty"`
	RememberContext bool     `json:"remember_context,omitempty"`
}

// ReviewRequest carries the optional per-trigger inputs to EnqueueReview.
type ReviewRequest struct {
	UserContext     string
	Skills          []string
	RememberContext bool
}

// EnqueueReview enqueues a review job for a locally-tracked MR. It is a no-op
// (returns "", nil) if an active review job already exists for the MR.
func EnqueueReview(ctx context.Context, db *state.DB, mrLocalID, projectID, iid int64, req ReviewRequest) (string, error) {
	active, err := db.HasActiveJob(ctx, state.JobReview, projectID, iid)
	if err != nil {
		return "", err
	}
	if active {
		return "", nil
	}
	payload, _ := json.Marshal(ReviewPayload{
		MRLocalID:       mrLocalID,
		UserContext:     req.UserContext,
		Skills:          req.Skills,
		RememberContext: req.RememberContext,
	})
	j := &state.Job{
		Type:        state.JobReview,
		PayloadJSON: string(payload),
		ProjectID:   &projectID,
		MRIID:       &iid,
		Priority:    1,
	}
	if err := db.EnqueueJob(ctx, j); err != nil {
		// A concurrent enqueue won the race and the partial unique index
		// rejected this duplicate — treat as already active.
		if state.IsUniqueViolation(err) {
			return "", nil
		}
		return "", err
	}
	return j.ID, nil
}

// EnqueueSync enqueues a one-shot sync job.
func EnqueueSync(ctx context.Context, db *state.DB) (string, error) {
	j := &state.Job{Type: state.JobSync}
	if err := db.EnqueueJob(ctx, j); err != nil {
		return "", err
	}
	return j.ID, nil
}

// DecodeReviewPayload extracts the review payload from a job.
func DecodeReviewPayload(job *state.Job) (ReviewPayload, error) {
	var p ReviewPayload
	err := json.Unmarshal([]byte(job.PayloadJSON), &p)
	return p, err
}
