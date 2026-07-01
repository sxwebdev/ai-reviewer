package index

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"go/parser"
	"go/token"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/sxwebdev/ai-reviewer/internal/state"
)

// maxFileBytes caps how much file content is stored/indexed (larger files keep
// metadata only).
const maxFileBytes = 512 * 1024

// Indexer walks a worktree and populates repo_files + files_fts.
type Indexer struct {
	db  *state.DB
	log *slog.Logger
}

// NewIndexer builds an Indexer.
func NewIndexer(db *state.DB, log *slog.Logger) *Indexer { return &Indexer{db: db, log: log} }

// IndexWorktree indexes all reviewable files under root for a project at a head
// sha, replacing any previous index for that sha. Binary and ignored files are
// skipped; vendored/generated/test files are flagged but still recorded.
func (ix *Indexer) IndexWorktree(ctx context.Context, projectID int64, headSHA, root string, ignoreGlobs []string) (int, error) {
	if err := ix.db.DeleteRepoFiles(ctx, projectID, headSHA); err != nil {
		return 0, err
	}
	count := 0
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if ignored(rel, ignoreGlobs) {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.Size() > maxFileBytes {
			return nil // skip large files entirely (would otherwise share an empty-content hash)
		}
		content, err := os.ReadFile(p)
		if err != nil {
			ix.log.Warn("read file failed; skipping", "path", rel, "err", err)
			return nil
		}
		if looksBinary(content) {
			return nil // never index binaries
		}

		sum := sha256.Sum256(content)
		f := &state.RepoFile{
			ProjectID: projectID, HeadSHA: headSHA, Path: rel,
			Language: languageFor(rel), PackageName: goPackage(rel, content),
			SizeBytes: info.Size(), SHA256: hex.EncodeToString(sum[:]),
			IsGenerated: isGenerated(rel, content), IsVendor: isVendorPath(rel), IsTest: isTestPath(rel),
		}
		// Do not FTS-index generated/vendored content (noise), but keep metadata.
		ftsContent := string(content)
		if f.IsGenerated || f.IsVendor {
			ftsContent = ""
		}
		if err := ix.db.InsertRepoFile(ctx, f, ftsContent); err != nil {
			ix.log.Warn("index file failed", "path", rel, "err", err)
			return nil
		}
		count++
		return nil
	})
	return count, err
}

// goPackage returns the Go package name for a .go file, or "".
func goPackage(rel string, content []byte) string {
	if !strings.HasSuffix(rel, ".go") || len(content) == 0 {
		return ""
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, rel, content, parser.PackageClauseOnly)
	if err != nil || f.Name == nil {
		return ""
	}
	return f.Name.Name
}
