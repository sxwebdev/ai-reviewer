package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrNotFound is returned when a lookup matches no row.
var ErrNotFound = errors.New("not found")

// IsUniqueViolation reports whether err is a SQLite UNIQUE constraint failure,
// used to treat a lost enqueue race as a benign no-op.
func IsUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func nowMillis() int64 { return time.Now().UnixMilli() }

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// prefixColumns rewrites a comma-separated column list to qualify each column
// with a table alias, e.g. prefixColumns("rm", "id, title") -> "rm.id, rm.title".
func prefixColumns(alias, columns string) string {
	parts := strings.Split(columns, ",")
	for i, p := range parts {
		parts[i] = alias + "." + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}

// ---- settings ----

// SetSetting upserts a key/value setting.
func (db *DB) SetSetting(ctx context.Context, key, value string) error {
	_, err := db.ExecContext(ctx,
		`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, nowMillis())
	return err
}

// GetSetting returns the value for key and whether it exists.
func (db *DB) GetSetting(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

// ---- projects ----

// UpsertProject inserts or updates a project by (gitlab_host, project_id) and
// returns its local id.
func (db *DB) UpsertProject(ctx context.Context, p *Project) (int64, error) {
	_, err := db.ExecContext(ctx,
		`INSERT INTO projects
			(gitlab_host, project_id, path_with_namespace, default_branch, clone_url_http, web_url, last_seen_at, last_indexed_sha)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(gitlab_host, project_id) DO UPDATE SET
			path_with_namespace = excluded.path_with_namespace,
			default_branch      = excluded.default_branch,
			clone_url_http      = excluded.clone_url_http,
			web_url             = excluded.web_url,
			last_seen_at        = excluded.last_seen_at`,
		p.GitLabHost, p.ProjectID, p.PathWithNamespace, p.DefaultBranch,
		p.CloneURLHTTP, p.WebURL, nowMillis(), p.LastIndexedSHA)
	if err != nil {
		return 0, err
	}
	var id int64
	err = db.QueryRowContext(ctx,
		`SELECT id FROM projects WHERE gitlab_host = ? AND project_id = ?`,
		p.GitLabHost, p.ProjectID).Scan(&id)
	return id, err
}

// GetProjectByGitLabID looks up a project by host and GitLab numeric id.
func (db *DB) GetProjectByGitLabID(ctx context.Context, host string, projectID int64) (*Project, error) {
	p := &Project{}
	err := db.QueryRowContext(ctx,
		`SELECT id, gitlab_host, project_id, path_with_namespace, default_branch,
		        clone_url_http, web_url, last_seen_at, last_indexed_sha
		 FROM projects WHERE gitlab_host = ? AND project_id = ?`,
		host, projectID).Scan(
		&p.ID, &p.GitLabHost, &p.ProjectID, &p.PathWithNamespace, &p.DefaultBranch,
		&p.CloneURLHTTP, &p.WebURL, &p.LastSeenAt, &p.LastIndexedSHA)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

// ---- merge requests ----

// UpsertMergeRequest inserts or updates an MR by (host, project_id, iid) and
// returns its local id. created_at is preserved when the incoming value is 0: a
// partial upsert from the review flow — which does not carry the MR's creation
// time — can't clobber the stored value, while a real value from sync still
// heals a row that a prior partial upsert had zeroed. review_status is likewise
// preserved across upserts (never in the update set).
func (db *DB) UpsertMergeRequest(ctx context.Context, mr *MergeRequest) (int64, error) {
	now := nowMillis()
	_, err := db.ExecContext(ctx,
		`INSERT INTO merge_requests
			(gitlab_host, project_id, iid, web_url, title, description, author_username,
			 source_branch, target_branch, state, draft, head_sha, base_sha, start_sha,
			 created_at, updated_at, last_seen_at, review_status)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(gitlab_host, project_id, iid) DO UPDATE SET
			web_url         = excluded.web_url,
			title           = excluded.title,
			description     = excluded.description,
			author_username = excluded.author_username,
			source_branch   = excluded.source_branch,
			target_branch   = excluded.target_branch,
			state           = excluded.state,
			draft           = excluded.draft,
			head_sha        = excluded.head_sha,
			base_sha        = excluded.base_sha,
			start_sha       = excluded.start_sha,
			created_at      = COALESCE(NULLIF(excluded.created_at, 0), created_at),
			updated_at      = excluded.updated_at,
			last_seen_at    = excluded.last_seen_at`,
		mr.GitLabHost, mr.ProjectID, mr.IID, mr.WebURL, mr.Title, mr.Description, mr.AuthorUsername,
		mr.SourceBranch, mr.TargetBranch, mr.State, b2i(mr.Draft), mr.HeadSHA, mr.BaseSHA, mr.StartSHA,
		mr.CreatedAt, mr.UpdatedAt, now, mr.ReviewStatus)
	if err != nil {
		return 0, err
	}
	var id int64
	err = db.QueryRowContext(ctx,
		`SELECT id FROM merge_requests WHERE gitlab_host = ? AND project_id = ? AND iid = ?`,
		mr.GitLabHost, mr.ProjectID, mr.IID).Scan(&id)
	return id, err
}

const mrColumns = `id, gitlab_host, project_id, iid, web_url, title, description, author_username,
	source_branch, target_branch, state, draft, head_sha, base_sha, start_sha,
	created_at, updated_at, last_seen_at, review_status`

func scanMR(s interface{ Scan(...any) error }) (*MergeRequest, error) {
	mr := &MergeRequest{}
	var draft int64
	err := s.Scan(&mr.ID, &mr.GitLabHost, &mr.ProjectID, &mr.IID, &mr.WebURL, &mr.Title,
		&mr.Description, &mr.AuthorUsername, &mr.SourceBranch, &mr.TargetBranch, &mr.State,
		&draft, &mr.HeadSHA, &mr.BaseSHA, &mr.StartSHA, &mr.CreatedAt, &mr.UpdatedAt, &mr.LastSeenAt, &mr.ReviewStatus)
	if err != nil {
		return nil, err
	}
	mr.Draft = draft != 0
	return mr, nil
}

// ListMergeRequests returns all tracked MRs, most-recently-updated first.
func (db *DB) ListMergeRequests(ctx context.Context) ([]*MergeRequest, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT `+mrColumns+` FROM merge_requests ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*MergeRequest
	for rows.Next() {
		mr, err := scanMR(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, mr)
	}
	return out, rows.Err()
}

// ListOpenMergeRequests returns tracked MRs for a host that are still open
// (state opened/locked/unknown), most-recently-updated first. Used by sync
// reconciliation, which only cares about MRs that might have transitioned to
// merged/closed — terminal rows are already settled and need no re-fetch.
func (db *DB) ListOpenMergeRequests(ctx context.Context, host string) ([]*MergeRequest, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT `+mrColumns+` FROM merge_requests
		 WHERE gitlab_host = ? AND (state IS NULL OR state IN ('', 'opened', 'locked'))
		 ORDER BY updated_at DESC`, host)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*MergeRequest
	for rows.Next() {
		mr, err := scanMR(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, mr)
	}
	return out, rows.Err()
}

// GetMergeRequest returns an MR by local id.
func (db *DB) GetMergeRequest(ctx context.Context, id int64) (*MergeRequest, error) {
	mr, err := scanMR(db.QueryRowContext(ctx, `SELECT `+mrColumns+` FROM merge_requests WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return mr, err
}

// GetMergeRequestByIID returns an MR by its GitLab identity.
func (db *DB) GetMergeRequestByIID(ctx context.Context, host string, projectID, iid int64) (*MergeRequest, error) {
	mr, err := scanMR(db.QueryRowContext(ctx,
		`SELECT `+mrColumns+` FROM merge_requests WHERE gitlab_host = ? AND project_id = ? AND iid = ?`,
		host, projectID, iid))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return mr, err
}

// ---- review memory (with FTS5) ----

// UpsertReviewMemory inserts or updates a memory item and keeps memory_fts in
// sync.
func (db *DB) UpsertReviewMemory(ctx context.Context, m *ReviewMemory) error {
	now := nowMillis()
	if m.CreatedAt == 0 {
		m.CreatedAt = now
	}
	m.UpdatedAt = now
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO review_memory
			(id, scope, gitlab_host, project_id, type, title, body, tags_json, priority, enabled, source, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
			scope=excluded.scope, gitlab_host=excluded.gitlab_host, project_id=excluded.project_id,
			type=excluded.type, title=excluded.title, body=excluded.body, tags_json=excluded.tags_json,
			priority=excluded.priority, enabled=excluded.enabled, source=excluded.source,
			updated_at=excluded.updated_at`,
		m.ID, m.Scope, m.GitLabHost, m.ProjectID, m.Type, m.Title, m.Body, m.TagsJSON,
		m.Priority, b2i(m.Enabled), m.Source, m.CreatedAt, m.UpdatedAt); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_fts WHERE mem_id = ?`, m.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO memory_fts (mem_id, title, body, tags) VALUES (?, ?, ?, ?)`,
		m.ID, m.Title, m.Body, m.TagsJSON); err != nil {
		return err
	}
	return tx.Commit()
}

const memColumns = `id, scope, gitlab_host, project_id, type, title, body, tags_json, priority, enabled, source, created_at, updated_at`

func scanMemory(s interface{ Scan(...any) error }) (*ReviewMemory, error) {
	m := &ReviewMemory{}
	var enabled int64
	err := s.Scan(&m.ID, &m.Scope, &m.GitLabHost, &m.ProjectID, &m.Type, &m.Title, &m.Body,
		&m.TagsJSON, &m.Priority, &enabled, &m.Source, &m.CreatedAt, &m.UpdatedAt)
	if err != nil {
		return nil, err
	}
	m.Enabled = enabled != 0
	return m, nil
}

// ListReviewMemory returns enabled memory items ordered by priority.
func (db *DB) ListReviewMemory(ctx context.Context) ([]*ReviewMemory, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT `+memColumns+` FROM review_memory ORDER BY priority DESC, updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ReviewMemory
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SearchReviewMemory does an FTS5 MATCH over memory, best matches first.
func (db *DB) SearchReviewMemory(ctx context.Context, query string, limit int) ([]*ReviewMemory, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := db.QueryContext(ctx,
		`SELECT `+prefixColumns("rm", memColumns)+`
		 FROM memory_fts f
		 JOIN review_memory rm ON rm.id = f.mem_id
		 WHERE memory_fts MATCH ? AND rm.enabled = 1
		 ORDER BY bm25(memory_fts), rm.priority DESC
		 LIMIT ?`,
		query, limit)
	if err != nil {
		return nil, fmt.Errorf("memory fts query: %w", err)
	}
	defer rows.Close()
	var out []*ReviewMemory
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
