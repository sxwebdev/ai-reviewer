-- Keep Linear issue counts distinct from the historical GitLab MR count.
ALTER TABLE digest_runs
    ADD COLUMN linear_issue_count int NOT NULL DEFAULT 0;
