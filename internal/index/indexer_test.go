package index

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/sxwebdev/ai-reviewer/internal/state"
)

func testDB(t *testing.T) *state.DB {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	return db
}

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestIndexWorktree(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "pkg/service.go", "package pkg\n\nfunc Serve() {}\n")
	writeFile(t, root, "pkg/service_test.go", "package pkg\n\nfunc TestServe(t *T) {}\n")
	writeFile(t, root, "vendor/dep/dep.go", "package dep\n\nfunc Vendored() {}\n")
	writeFile(t, root, "api.pb.go", "package api\n// generated\n")
	writeFile(t, root, "README.md", "# Serve\nhello\n")
	writeFile(t, root, "assets/logo.png", "PNG\x00\x00binary")
	writeFile(t, root, "ignoreme.log", "noise")

	db := testDB(t)
	ctx := t.Context()
	ix := NewIndexer(db, slog.New(slog.NewTextHandler(io.Discard, nil)))

	n, err := ix.IndexWorktree(ctx, 1, "sha1", root, []string{"*.log"})
	if err != nil {
		t.Fatal(err)
	}
	// service.go, service_test.go, dep.go, api.pb.go, README.md = 5 (binary + .log skipped)
	if n != 5 {
		t.Errorf("indexed %d files, want 5", n)
	}

	byPath := map[string]*state.RepoFile{}
	for _, f := range mustSearchAll(t, db) {
		byPath[f.Path] = f
	}
	if f := byPath["pkg/service.go"]; f == nil || f.Language != "go" || f.PackageName != "pkg" || f.IsTest {
		t.Errorf("service.go classified wrong: %+v", f)
	}
	if f := byPath["pkg/service_test.go"]; f == nil || !f.IsTest {
		t.Errorf("service_test.go should be is_test: %+v", f)
	}
	if f := byPath["vendor/dep/dep.go"]; f == nil || !f.IsVendor {
		t.Errorf("vendored file should be is_vendor: %+v", f)
	}
	if f := byPath["api.pb.go"]; f == nil || !f.IsGenerated {
		t.Errorf("pb.go should be is_generated: %+v", f)
	}

	// FTS: "Serve" appears in service.go and README, but vendored content is not indexed.
	hits, err := db.SearchRepoFiles(ctx, 1, "sha1", "Serve", 10)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, h := range hits {
		paths = append(paths, h.Path)
	}
	if !contains(paths, "pkg/service.go") {
		t.Errorf("FTS should find service.go, got %v", paths)
	}
	if contains(paths, "vendor/dep/dep.go") {
		t.Errorf("vendored content must not be FTS-indexed, got %v", paths)
	}

	// Re-indexing replaces the previous index (no duplicates).
	n2, err := ix.IndexWorktree(ctx, 1, "sha1", root, []string{"*.log"})
	if err != nil || n2 != 5 {
		t.Fatalf("re-index n=%d err=%v", n2, err)
	}
	if all := mustSearchAll(t, db); len(all) != 5 {
		t.Errorf("re-index left %d rows, want 5", len(all))
	}
}

func mustSearchAll(t *testing.T, db *state.DB) []*state.RepoFile {
	t.Helper()
	rows, err := db.QueryContext(t.Context(),
		`SELECT id, project_id, head_sha, path, language, package_name, size_bytes, sha256,
		        is_generated, is_vendor, is_test, indexed_at FROM repo_files`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []*state.RepoFile
	for rows.Next() {
		f := &state.RepoFile{}
		var g, v, ts int64
		if err := rows.Scan(&f.ID, &f.ProjectID, &f.HeadSHA, &f.Path, &f.Language, &f.PackageName,
			&f.SizeBytes, &f.SHA256, &g, &v, &ts, &f.IndexedAt); err != nil {
			t.Fatal(err)
		}
		f.IsGenerated, f.IsVendor, f.IsTest = g != 0, v != 0, ts != 0
		out = append(out, f)
	}
	return out
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
