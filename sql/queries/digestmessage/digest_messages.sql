-- ClaimForSend is step 1 of the slack_send idempotency protocol (section 6.4).
-- chat.postMessage has no idempotency key, so the window "the POST went through
-- but the worker died before recording it" is closed by state in the database.
--
-- The CTE exists because RETURNING hands back the *new* row values, while the
-- whole decision hangs on the status the row had before the claim:
--
--   status_before='sent'                  -> job is a no-op (already delivered)
--   status_before IN ('failed','dry_run') -> job is a no-op (nothing to resend)
--   status_before='pending'               -> ordinary first delivery
--   status_before='sending'               -> a previous attempt died between the
--                                            POST and the result write; the
--                                            message is sent again on purpose
--                                            (a duplicate in the channel is
--                                            noise, a lost digest is a team not
--                                            acting), with a warning and
--                                            slack_resend_uncertain_total.
--
-- FOR UPDATE is what forces the CTE to be materialized rather than inlined, so
-- prev really is the pre-UPDATE snapshot.
--
-- It does NOT serialize two workers into a winner and a loser, and nothing here
-- should be read as if it did: the second claimer blocks on the row lock, then
-- re-reads the *post*-update tuple, sees status='sending' and takes the resend
-- branch — so both would POST. That two claimers never coexist is guaranteed by
-- River's in-flight uniqueness on slack_send (one job per digest_message_id),
-- not by this statement. Anything that introduces a second claimer — a manual
-- re-drive, a sweep for stuck rows, RescueStuckJobsAfter shortened below a job's
-- runtime — is an immediate double-post, not a race that mostly resolves.
-- name: ClaimForSend :one
WITH prev AS (
    SELECT dm.id, dm.status FROM digest_messages dm WHERE dm.id = @id FOR UPDATE
), claimed AS (
    UPDATE digest_messages m
       SET status = 'sending'
      FROM prev
     WHERE m.id = prev.id AND prev.status IN ('pending', 'sending')
    RETURNING m.id
)
SELECT prev.status::text AS status_before,
       ((SELECT count(*) FROM claimed) > 0)::boolean AS claimed
FROM prev;

-- name: MarkSent :exec
UPDATE digest_messages
SET status = 'sent',
    slack_ts = @slack_ts,
    sent_at = now(),
    error = ''
WHERE id = @id;

-- The parameter is @error_text rather than @error so the generated positional
-- argument does not shadow the predeclared `error` identifier.
-- name: MarkFailed :exec
UPDATE digest_messages
SET status = 'failed',
    error = @error_text
WHERE id = @id;

-- ReleaseClaim hands a claimed row back for an ordinary retry, and it is what
-- keeps slack_resend_uncertain_total meaning what section 14.3 says it means.
--
-- 'sending' is supposed to say "a previous attempt died between the POST and the
-- result write" — the one case where a duplicate may really be in the channel.
-- But a retryable delivery failure also left the row at 'sending', so the next
-- attempt logged "previous delivery outcome is unknown" and bumped the counter
-- on `ratelimited`, the most common answer Slack gives and one that explicitly
-- means NOT delivered. The caller therefore releases the claim whenever Slack
-- answered at the application layer, and leaves the row 'sending' only when the
-- outcome is genuinely unknown (transport failure, 5xx, or a crash).
-- name: ReleaseClaim :exec
UPDATE digest_messages
SET status = 'pending',
    error = @error_text
WHERE id = @id AND status = 'sending';

-- name: ListByRun :many
SELECT * FROM digest_messages
WHERE digest_run_id = @digest_run_id
ORDER BY part_no;
