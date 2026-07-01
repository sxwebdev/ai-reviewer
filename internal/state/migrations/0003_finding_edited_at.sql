-- Track when a finding's body was edited by the reviewer, so the UI can mark
-- edited findings distinctly from untouched proposed ones. 0 = never edited.
ALTER TABLE findings ADD COLUMN edited_at INTEGER NOT NULL DEFAULT 0;
