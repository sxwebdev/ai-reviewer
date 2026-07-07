-- Per-review inputs and timing surfaced in the web UI:
-- duration_ms:  wall-clock time the review run took (ms), shown on the MR detail
-- user_context: free-form reviewer-supplied context typed at run time
-- skills_json:  serialized []string of Claude skills selected for the run
ALTER TABLE reviews ADD COLUMN duration_ms INTEGER NOT NULL DEFAULT 0;
ALTER TABLE reviews ADD COLUMN user_context TEXT NOT NULL DEFAULT '';
ALTER TABLE reviews ADD COLUMN skills_json TEXT NOT NULL DEFAULT '';
