-- suppressed_json: serialized []review.SuppressedFinding — findings the pipeline
-- dropped (below threshold, duplicate of a prior finding, skeptic- or
-- verifier-refuted) retained read-only so a real-but-filtered concern is visible
-- instead of silently discarded. Never publishable.
ALTER TABLE reviews ADD COLUMN suppressed_json TEXT NOT NULL DEFAULT '';
