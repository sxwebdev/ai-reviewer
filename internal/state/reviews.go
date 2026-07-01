package state

import (
	"context"
	"database/sql"
	"errors"
)

// ---- reviews ----

// CreateReview inserts a new review row.
func (db *DB) CreateReview(ctx context.Context, r *Review) error {
	now := nowMillis()
	if r.CreatedAt == 0 {
		r.CreatedAt = now
	}
	r.UpdatedAt = now
	_, err := db.ExecContext(ctx,
		`INSERT INTO reviews
			(id, mr_id, project_id, mr_iid, head_sha, base_sha, start_sha, mode, status,
			 risk_level, overall_recommendation, llm_provider, llm_model, reviewer_profile_id,
			 summary, raw_report_json, cost_usd, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.MRID, r.ProjectID, r.MRIID, r.HeadSHA, r.BaseSHA, r.StartSHA, r.Mode, r.Status,
		r.RiskLevel, r.OverallRecommendation, r.LLMProvider, r.LLMModel, r.ReviewerProfileID,
		r.Summary, r.RawReportJSON, r.CostUSD, r.CreatedAt, r.UpdatedAt)
	return err
}

// UpdateReviewStatus sets a review's status.
func (db *DB) UpdateReviewStatus(ctx context.Context, id, status string) error {
	_, err := db.ExecContext(ctx,
		`UPDATE reviews SET status = ?, updated_at = ? WHERE id = ?`,
		status, nowMillis(), id)
	return err
}

const reviewColumns = `id, mr_id, project_id, mr_iid, head_sha, base_sha, start_sha, mode, status,
	risk_level, overall_recommendation, llm_provider, llm_model, reviewer_profile_id,
	summary, raw_report_json, cost_usd, created_at, updated_at`

func scanReview(s interface{ Scan(...any) error }) (*Review, error) {
	r := &Review{}
	err := s.Scan(&r.ID, &r.MRID, &r.ProjectID, &r.MRIID, &r.HeadSHA, &r.BaseSHA, &r.StartSHA,
		&r.Mode, &r.Status, &r.RiskLevel, &r.OverallRecommendation, &r.LLMProvider, &r.LLMModel,
		&r.ReviewerProfileID, &r.Summary, &r.RawReportJSON, &r.CostUSD, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return r, nil
}

// GetReview returns a review by id.
func (db *DB) GetReview(ctx context.Context, id string) (*Review, error) {
	r, err := scanReview(db.QueryRowContext(ctx, `SELECT `+reviewColumns+` FROM reviews WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

// ListReviewsByMR returns reviews for an MR, newest first.
func (db *DB) ListReviewsByMR(ctx context.Context, mrID int64) ([]*Review, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT `+reviewColumns+` FROM reviews WHERE mr_id = ? ORDER BY created_at DESC`, mrID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Review
	for rows.Next() {
		r, err := scanReview(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- findings ----

// InsertFinding inserts a finding and mirrors it into findings_fts.
func (db *DB) InsertFinding(ctx context.Context, f *Finding) error {
	now := nowMillis()
	if f.CreatedAt == 0 {
		f.CreatedAt = now
	}
	f.UpdatedAt = now
	if f.Status == "" {
		f.Status = FindingProposed
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	// DO NOTHING on the (review_id, fingerprint) unique index: a repeated
	// fingerprint within a review is a benign no-op rather than a hard error.
	res, err := tx.ExecContext(ctx,
		`INSERT INTO findings
			(id, review_id, mr_id, head_sha, severity, category, file_path, old_path, new_path,
			 old_line, new_line, line_kind, line_range_start, line_range_end, title, body, suggestion,
			 confidence, evidence_json, fingerprint, status, rejection_reason, gitlab_position_json,
			 gitlab_draft_note_id, gitlab_discussion_id, validation_error, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(review_id, fingerprint) DO NOTHING`,
		f.ID, f.ReviewID, f.MRID, f.HeadSHA, f.Severity, f.Category, f.FilePath, f.OldPath, f.NewPath,
		f.OldLine, f.NewLine, f.LineKind, f.LineRangeStart, f.LineRangeEnd, f.Title, f.Body, f.Suggestion,
		f.Confidence, f.EvidenceJSON, f.Fingerprint, f.Status, f.RejectionReason, f.GitLabPositionJSON,
		f.GitLabDraftNoteID, f.GitLabDiscussionID, f.ValidationError, f.CreatedAt, f.UpdatedAt)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return tx.Commit() // duplicate fingerprint — nothing inserted, skip FTS
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO findings_fts (finding_id, title, body, file_path) VALUES (?, ?, ?, ?)`,
		f.ID, f.Title, f.Body, f.FilePath); err != nil {
		return err
	}
	return tx.Commit()
}

const findingColumns = `id, review_id, mr_id, head_sha, severity, category, file_path, old_path, new_path,
	old_line, new_line, line_kind, line_range_start, line_range_end, title, body, suggestion,
	confidence, evidence_json, fingerprint, status, rejection_reason, gitlab_position_json,
	gitlab_draft_note_id, gitlab_discussion_id, validation_error, created_at, updated_at`

func scanFinding(s interface{ Scan(...any) error }) (*Finding, error) {
	f := &Finding{}
	err := s.Scan(&f.ID, &f.ReviewID, &f.MRID, &f.HeadSHA, &f.Severity, &f.Category, &f.FilePath,
		&f.OldPath, &f.NewPath, &f.OldLine, &f.NewLine, &f.LineKind, &f.LineRangeStart, &f.LineRangeEnd,
		&f.Title, &f.Body, &f.Suggestion, &f.Confidence, &f.EvidenceJSON, &f.Fingerprint, &f.Status,
		&f.RejectionReason, &f.GitLabPositionJSON, &f.GitLabDraftNoteID, &f.GitLabDiscussionID,
		&f.ValidationError, &f.CreatedAt, &f.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// ListFindingsByReview returns a review's findings ordered by severity.
func (db *DB) ListFindingsByReview(ctx context.Context, reviewID string) ([]*Finding, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT `+findingColumns+` FROM findings WHERE review_id = ?
		 ORDER BY CASE severity
		   WHEN 'blocking' THEN 5 WHEN 'high' THEN 4 WHEN 'medium' THEN 3
		   WHEN 'low' THEN 2 WHEN 'nit' THEN 1 ELSE 0 END DESC, confidence DESC`, reviewID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanFindings(rows)
}

// ListFindingsByMR returns all findings ever produced for an MR (any review).
func (db *DB) ListFindingsByMR(ctx context.Context, mrID int64) ([]*Finding, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+findingColumns+` FROM findings WHERE mr_id = ?`, mrID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanFindings(rows)
}

func scanFindings(rows *sql.Rows) ([]*Finding, error) {
	var out []*Finding
	for rows.Next() {
		f, err := scanFinding(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// GetFinding returns a finding by id.
func (db *DB) GetFinding(ctx context.Context, id string) (*Finding, error) {
	f, err := scanFinding(db.QueryRowContext(ctx, `SELECT `+findingColumns+` FROM findings WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return f, err
}

// UpdateFindingStatus sets a finding's status (and optional rejection reason).
func (db *DB) UpdateFindingStatus(ctx context.Context, id, status, rejectionReason string) error {
	_, err := db.ExecContext(ctx,
		`UPDATE findings SET status = ?, rejection_reason = ?, updated_at = ? WHERE id = ?`,
		status, rejectionReason, nowMillis(), id)
	return err
}

// SetFindingBody updates a finding's edited body.
func (db *DB) SetFindingBody(ctx context.Context, id, body string) error {
	_, err := db.ExecContext(ctx,
		`UPDATE findings SET body = ?, updated_at = ? WHERE id = ?`, body, nowMillis(), id)
	return err
}

// SetFindingDraftNote records the created GitLab draft note id and status.
func (db *DB) SetFindingDraftNote(ctx context.Context, id string, draftNoteID int64, status string) error {
	_, err := db.ExecContext(ctx,
		`UPDATE findings SET gitlab_draft_note_id = ?, status = ?, updated_at = ? WHERE id = ?`,
		draftNoteID, status, nowMillis(), id)
	return err
}
