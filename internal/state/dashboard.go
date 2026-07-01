package state

import "context"

// DashboardRow is a merge-request row enriched for the dashboard.
type DashboardRow struct {
	ID           int64
	ProjectPath  string
	Title        string
	Author       string
	Source       string
	Target       string
	ReviewStatus string
	Findings     int
}

// DashboardRows returns tracked MRs with project path and finding counts.
func (db *DB) DashboardRows(ctx context.Context) ([]DashboardRow, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT mr.id,
		        COALESCE(p.path_with_namespace, ''),
		        mr.title, mr.author_username, mr.source_branch, mr.target_branch,
		        COALESCE(mr.review_status, ''),
		        (SELECT COUNT(*) FROM findings f
		           JOIN reviews rv ON f.review_id = rv.id
		          WHERE rv.mr_id = mr.id) AS findings
		 FROM merge_requests mr
		 LEFT JOIN projects p
		   ON p.gitlab_host = mr.gitlab_host AND p.project_id = mr.project_id
		 ORDER BY mr.updated_at DESC, mr.id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DashboardRow
	for rows.Next() {
		var r DashboardRow
		if err := rows.Scan(&r.ID, &r.ProjectPath, &r.Title, &r.Author, &r.Source, &r.Target, &r.ReviewStatus, &r.Findings); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
