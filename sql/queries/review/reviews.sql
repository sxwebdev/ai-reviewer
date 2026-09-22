-- GetByHeadSHA answers "has this head SHA already been reviewed?" — the single
-- hot-path query of the scanner. The predicate is written as `status <> 'failed'`
-- to match the partial unique index mr_reviews_success_uniq exactly, which also
-- guarantees at most one row.
-- name: GetByHeadSHA :one
SELECT * FROM mr_reviews
WHERE project_id = @project_id
  AND mr_iid = @mr_iid
  AND head_sha = @head_sha
  AND status <> 'failed'
LIMIT 1;

-- GetLatestByMR returns the most recent non-failed review of an MR regardless
-- of SHA: it is what tells a re-review after a push from a first review, and
-- what the digest renders as "last reviewed".
-- name: GetLatestByMR :one
SELECT * FROM mr_reviews
WHERE project_id = @project_id
  AND mr_iid = @mr_iid
  AND status <> 'failed'
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- FailureStats lives in repo_review/failure_stats.go, not here: sqlc types
-- `max(created_at)` over zero rows as a non-nullable time.Time (or, without the
-- cast, as interface{}), and the whole point of the query is that NULL means
-- "no failures yet".

-- ClaimStalePublications is the safety net of plan section 6.3: reviews whose
-- publish_review job never finished (attempts exhausted, job cancelled by hand,
-- database restored from a backup) stay in status='reviewed' forever. scan_repo
-- picks them up at the end of a pass and re-enqueues publication only — the
-- expensive LLM part is never repeated.
--
-- Every clause after `status = 'reviewed'` is a bound the first version lacked,
-- and each one is load-bearing:
--
--   publish_attempts < @max_attempts  a review that can never be published (MR
--                                     deleted, scope revoked) would otherwise be
--                                     re-enqueued every scan_interval forever
--                                     through a 1-2-worker queue, starving real
--                                     publications. Rows that reach the ceiling
--                                     are retired by AbandonExhaustedPublications.
--   LIMIT @max_rows                   one repository with a backlog must not be
--                                     able to fill the publish queue in one pass.
--   FOR UPDATE SKIP LOCKED            two replicas scanning the same project
--                                     claim disjoint batches instead of both
--                                     bumping the same counters.
--
-- The increment happens in the same statement as the selection, so a claim can
-- never be lost: the counter is what bounds the loop, and a crash after the
-- enqueue but before a separate UPDATE would reset the bound.
-- name: ClaimStalePublications :many
UPDATE mr_reviews
   SET publish_attempts = publish_attempts + 1
 WHERE id IN (
       SELECT r.id FROM mr_reviews r
        WHERE r.project_id = @project_id
          AND r.status = 'reviewed'
          AND r.created_at < @older_than
          AND r.publish_attempts < @max_attempts
        ORDER BY r.created_at
        LIMIT @max_rows
        FOR UPDATE SKIP LOCKED
 )
RETURNING id;

-- AbandonExhaustedPublications retires the reviews ClaimStalePublications has
-- given up on (plan section 13.5's terminal-state rule applied to publication).
--
-- 'abandoned' rather than 'failed' on purpose: the review itself succeeded, so it
-- must keep its slot in mr_reviews_success_uniq — otherwise the scanner would
-- re-review the same head SHA at full LLM price — and it must not count towards
-- the section 6.5 poison-MR ladder, which exists to punish MRs that break the
-- reviewer, not GitLab outages. Being outside 'reviewed' also drops the row out
-- of the sweep, which is the whole point.
-- name: AbandonExhaustedPublications :many
UPDATE mr_reviews
   SET status = 'abandoned',
       error  = @error_text
 WHERE project_id = @project_id
   AND status = 'reviewed'
   AND created_at < @older_than
   AND publish_attempts >= @max_attempts
RETURNING id;

-- Abandon retires one review whose publication failed deterministically — a
-- 4xx that a retry cannot change (the MR was deleted, the project archived, the
-- token lost its api scope). Waiting for the attempt ceiling would spend ten job
-- attempts plus a sweep budget on an answer GitLab already gave.
-- name: Abandon :execrows
UPDATE mr_reviews
   SET status = 'abandoned',
       error  = @error_text
 WHERE id = @id
   AND status = 'reviewed';

-- PromoteDryRun turns a persisted dry run into a publishable review (section 10.3).
--
-- Without it, findings computed while ai_review_publish_enabled was false can
-- never reach GitLab: GetByHeadSHA matches the dry_run row, so NeedsAIReview
-- answers up_to_date and the review is skipped — even for an explicit
-- `review <ref> --publish` — while PublishReview is a deliberate no-op for
-- status='dry_run'. The fingerprint does not depend on the head SHA, so no later
-- push frees them either.
--
-- Promoting rather than re-reviewing is what keeps mr_reviews_success_uniq
-- satisfied (the row stays the single non-failed row for this SHA) and costs no
-- tokens: the findings are already persisted with note_id IS NULL, which is
-- exactly what publish_review consumes. `AND status = 'dry_run'` makes it a
-- compare-and-swap, so two concurrent promoters produce one publication.
-- name: PromoteDryRun :execrows
UPDATE mr_reviews SET status = 'reviewed' WHERE id = @id AND status = 'dry_run';

-- StartAttempt records that a review is about to start spending money.
--
-- The row is born 'failed' on purpose. review.MaxAttempts is 1, so an OOM kill,
-- a SIGKILL or a node eviction during the LLM pass makes River discard the job
-- with nothing written: FailureStats stays at 0 and scan_repo re-runs the same
-- pathological MR at full price every interval — precisely the case section 6.5
-- was written for, and the one it could not see. Writing the row up front makes
-- the crash countable; CompleteAttempt and DiscardAttempt take it back on the
-- two paths where the process survived to say what happened.
--
-- 'failed' rows are excluded from mr_reviews_success_uniq, so an in-flight
-- attempt cannot collide with anything.
-- name: StartAttempt :one
INSERT INTO mr_reviews (project_id, project_path, team, mr_iid, head_sha, status, error, attempt)
VALUES (@project_id, @project_path, @team, @mr_iid, @head_sha, 'failed', @error_text, @attempt)
RETURNING *;

-- DiscardAttempt removes an in-flight attempt row. Two callers, both meaning
-- "this attempt is not a strike against the MR":
--
--   * the section 10.4 transaction, right after the reviewed/dry_run row is
--     inserted — the provisional row and its replacement appear and disappear
--     atomically, so no observer ever sees both or neither;
--   * a review cancelled by our own shutdown (River's SoftStopTimeout, a rolling
--     deploy). Four deploys landing during long reviews would otherwise blacklist
--     a perfectly healthy head SHA through the section 6.5 ladder.
-- name: DiscardAttempt :execrows
DELETE FROM mr_reviews WHERE id = @id AND status = 'failed';

-- FailAttempt turns an in-flight attempt into a recorded failure: the real
-- reason, and created_at moved to now so the section 6.5 ladder measures the
-- wait from when the attempt failed rather than from when it started.
-- name: FailAttempt :exec
UPDATE mr_reviews
   SET error = @error_text,
       created_at = now()
 WHERE id = @id AND status = 'failed';

-- MarkSucceeded is the last step of publish_review: it runs only after the
-- summary note carrying the review marker has been posted, so a 'succeeded' row
-- can never be a false success.
-- name: MarkSucceeded :exec
UPDATE mr_reviews SET status = 'succeeded' WHERE id = @id;
