-- 0001_init: initial ai-reviewer schema.
-- Timestamps are INTEGER unix-milliseconds (set in Go) to avoid driver-specific
-- time handling. IDs are TEXT UUIDs for externally-referenced rows (reviews,
-- findings, jobs, memory) and INTEGER autoincrement for internal rows.

CREATE TABLE settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE projects (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    gitlab_host         TEXT NOT NULL,
    project_id          INTEGER NOT NULL,
    path_with_namespace TEXT NOT NULL,
    default_branch      TEXT,
    clone_url_http      TEXT,
    web_url             TEXT,
    last_seen_at        INTEGER,
    last_indexed_sha    TEXT,
    UNIQUE (gitlab_host, project_id)
);

CREATE TABLE merge_requests (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    gitlab_host     TEXT NOT NULL,
    project_id      INTEGER NOT NULL,
    iid             INTEGER NOT NULL,
    web_url         TEXT,
    title           TEXT,
    description     TEXT,
    author_username TEXT,
    source_branch   TEXT,
    target_branch   TEXT,
    state           TEXT,
    draft           INTEGER NOT NULL DEFAULT 0,
    head_sha        TEXT,
    base_sha        TEXT,
    start_sha       TEXT,
    updated_at      INTEGER,
    last_seen_at    INTEGER,
    review_status   TEXT,
    UNIQUE (gitlab_host, project_id, iid)
);

CREATE TABLE merge_request_versions (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    mr_id      INTEGER NOT NULL REFERENCES merge_requests(id) ON DELETE CASCADE,
    version_id INTEGER,
    base_sha   TEXT,
    head_sha   TEXT,
    start_sha  TEXT,
    created_at INTEGER,
    UNIQUE (mr_id, version_id)
);

CREATE TABLE mr_diffs (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    mr_id       INTEGER NOT NULL REFERENCES merge_requests(id) ON DELETE CASCADE,
    head_sha    TEXT NOT NULL,
    old_path    TEXT,
    new_path    TEXT,
    diff        TEXT,
    new_file    INTEGER NOT NULL DEFAULT 0,
    renamed     INTEGER NOT NULL DEFAULT 0,
    deleted     INTEGER NOT NULL DEFAULT 0,
    is_binary   INTEGER NOT NULL DEFAULT 0,
    is_vendored INTEGER NOT NULL DEFAULT 0,
    UNIQUE (mr_id, head_sha, new_path, old_path)
);

CREATE TABLE reviews (
    id                     TEXT PRIMARY KEY,
    mr_id                  INTEGER NOT NULL REFERENCES merge_requests(id) ON DELETE CASCADE,
    project_id             INTEGER,
    mr_iid                 INTEGER,
    head_sha               TEXT,
    base_sha               TEXT,
    start_sha              TEXT,
    mode                   TEXT,
    status                 TEXT NOT NULL,
    risk_level             TEXT,
    overall_recommendation TEXT,
    llm_provider           TEXT,
    llm_model              TEXT,
    reviewer_profile_id    TEXT,
    summary                TEXT,
    raw_report_json        TEXT,
    cost_usd               REAL,
    created_at             INTEGER NOT NULL,
    updated_at             INTEGER NOT NULL
);
CREATE INDEX idx_reviews_mr ON reviews (mr_id, head_sha);

CREATE TABLE review_runs (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    review_id   TEXT NOT NULL REFERENCES reviews(id) ON DELETE CASCADE,
    stage       TEXT,
    status      TEXT,
    detail      TEXT,
    started_at  INTEGER,
    finished_at INTEGER
);

CREATE TABLE findings (
    id                   TEXT PRIMARY KEY,
    review_id            TEXT NOT NULL REFERENCES reviews(id) ON DELETE CASCADE,
    mr_id                INTEGER,
    head_sha             TEXT,
    severity             TEXT,
    category             TEXT,
    file_path            TEXT,
    old_path             TEXT,
    new_path             TEXT,
    old_line             INTEGER,
    new_line             INTEGER,
    line_kind            TEXT,
    line_range_start     INTEGER,
    line_range_end       INTEGER,
    title                TEXT,
    body                 TEXT,
    suggestion           TEXT,
    confidence           REAL,
    evidence_json        TEXT,
    fingerprint          TEXT,
    status               TEXT NOT NULL DEFAULT 'proposed',
    rejection_reason     TEXT,
    gitlab_position_json TEXT,
    gitlab_draft_note_id INTEGER,
    gitlab_discussion_id TEXT,
    validation_error     TEXT,
    created_at           INTEGER NOT NULL,
    updated_at           INTEGER NOT NULL,
    UNIQUE (review_id, fingerprint)
);
CREATE INDEX idx_findings_review ON findings (review_id);
CREATE INDEX idx_findings_fp ON findings (mr_id, fingerprint);

CREATE TABLE draft_notes (
    id                   INTEGER PRIMARY KEY AUTOINCREMENT,
    review_id            TEXT REFERENCES reviews(id) ON DELETE CASCADE,
    finding_id           TEXT REFERENCES findings(id) ON DELETE CASCADE,
    gitlab_draft_note_id INTEGER,
    position_json        TEXT,
    note_body            TEXT,
    status               TEXT NOT NULL DEFAULT 'pending',
    error                TEXT,
    created_at           INTEGER NOT NULL,
    updated_at           INTEGER NOT NULL
);

CREATE TABLE posted_comments (
    id                   INTEGER PRIMARY KEY AUTOINCREMENT,
    review_id            TEXT REFERENCES reviews(id) ON DELETE CASCADE,
    finding_id           TEXT REFERENCES findings(id) ON DELETE CASCADE,
    gitlab_note_id       INTEGER,
    gitlab_discussion_id TEXT,
    url                  TEXT,
    created_at           INTEGER NOT NULL
);

