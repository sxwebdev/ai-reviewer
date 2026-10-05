-- name: Create :one
INSERT INTO digest_messages (digest_run_id, part_no, parts_total, channel, payload, status, slack_ts, error, sent_at)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	RETURNING *;

-- name: GetByID :one
SELECT * FROM digest_messages WHERE id=$1 LIMIT 1;
