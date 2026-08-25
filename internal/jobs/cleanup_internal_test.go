package jobs

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mkWorktree creates a directory that looks exactly like a linked git worktree:
// a `.git` file holding a gitdir pointer.
func mkWorktree(t *testing.T, path string, age time.Duration) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, ".git"), []byte("gitdir: /somewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A file inside proves RemoveAll reached the whole tree, not just the marker.
	if err := os.WriteFile(filepath.Join(path, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	touch(t, path, age)
}

// mkMirror creates a bare mirror: a *.git directory with a HEAD file.
func mkMirror(t *testing.T, path string, age time.Duration) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fetchHead := filepath.Join(path, "FETCH_HEAD")
	if err := os.WriteFile(fetchHead, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	touch(t, fetchHead, age)
	touch(t, path, age)
}

func touch(t *testing.T, path string, age time.Duration) {
	t.Helper()
	when := time.Now().Add(-age)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// TestSweepWorkdirRemovesOnlyAbandonedState is the cleanup job's contract: an
// orphaned worktree is reclaimed, a worktree young enough to belong to a
// running review is not, and nothing that is not a worktree or a mirror is
// touched at all.
func TestSweepWorkdirRemovesOnlyAbandonedState(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	stale := filepath.Join(root, "worktrees", "gitlab", "backend", "payments", "aaaa")
	fresh := filepath.Join(root, "worktrees", "gitlab", "backend", "payments", "bbbb")
	mkWorktree(t, stale, worktreeTTL+time.Hour)
	mkWorktree(t, fresh, time.Minute)

	// Not a worktree: no .git marker. Old, but it is not ours to delete.
	notOurs := filepath.Join(root, "worktrees", "gitlab", "scratch")
	if err := os.MkdirAll(notOurs, 0o755); err != nil {
		t.Fatal(err)
	}
	touch(t, notOurs, 30*24*time.Hour)

	liveMirror := filepath.Join(root, "mirrors", "gitlab", "backend", "payments.git")
	deadMirror := filepath.Join(root, "mirrors", "gitlab", "backend", "retired.git")
	mkMirror(t, liveMirror, time.Hour)
	mkMirror(t, deadMirror, mirrorTTL+24*time.Hour)

	var pruned []string
	rep, err := sweepWorkdir(t.Context(), root, time.Now(), func(_ context.Context, bare string) {
		pruned = append(pruned, bare)
	})
	if err != nil {
		t.Fatalf("sweepWorkdir: %v", err)
	}

	if rep.Worktrees != 1 {
		t.Errorf("worktrees removed = %d, want 1", rep.Worktrees)
	}
	if rep.Mirrors != 1 {
		t.Errorf("mirrors removed = %d, want 1", rep.Mirrors)
	}
	if exists(stale) {
		t.Error("the abandoned worktree survived")
	}
	if !exists(fresh) {
		t.Error("a worktree younger than the TTL was deleted; it may belong to a running review")
	}
	if !exists(notOurs) {
		t.Error("a plain directory with no worktree marker was deleted")
	}
	if !exists(liveMirror) {
		t.Error("a recently fetched mirror was deleted; re-cloning costs minutes")
	}
	if exists(deadMirror) {
		t.Error("a mirror untouched for a month survived")
	}

	// git's own records must be pruned for the mirrors that stayed, and only
	// because a worktree was actually removed.
	if len(pruned) != 1 || pruned[0] != liveMirror {
		t.Errorf("pruned = %v, want [%s]", pruned, liveMirror)
	}
}

// TestSweepWorkdirSkipsPruneWhenNothingWasRemoved keeps the hourly sweep from
// forking git once per mirror for no reason.
func TestSweepWorkdirSkipsPruneWhenNothingWasRemoved(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mkWorktree(t, filepath.Join(root, "worktrees", "gitlab", "a", "sha"), time.Minute)
	mkMirror(t, filepath.Join(root, "mirrors", "gitlab", "a.git"), time.Minute)

	calls := 0
	rep, err := sweepWorkdir(t.Context(), root, time.Now(), func(context.Context, string) { calls++ })
	if err != nil {
		t.Fatal(err)
	}
	if rep.Worktrees != 0 || rep.Mirrors != 0 {
		t.Errorf("nothing was abandoned, yet %+v was reclaimed", rep)
	}
	if calls != 0 {
		t.Errorf("prune ran %d times with nothing removed", calls)
	}
}

// TestSweepWorkdirOnMissingRoot: the workdir does not exist until the first
// review clones something. An hourly job must not fail because of that.
func TestSweepWorkdirOnMissingRoot(t *testing.T) {
	t.Parallel()
	rep, err := sweepWorkdir(t.Context(), filepath.Join(t.TempDir(), "never-created"), time.Now(), nil)
	if err != nil {
		t.Fatalf("a missing workdir must not be an error: %v", err)
	}
	if rep.Worktrees != 0 || rep.Mirrors != 0 {
		t.Errorf("report = %+v", rep)
	}
}

// TestSweepWorkdirDoesNotDescendIntoAWorktree guards against the sweep treating
// a checked-out repository's own nested directories as candidates — reviewed
// repositories can contain anything, including a `.git` file of their own.
func TestSweepWorkdirDoesNotDescendIntoAWorktree(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	wt := filepath.Join(root, "worktrees", "gitlab", "a", "sha")
	mkWorktree(t, wt, time.Minute)

	nested := filepath.Join(wt, "vendor", "submodule")
	mkWorktree(t, nested, 30*24*time.Hour) // old, but inside a live worktree

	rep, err := sweepWorkdir(t.Context(), root, time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Worktrees != 0 {
		t.Errorf("the sweep reached inside a live worktree and removed %d entries", rep.Worktrees)
	}
	if !exists(nested) {
		t.Error("content inside a live worktree was deleted")
	}
}

func TestIsWorktreeRootAndIsBareMirror(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	wt := filepath.Join(root, "wt")
	mkWorktree(t, wt, 0)
	if !isWorktreeRoot(wt) {
		t.Error("a directory with a .git file is a worktree")
	}
	if isBareMirror(wt) {
		t.Error("a worktree is not a bare mirror")
	}

	mirror := filepath.Join(root, "m.git")
	mkMirror(t, mirror, 0)
	if !isBareMirror(mirror) {
		t.Error("a *.git directory with HEAD is a mirror")
	}
	if isWorktreeRoot(mirror) {
		t.Error("a mirror has a .git directory, not a .git file — it is not a worktree root")
	}

	// A repository checkout whose .git is a directory (a normal clone, not a
	// linked worktree) must not be mistaken for either.
	clone := filepath.Join(root, "clone")
	if err := os.MkdirAll(filepath.Join(clone, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if isWorktreeRoot(clone) || isBareMirror(clone) {
		t.Error("a normal clone matched a sweep predicate")
	}

	// A *.git name with no HEAD is not a mirror.
	empty := filepath.Join(root, "empty.git")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	if isBareMirror(empty) {
		t.Error("a *.git directory without HEAD matched")
	}
}

// TestMirrorActivityPrefersFetchHead: the mirror directory's mtime only changes
// when an entry is added or removed, so a mirror fetched daily for a year would
// look untouched and be deleted.
func TestMirrorActivityPrefersFetchHead(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	bare := filepath.Join(root, "m.git")
	mkMirror(t, bare, mirrorTTL+time.Hour)

	// Simulate a fetch: FETCH_HEAD is fresh, the directory itself is not.
	touch(t, filepath.Join(bare, "FETCH_HEAD"), time.Minute)
	if older(mirrorActivity(bare), time.Now(), mirrorTTL) {
		t.Error("a mirror fetched a minute ago was judged stale")
	}

	// Without FETCH_HEAD the directory mtime is the only signal left.
	if err := os.Remove(filepath.Join(bare, "FETCH_HEAD")); err != nil {
		t.Fatal(err)
	}
	touch(t, bare, mirrorTTL+time.Hour)
	if !older(mirrorActivity(bare), time.Now(), mirrorTTL) {
		t.Error("a never-fetched, month-old mirror should be reclaimable")
	}
}

// TestPruneWorktreesDropsDanglingRecords uses a real repository: the sweep
// removes the directory, and without this prune git keeps listing the worktree
// forever and refuses to re-create one at the same path.
func TestPruneWorktreesDropsDanglingRecords(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	src := filepath.Join(root, "src")
	bare := filepath.Join(root, "repo.git")
	wt := filepath.Join(root, "wt")

	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	run(src, "init", "--quiet", "-b", "main")
	if err := os.WriteFile(filepath.Join(src, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(src, "add", "f.txt")
	run(src, "commit", "--quiet", "-m", "init")
	run(root, "clone", "--mirror", "--quiet", src, bare)
	run(bare, "worktree", "add", "--detach", "--quiet", wt, "HEAD")

	// What a SIGKILL leaves behind: the directory is gone, the record is not.
	if err := os.RemoveAll(wt); err != nil {
		t.Fatal(err)
	}
	list := func() string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", "-C", bare, "worktree", "list")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git worktree list: %v\n%s", err, out)
		}
		return string(out)
	}
	if !strings.Contains(list(), "wt") {
		t.Fatal("the dangling record is not there to begin with; the test proves nothing")
	}

	pruneWorktrees(t.Context(), bare)

	if strings.Contains(list(), "wt") {
		t.Errorf("the dangling worktree record survived the prune:\n%s", list())
	}
}

func TestOlderTreatsUnstatableAsNotOld(t *testing.T) {
	t.Parallel()
	if older(filepath.Join(t.TempDir(), "absent"), time.Now(), time.Nanosecond) {
		t.Error("a path that cannot be inspected must never be deleted")
	}
}
