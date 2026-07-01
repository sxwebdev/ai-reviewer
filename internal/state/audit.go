package state

import "context"

// Audit event kinds.
const (
	AuditCreateDraft = "create_draft"
	AuditPublish     = "publish"
	AuditOverview    = "overview_note"
)

// InsertAuditEvent records an externally-visible action for provability that
// nothing was published without an explicit user action.
func (db *DB) InsertAuditEvent(ctx context.Context, kind, reviewID string, mrID int64, detail string) error {
	_, err := db.ExecContext(ctx,
		`INSERT INTO audit_events (kind, review_id, mr_id, detail, created_at) VALUES (?, ?, ?, ?, ?)`,
		kind, reviewID, mrID, detail, nowMillis())
	return err
}

// ListFindingsByReviewStatus returns a review's findings filtered by status.
func (db *DB) ListFindingsByReviewStatus(ctx context.Context, reviewID, status string) ([]*Finding, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT `+findingColumns+` FROM findings WHERE review_id = ? AND status = ?`, reviewID, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanFindings(rows)
}
