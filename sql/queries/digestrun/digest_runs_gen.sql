-- name: Create :one
INSERT INTO digest_runs (team, slot, run_date, attempt, status, parts, mr_count, error)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	RETURNING *;

-- name: GetByID :one
SELECT * FROM digest_runs WHERE id=$1 LIMIT 1;
