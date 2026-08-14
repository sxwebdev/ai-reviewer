-- Insert is the only way findings enter the database. The ON CONFLICT clause is
-- mandatory (plan section 5.2): a duplicate fingerprint is an expected outcome —
-- the engine may fail to filter it when reading discussions degrades — and must
-- never abort the transaction that persists the whole review.
--
-- It is DO UPDATE rather than DO NOTHING because of the other half of the
-- section 10.3 trap. A dry-run review A stores fingerprint F with note_id NULL.
-- ListPublishedFingerprints rightly omits F, so the engine emits it again on
-- review B — but under DO NOTHING the row stays attached to A, so
-- ListUnpublishedByReview(B) never sees it and PublishReview(B) posts nothing,
-- while A (status 'dry_run') is never published either. F would be stuck
-- forever: the fingerprint does not depend on the head SHA, so no later push
-- frees it. Re-attaching the row to the newest review is what makes the finding
-- reachable again.
--
-- `WHERE mr_findings.note_id IS NULL` is the counterweight: a row that already
-- reached GitLab stays bound to the review that published it, and its
-- note_id/published_at are never clobbered — that binding is the audit trail and
-- the dedupe key at once. created_at deliberately keeps the first-seen time.
--
-- The NOT EXISTS clause is the second counterweight, and it is what keeps
-- re-attachment from racing publication. `status='reviewed'` means "persisted,
-- publication pending or in flight", and publish uniqueness is ByArgs(review_id)
-- — so two publishers for two reviews of the SAME merge request are not mutually
-- excluded. Without the clause, review B's persist transaction can move A's
-- still-unposted finding to B while publish_review(A) is between its snapshot
-- read and its POST, which produces exactly the two outcomes section 10.4 says
-- are impossible: the same comment posted twice (A posts from its snapshot, B
-- posts because the row is now its own), or — if B is a dry run — a review
-- marked 'succeeded' having posted none of its findings, because they were
-- stolen into a review that will never publish.
--
-- A finding is therefore only re-attached when nobody is going to publish it
-- where it is: dry_run (the section 10.3 trap this DO UPDATE exists for),
-- abandoned (publication gave up) or succeeded-but-unrecorded. Everything else
-- stays put and the caller is told so.
--
-- :execrows therefore reports "this finding is now attached to this review and
-- awaiting publication" (1) versus "somebody else owns it — already published,
-- or attached to a review whose publication is still pending" (0). It is no
-- longer a pure duplicate detector, but the caller's branch is unchanged.
-- name: Insert :execrows
INSERT INTO mr_findings (
    review_id, project_id, mr_iid, fingerprint, severity, category,
    file_path, title, body, position_json, pass, verification
) VALUES (
    @review_id, @project_id, @mr_iid, @fingerprint, @severity, @category,
    @file_path, @title, @body, @position_json, @pass, @verification
)
ON CONFLICT (project_id, mr_iid, fingerprint) DO UPDATE
   SET review_id     = EXCLUDED.review_id,
       severity      = EXCLUDED.severity,
       body          = EXCLUDED.body,
       position_json = EXCLUDED.position_json,
       pass          = EXCLUDED.pass,
       verification  = EXCLUDED.verification
 WHERE mr_findings.note_id IS NULL
   AND NOT EXISTS (
       SELECT 1 FROM mr_reviews r
        WHERE r.id = mr_findings.review_id
          AND r.status = 'reviewed'
   );

-- ListByReview returns every finding a review produced, published or not. It
-- backs the section 11 prior-review continuity block: the next review of the same
-- MR is told what the previous one raised and whether it reached GitLab, so it
-- does not spend tokens re-deriving findings the deterministic fingerprint gate
-- would drop anyway.
-- name: ListByReview :many
SELECT * FROM mr_findings
WHERE review_id = @review_id
ORDER BY created_at, id;

-- ListPublishedFingerprints builds ExistingFingerprints for the engine
-- (plan section 10.3).
--
-- `note_id IS NOT NULL` is load-bearing: dedupe must mean "this finding is
-- already hanging in GitLab", not "we once computed it". Without it every
-- dry-run writes findings with note_id = NULL, and once publishing is switched
-- on the engine treats them as duplicates and never publishes them — the
-- fingerprint does not depend on the head SHA, so new pushes cannot fix it.
-- name: ListPublishedFingerprints :many
SELECT fingerprint FROM mr_findings
WHERE project_id = @project_id
  AND mr_iid = @mr_iid
  AND note_id IS NOT NULL;

-- ListUnpublishedByReview drives the idempotent publish_review job: a retry
-- after a crash mid-way posts only what is still missing.
-- name: ListUnpublishedByReview :many
SELECT * FROM mr_findings
WHERE review_id = @review_id
  AND note_id IS NULL
ORDER BY created_at, id;

-- MarkPublished is written immediately after each successful POST, not batched
-- at the end — that is what makes a crash inside publication cost at most one
-- duplicate-free retry. The cast keeps the parameter a plain int64 even though
-- the column is nullable.
-- name: MarkPublished :exec
UPDATE mr_findings
SET note_id = @note_id::bigint,
    published_at = now()
WHERE id = @id;
