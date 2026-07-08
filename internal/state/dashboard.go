package state

import "context"

// DashboardRow is a merge-request row enriched with its latest review (if any).
// ReviewHeadSHA is the head the latest review ran against; comparing it to
// HeadSHA tells whether the review is current or the MR moved on.
type DashboardRow struct {
	ID            int64
	ProjectID     int64
	IID           int64
	ProjectPath   string
	Title         string
	Author        string
	Source        string
	Target        string
	CreatedAt     int64 // GitLab MR creation time (0 = unknown)
	HeadSHA       string
	ReviewHeadSHA string // latest review's head sha ("" = never reviewed)
	ReviewedAt    int64  // latest review's creation time (0 = never reviewed)
	RiskLevel     string
	Findings      int
	Drafted       int // findings currently in GitLab draft notes (any review)
	Published     int // findings published to GitLab (any review)
}

// DashboardRows returns tracked MRs, each joined to its most recent review and
// that review's finding count.
func (db *DB) DashboardRows(ctx context.Context) ([]DashboardRow, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT mr.id, mr.project_id, mr.iid,
		        COALESCE(p.path_with_namespace, ''),
		        mr.title, mr.author_username, mr.source_branch, mr.target_branch,
		        COALESCE(mr.created_at, 0), COALESCE(mr.head_sha, ''),
		        COALESCE(rv.head_sha, ''), COALESCE(rv.created_at, 0), COALESCE(rv.risk_level, ''), COALESCE(rv.fcount, 0),
		        (SELECT COUNT(*) FROM findings f JOIN reviews r2 ON f.review_id = r2.id
		           WHERE r2.mr_id = mr.id AND f.status = 'drafted'),
		        (SELECT COUNT(*) FROM findings f JOIN reviews r2 ON f.review_id = r2.id
		           WHERE r2.mr_id = mr.id AND f.status = 'published')
		 FROM merge_requests mr
		 LEFT JOIN projects p
		   ON p.gitlab_host = mr.gitlab_host AND p.project_id = mr.project_id
		 LEFT JOIN (
		   SELECT mr_id, head_sha, created_at, risk_level, fcount FROM (
		     SELECT r.mr_id, r.head_sha, r.created_at, r.risk_level,
		            (SELECT COUNT(*) FROM findings f WHERE f.review_id = r.id) AS fcount,
		            ROW_NUMBER() OVER (PARTITION BY r.mr_id ORDER BY r.created_at DESC, r.id DESC) AS rn
		     FROM reviews r
		   ) WHERE rn = 1
		 ) rv ON rv.mr_id = mr.id
		 ORDER BY mr.updated_at DESC, mr.id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DashboardRow
	for rows.Next() {
		var r DashboardRow
		if err := rows.Scan(&r.ID, &r.ProjectID, &r.IID, &r.ProjectPath, &r.Title, &r.Author,
			&r.Source, &r.Target, &r.CreatedAt, &r.HeadSHA, &r.ReviewHeadSHA, &r.ReviewedAt,
			&r.RiskLevel, &r.Findings, &r.Drafted, &r.Published); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
