package state

import "context"

// DeleteRepoFiles removes the file index (and its FTS rows) for a project at a
// head sha, so re-indexing starts clean.
func (db *DB) DeleteRepoFiles(ctx context.Context, projectID int64, headSHA string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM files_fts WHERE file_id IN
		 (SELECT id FROM repo_files WHERE project_id = ? AND head_sha = ?)`,
		projectID, headSHA); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM repo_files WHERE project_id = ? AND head_sha = ?`, projectID, headSHA); err != nil {
		return err
	}
	return tx.Commit()
}

// InsertRepoFile inserts an indexed file and mirrors its content into files_fts.
func (db *DB) InsertRepoFile(ctx context.Context, f *RepoFile, content string) error {
	if f.IndexedAt == 0 {
		f.IndexedAt = nowMillis()
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	// RETURNING gives the true row id on both the insert and the ON CONFLICT
	// update path (LastInsertId is unreliable for updates), so files_fts stays
	// correctly keyed.
	var id int64
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO repo_files
			(project_id, head_sha, path, language, package_name, size_bytes, sha256,
			 is_generated, is_vendor, is_test, indexed_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(project_id, head_sha, path) DO UPDATE SET
			language=excluded.language, package_name=excluded.package_name,
			size_bytes=excluded.size_bytes, sha256=excluded.sha256,
			is_generated=excluded.is_generated, is_vendor=excluded.is_vendor,
			is_test=excluded.is_test, indexed_at=excluded.indexed_at
		 RETURNING id`,
		f.ProjectID, f.HeadSHA, f.Path, f.Language, f.PackageName, f.SizeBytes, f.SHA256,
		b2i(f.IsGenerated), b2i(f.IsVendor), b2i(f.IsTest), f.IndexedAt).Scan(&id); err != nil {
		return err
	}
	// Replace any prior FTS row for this file so content never goes stale.
	if _, err := tx.ExecContext(ctx, `DELETE FROM files_fts WHERE file_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO files_fts (file_id, path, content, package_name) VALUES (?, ?, ?, ?)`,
		id, f.Path, content, f.PackageName); err != nil {
		return err
	}
	return tx.Commit()
}

// SearchRepoFiles runs an FTS5 MATCH over indexed file content, best first.
func (db *DB) SearchRepoFiles(ctx context.Context, projectID int64, headSHA, query string, limit int) ([]*RepoFile, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := db.QueryContext(ctx,
		`SELECT rf.id, rf.project_id, rf.head_sha, rf.path, rf.language, rf.package_name,
		        rf.size_bytes, rf.sha256, rf.is_generated, rf.is_vendor, rf.is_test, rf.indexed_at
		 FROM files_fts f
		 JOIN repo_files rf ON rf.id = f.file_id
		 WHERE files_fts MATCH ? AND rf.project_id = ? AND rf.head_sha = ?
		 ORDER BY bm25(files_fts) LIMIT ?`,
		query, projectID, headSHA, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*RepoFile
	for rows.Next() {
		f := &RepoFile{}
		var gen, ven, tst int64
		if err := rows.Scan(&f.ID, &f.ProjectID, &f.HeadSHA, &f.Path, &f.Language, &f.PackageName,
			&f.SizeBytes, &f.SHA256, &gen, &ven, &tst, &f.IndexedAt); err != nil {
			return nil, err
		}
		f.IsGenerated, f.IsVendor, f.IsTest = gen != 0, ven != 0, tst != 0
		out = append(out, f)
	}
	return out, rows.Err()
}
