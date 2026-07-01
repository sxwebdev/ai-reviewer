package state

import "context"

const mrDiffColumns = `id, mr_id, head_sha, old_path, new_path, diff,
	new_file, renamed, deleted, is_binary, is_vendored`

func scanMRDiff(s interface{ Scan(...any) error }) (*MRDiff, error) {
	d := &MRDiff{}
	var newFile, renamed, deleted, isBinary, isVendored int64
	if err := s.Scan(&d.ID, &d.MRID, &d.HeadSHA, &d.OldPath, &d.NewPath, &d.Diff,
		&newFile, &renamed, &deleted, &isBinary, &isVendored); err != nil {
		return nil, err
	}
	d.NewFile, d.Renamed, d.Deleted = newFile != 0, renamed != 0, deleted != 0
	d.IsBinary, d.IsVendored = isBinary != 0, isVendored != 0
	return d, nil
}

// UpsertMRDiff stores one changed file's raw diff for an MR at a head sha. Diffs
// are persisted at review time — including binary and vendored files, which the
// LLM never sees — so the web UI can render the full diff with findings pinned
// inline. Keyed on (mr_id, head_sha, new_path, old_path).
func (db *DB) UpsertMRDiff(ctx context.Context, d *MRDiff) error {
	_, err := db.ExecContext(ctx,
		`INSERT INTO mr_diffs
			(mr_id, head_sha, old_path, new_path, diff, new_file, renamed, deleted, is_binary, is_vendored)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(mr_id, head_sha, new_path, old_path) DO UPDATE SET
			diff=excluded.diff, new_file=excluded.new_file, renamed=excluded.renamed,
			deleted=excluded.deleted, is_binary=excluded.is_binary, is_vendored=excluded.is_vendored`,
		d.MRID, d.HeadSHA, d.OldPath, d.NewPath, d.Diff,
		b2i(d.NewFile), b2i(d.Renamed), b2i(d.Deleted), b2i(d.IsBinary), b2i(d.IsVendored))
	return err
}

// ListMRDiffFiles returns the persisted per-file diffs for an MR at a head sha,
// vendored files sorted last (they render collapsed by default). It returns an
// empty slice — not ErrNotFound — when nothing is stored for that sha, so the
// diff pane can show a clear "not captured" state. Named distinctly from
// gitlab.API.ListMRDiffs to keep the two apart in searches.
func (db *DB) ListMRDiffFiles(ctx context.Context, mrID int64, headSHA string) ([]*MRDiff, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT `+mrDiffColumns+` FROM mr_diffs
		 WHERE mr_id = ? AND head_sha = ?
		 ORDER BY is_vendored ASC, COALESCE(new_path, old_path) ASC`, mrID, headSHA)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*MRDiff
	for rows.Next() {
		d, err := scanMRDiff(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
