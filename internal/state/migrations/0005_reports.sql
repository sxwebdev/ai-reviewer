-- Deterministic/report artifacts of the review pipeline:
-- risk_json:         serialized review.RiskReport (deterministic risk score)
-- completeness_json: serialized review.CompletenessReport (acceptance-criteria audit)
-- coverage_json:     serialized coverage.Report (changed-line test coverage)
ALTER TABLE reviews ADD COLUMN risk_json TEXT NOT NULL DEFAULT '';
ALTER TABLE reviews ADD COLUMN completeness_json TEXT NOT NULL DEFAULT '';
ALTER TABLE reviews ADD COLUMN coverage_json TEXT NOT NULL DEFAULT '';
