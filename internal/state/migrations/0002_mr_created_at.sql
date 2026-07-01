-- Store each MR's GitLab creation time so the UI can show when it was opened
-- and how long it has been open. Existing rows default to 0 (unknown) until the
-- next sync backfills the real value.
ALTER TABLE merge_requests ADD COLUMN created_at INTEGER NOT NULL DEFAULT 0;