CREATE TABLE jobs (
    id               TEXT PRIMARY KEY,
    type             TEXT NOT NULL,
    status           TEXT NOT NULL,
    payload_json     TEXT NOT NULL DEFAULT '',
    project_id       INTEGER,
    mr_iid           INTEGER,
    review_id        TEXT NOT NULL DEFAULT '',
    priority         INTEGER NOT NULL DEFAULT 0,
    attempts         INTEGER NOT NULL DEFAULT 0,
    max_attempts     INTEGER NOT NULL DEFAULT 3,
    run_after        INTEGER NOT NULL DEFAULT 0,
    locked_at        INTEGER,
    locked_by        TEXT NOT NULL DEFAULT '',
    error            TEXT NOT NULL DEFAULT '',
    progress_current INTEGER NOT NULL DEFAULT 0,
    progress_total   INTEGER NOT NULL DEFAULT 0,
    created_at       INTEGER NOT NULL,
    started_at       INTEGER,
    finished_at      INTEGER,
    updated_at       INTEGER NOT NULL
);
CREATE INDEX idx_jobs_claim ON jobs (status, run_after, priority, id);

-- At most one active (queued/running) job per (type, project_id, mr_iid) when
-- both ids are present; NULL ids (sync/index jobs) remain distinct. Backstops
-- the check-then-act dedup in EnqueueReview against concurrent enqueues.
CREATE UNIQUE INDEX idx_jobs_active_dedup
    ON jobs (type, project_id, mr_iid)
    WHERE status IN ('queued', 'running');

CREATE TABLE job_logs (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    job_id     TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    level      TEXT,
    message    TEXT,
    created_at INTEGER NOT NULL
);

CREATE TABLE repo_files (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id   INTEGER NOT NULL,
    head_sha     TEXT NOT NULL,
    path         TEXT NOT NULL,
    language     TEXT,
    package_name TEXT,
    size_bytes   INTEGER,
    sha256       TEXT,
    is_generated INTEGER NOT NULL DEFAULT 0,
    is_vendor    INTEGER NOT NULL DEFAULT 0,
    is_test      INTEGER NOT NULL DEFAULT 0,
    indexed_at   INTEGER NOT NULL,
    UNIQUE (project_id, head_sha, path)
);

CREATE TABLE repo_symbols (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id   INTEGER NOT NULL,
    head_sha     TEXT NOT NULL,
    file_path    TEXT NOT NULL,
    symbol_name  TEXT,
    symbol_kind  TEXT,
    package_name TEXT,
    start_line   INTEGER,
    end_line     INTEGER,
    signature    TEXT,
    exported     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_symbols_name ON repo_symbols (project_id, head_sha, symbol_name);

CREATE TABLE repo_imports (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id  INTEGER NOT NULL,
    head_sha    TEXT NOT NULL,
    file_path   TEXT NOT NULL,
    import_path TEXT NOT NULL
);
CREATE INDEX idx_imports_path ON repo_imports (project_id, head_sha, import_path);

CREATE TABLE repo_references (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id  INTEGER NOT NULL,
    head_sha    TEXT NOT NULL,
    symbol_name TEXT,
    file_path   TEXT,
    line        INTEGER
);

CREATE TABLE repo_index_runs (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id    INTEGER NOT NULL,
    head_sha      TEXT NOT NULL,
    status        TEXT,
    files_indexed INTEGER,
    started_at    INTEGER,
    finished_at   INTEGER
);

CREATE TABLE review_memory (
    id          TEXT PRIMARY KEY,
    scope       TEXT NOT NULL,
    gitlab_host TEXT,
    project_id  INTEGER,
    type        TEXT NOT NULL,
    title       TEXT,
    body        TEXT,
    tags_json   TEXT,
    priority    INTEGER NOT NULL DEFAULT 0,
    enabled     INTEGER NOT NULL DEFAULT 1,
    source      TEXT NOT NULL DEFAULT 'user',
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);

CREATE TABLE reviewer_profiles (
    id         TEXT PRIMARY KEY,
    name       TEXT UNIQUE NOT NULL,
    data_json  TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE prompt_templates (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    scope      TEXT NOT NULL,
    project_id INTEGER,
    name       TEXT NOT NULL,
    body       TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE prompt_events (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    review_id  TEXT,
    kind       TEXT,
    detail     TEXT,
    created_at INTEGER NOT NULL
);

CREATE TABLE false_positive_patterns (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    scope       TEXT NOT NULL,
    gitlab_host TEXT,
    project_id  INTEGER,
    fingerprint TEXT,
    file_glob   TEXT,
    category    TEXT,
    reason      TEXT,
    created_at  INTEGER NOT NULL
);

CREATE TABLE audit_events (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    kind       TEXT NOT NULL,
    review_id  TEXT,
    mr_id      INTEGER,
    detail     TEXT,
    created_at INTEGER NOT NULL
);

-- Full-text search (FTS5). Standalone tables with an UNINDEXED id column that
-- links back to the owning row; the repository layer keeps them in sync.
CREATE VIRTUAL TABLE files_fts USING fts5(
    file_id UNINDEXED, path, content, package_name,
    tokenize = 'porter unicode61'
);

CREATE VIRTUAL TABLE memory_fts USING fts5(
    mem_id UNINDEXED, title, body, tags,
    tokenize = 'porter unicode61'
);

CREATE VIRTUAL TABLE findings_fts USING fts5(
    finding_id UNINDEXED, title, body, file_path,
    tokenize = 'porter unicode61'
);
