package jobs

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/riverqueue/river"
	"github.com/tkcrm/mx/logger"
)

// Ages after which the sweep considers cached state abandoned.
//
// worktreeTTL is far above the review timeout (30m) on purpose: a worktree
// younger than this may belong to a review that is still running, and deleting
// it out from under the LLM would fail the review for no reason. Anything older
// than six hours cannot belong to a live job, so it is the residue of a SIGKILL
// or an OOM — the case §6.3 names.
//
// mirrorTTL is generous because a mirror is a cache, not garbage: re-cloning a
// large repository is minutes of wall time and bandwidth. A month without a
// single scan means the repository left the config.
const (
	worktreeTTL = 6 * time.Hour
	mirrorTTL   = 30 * 24 * time.Hour
)

// CleanupWorker sweeps review.workdir: orphaned worktrees first, then mirrors
// nothing has fetched in a month. Both live under the layout internal/git owns
// (<root>/mirrors/<host>/<path>.git and <root>/worktrees/<host>/<path>/<sha>).
type CleanupWorker struct {
	river.WorkerDefaults[CleanupArgs]
	log     logger.Logger
	workdir string
}

func (w *CleanupWorker) Timeout(*river.Job[CleanupArgs]) time.Duration { return cleanupTimeout }

func (w *CleanupWorker) Work(ctx context.Context, job *river.Job[CleanupArgs]) error {
	return tracked(job, func() error {
		if w.workdir == "" {
			return nil
		}
		rep, err := sweepWorkdir(ctx, w.workdir, time.Now(), pruneWorktrees)
		// Report what was reclaimed even on a partial failure: the numbers are
		// the only visibility this job has.
		w.log.Infow("workdir swept",
			"operation", "cleanup", "workdir", w.workdir,
			"worktrees_removed", rep.Worktrees, "mirrors_removed", rep.Mirrors,
			"job_id", job.ID)
		return err
	})
}

// sweepReport counts what one sweep reclaimed.
type sweepReport struct {
	Worktrees int
	Mirrors   int
}

// sweepWorkdir is the whole cleanup, separated from the worker so it can be
// tested against a temp directory with no git and no queue. prune is called for
// every mirror whose worktrees were touched, so git's own administrative
// records stop pointing at directories that no longer exist.
func sweepWorkdir(ctx context.Context, root string, now time.Time, prune func(context.Context, string)) (sweepReport, error) {
	var (
		rep  sweepReport
		errs []error
	)

	worktrees := filepath.Join(root, "worktrees")
	mirrors := filepath.Join(root, "mirrors")

	// A git worktree root is identified by a `.git` *file* (not a directory)
	// containing a gitdir pointer — that is what tells an orphaned checkout
	// apart from an ordinary directory somebody left in the workdir. Nothing
	// outside a directory carrying that marker is ever removed.
	pruneNeeded := false
	err := filepath.WalkDir(worktrees, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// A directory that vanished mid-walk (another replica's cleanup, or
			// a review finishing) is not an error worth reporting.
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if !d.IsDir() || path == worktrees {
			return nil
		}
		if !isWorktreeRoot(path) {
			return nil
		}
		// Found a worktree: never descend into a checkout of somebody else's
		// repository looking for more.
		if older(path, now, worktreeTTL) {
			if rmErr := os.RemoveAll(path); rmErr != nil {
				errs = append(errs, fmt.Errorf("remove worktree %s: %w", path, rmErr))
			} else {
				rep.Worktrees++
				pruneNeeded = true
			}
		}
		return fs.SkipDir
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		errs = append(errs, fmt.Errorf("walk worktrees: %w", err))
	}

	// Mirrors: prune the admin records of anything just removed, then drop the
	// mirrors themselves that nothing has fetched in a month.
	err = filepath.WalkDir(mirrors, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if !d.IsDir() || !isBareMirror(path) {
			return nil
		}
		if older(mirrorActivity(path), now, mirrorTTL) {
			if rmErr := os.RemoveAll(path); rmErr != nil {
				errs = append(errs, fmt.Errorf("remove mirror %s: %w", path, rmErr))
			} else {
				rep.Mirrors++
			}
			return fs.SkipDir
		}
		if pruneNeeded && prune != nil {
			prune(ctx, path)
		}
		return fs.SkipDir
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		errs = append(errs, fmt.Errorf("walk mirrors: %w", err))
	}

	return rep, errors.Join(errs...)
}

// isWorktreeRoot reports whether dir is a linked git worktree: a directory
// holding a `.git` file. A bare mirror holds a `.git` directory instead, and an
// ordinary directory holds neither.
func isWorktreeRoot(dir string) bool {
	fi, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil && fi.Mode().IsRegular()
}

// isBareMirror reports whether dir is one of the cache's bare clones.
func isBareMirror(dir string) bool {
	if filepath.Ext(dir) != ".git" {
		return false
	}
	fi, err := os.Stat(filepath.Join(dir, "HEAD"))
	return err == nil && fi.Mode().IsRegular()
}

// mirrorActivity returns the path whose modification time best represents the
// last use of a mirror. git writes FETCH_HEAD on every fetch, while the mirror
// directory's own mtime only changes when an entry is added or removed — a
// mirror fetched daily for a year would otherwise look untouched.
func mirrorActivity(bare string) string {
	fetchHead := filepath.Join(bare, "FETCH_HEAD")
	if _, err := os.Stat(fetchHead); err == nil {
		return fetchHead
	}
	return bare
}

// older reports whether path was last modified more than ttl ago. A path that
// cannot be stat'ed is treated as not old: removing something we cannot inspect
// is the wrong default.
func older(path string, now time.Time, ttl time.Duration) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	return now.Sub(fi.ModTime()) > ttl
}

// pruneWorktrees drops a mirror's records of worktrees that no longer exist on
// disk. Failure is logged nowhere and ignored: the directories are already
// gone, and a stale record only wastes a few bytes until the next sweep.
func pruneWorktrees(ctx context.Context, bare string) {
	cmd := exec.CommandContext(ctx, "git", "-C", bare, "worktree", "prune")
	_ = cmd.Run()
}
