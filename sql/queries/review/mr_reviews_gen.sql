-- name: Create :one
INSERT INTO mr_reviews (project_id, project_path, team, mr_iid, head_sha, base_sha, start_sha, status, findings_count, risk_level, summary, pipeline_json, risk_json, cost_usd, duration_ms, error, attempt, publish_attempts)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)
	RETURNING *;

-- name: GetByID :one
SELECT * FROM mr_reviews WHERE id=$1 LIMIT 1;
