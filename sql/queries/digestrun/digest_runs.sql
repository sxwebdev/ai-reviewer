-- NextAttempt is what `ai-reviewer digest --force` uses: attempt 0 is the
-- scheduled run of a slot (its uniqueness is what stops N replicas doubling it),
-- so an explicit repeat has to take the next free number rather than relax the
-- index.
-- name: NextAttempt :one
SELECT COALESCE(max(attempt) + 1, 0)::int AS next_attempt
FROM digest_runs
WHERE team = @team
  AND run_date = @run_date
  AND slot = @slot;

-- GetBySlot returns the most recent run of a slot ("has this slot been sent
-- today?").
-- name: GetBySlot :one
SELECT * FROM digest_runs
WHERE team = @team
  AND run_date = @run_date
  AND slot = @slot
ORDER BY attempt DESC
LIMIT 1;

-- GetBySlotAttempt addresses one specific run. It is what the scheduled job must
-- use: `digest --force` earlier in the day creates attempt 1, so asking GetBySlot
-- whether the 09:00 slot already ran would answer "yes, attempt 1" and the
-- scheduled attempt 0 would collide with digest_runs_slot_uniq instead of being
-- recognised as a run that has not happened yet.
-- name: GetBySlotAttempt :one
SELECT * FROM digest_runs
WHERE team = @team
  AND run_date = @run_date
  AND slot = @slot
  AND attempt = @attempt
LIMIT 1;

-- SetStatus closes out the BUILD half of a run: 'built' when the payload is
-- assembled and the delivery jobs are queued, 'partial' when some repository
-- could not be inspected, 'dry_run' when nothing may be sent, 'failed' when the
-- build itself broke. The delivery half is SettleDelivery's job.
-- name: SetStatus :exec
UPDATE digest_runs
SET status = @status,
    parts = @parts,
    mr_count = @mr_count,
    linear_issue_count = @linear_issue_count,
    error = @error
WHERE id = @id;

-- SettleDelivery recomputes a run's status from its parts once they have all
-- stopped moving (plan section 13.5: 'sent' — все части подтверждены, 'partial' —
-- часть частей не доставлена).
--
-- Without it nothing ever writes 'sent': the build-time statuses are the only
-- ones a run ever gets, so a digest whose parts all failed is byte-identical to
-- one that was delivered, and `SELECT * FROM digest_runs WHERE status <> 'sent'`
-- — the query the schema invites — returns every row that ever existed. It is
-- called from slack_send after each part settles; the last part to settle is the
-- one whose call finds pending = 0 and writes the answer.
--
-- Three guards make it safe to call from every worker on every part:
--
--   c.pending = 0            a run with parts still 'pending'/'sending' has not
--                            settled; leave the build-time status alone.
--   r.status IN (...)        'dry_run' and 'failed' are terminal build outcomes
--                            and are never delivery states.
--   the 'partial' arm        a run built on incomplete repository data stays
--                            'partial' even when every part is delivered —
--                            promoting it to 'sent' would erase the one fact the
--                            section 6.6 journal exists to record.
-- name: SettleDelivery :many
WITH counts AS (
    SELECT count(*) AS total,
           count(*) FILTER (WHERE status = 'sent') AS sent,
           count(*) FILTER (WHERE status IN ('pending', 'sending')) AS pending
      FROM digest_messages
     WHERE digest_run_id = @id
)
UPDATE digest_runs r
   SET status = CASE
       WHEN c.sent = 0            THEN 'failed'
       WHEN c.sent < c.total      THEN 'partial'
       WHEN r.status = 'partial'  THEN 'partial'
       ELSE 'sent'
   END
  FROM counts c
 WHERE r.id = @id
   AND r.status IN ('built', 'partial')
   AND c.total > 0
   AND c.pending = 0
RETURNING r.status;
