-- Multi-pass review pipeline: per-finding provenance (which pass found it,
-- skeptic verification outcome) and per-review pass reports (cost/duration).
ALTER TABLE findings ADD COLUMN pass TEXT NOT NULL DEFAULT '';
ALTER TABLE findings ADD COLUMN verification TEXT NOT NULL DEFAULT '';
ALTER TABLE reviews ADD COLUMN pipeline_json TEXT NOT NULL DEFAULT '';
