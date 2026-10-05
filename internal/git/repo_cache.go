// Package git manages the local repository cache: a bare mirror per project and
// disposable worktrees per MR head sha, used for agent-mode deep analysis. All
// operations shell out to the system `git`; the cache never writes outside its
// root. Authentication is supplied per-invocation via an http.extraHeader passed
// through the environment (git 2.31+ GIT_CONFIG_*), so the token is never
// embedded in the clone URL — and therefore never persisted to the mirror's
// on-disk config — nor placed in the process argv.
package git

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sync/semaphore"

	"github.com/sxwebdev/ai-reviewer/internal/security"
	"github.com/tkcrm/mx/logger"
)

// Locker serializes work on one repository beyond this process. It is an
// interface, and injected, so this package stays free of a database dependency:
// the natural implementation is a Postgres advisory lock keyed on the string,
// but a single-process deployment can leave it nil.
type Locker interface {
	// Lock blocks until the named lock is held or ctx is done. The returned
	// release func is called exactly once and must be safe on a cancelled ctx.
	Lock(ctx context.Context, key string) (release func(), err error)
}

// Option customises a Cache.
type Option func(*Cache)

// WithLocker adds cross-process exclusion on top of the in-process one. Without
// it a Cache is still safe within its own process, which is what a per-pod
// workdir (the reference deployment mounts an emptyDir) needs; with it, two
// replicas sharing a volume are safe too.
func WithLocker(l Locker) Option {
	return func(c *Cache) { c.locker = l }
}

// Cache manages bare mirrors and worktrees under a root directory.
//
// A mirror is shared mutable state: one scan enqueues a review per candidate MR
// of a repository, so N reviews of the same project start within milliseconds of
// each other and all reach for the same bare clone. Concurrent `git fetch` on
// one mirror does not merely duplicate work — it fails, with
// `cannot lock ref 'refs/heads/…': is at X but expected Y`, and measured at 6
// concurrent fetches, 5 of them lose. The losers degrade to a diff-only review
// (no agent mode, no coverage, no interdiff, skeptic downgraded to reflect), so
// review depth becomes a function of fetch ordering, visible only as a warning.
// Hence: every operation that touches one repository's mirror is serialized on
// that repository.
type Cache struct {
	root   string
	log    logger.Logger
	locker Locker

	// repoLocks is the in-process half: one semaphore per repository, created on
	// first use. Weighted rather than sync.Mutex because acquisition must respect
	// the caller's context — a review whose deadline expires while queued behind
	// another review's fetch has to give up, not block a worker slot.
	mu        sync.Mutex
	repoLocks map[string]*semaphore.Weighted
}

// NewCache builds a Cache rooted at dir.
func NewCache(dir string, log logger.Logger, opts ...Option) *Cache {
	c := &Cache{root: dir, log: log, repoLocks: map[string]*semaphore.Weighted{}}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// lockKey names one repository's mirror. It is also the key handed to the
// injected Locker, so it must identify the repository and nothing narrower —
// two reviews of different head SHAs still share one mirror.
func lockKey(host, projectPath string) string {
	return "git-mirror:" + sanitizeHost(host) + "/" + strings.Trim(projectPath, "/")
}

// lockRepo serializes callers on one repository, in this process first and then
// (if configured) across processes. The returned func releases both, and is
// safe to call from a deferred statement on any path.
func (c *Cache) lockRepo(ctx context.Context, host, projectPath string) (func(), error) {
	key := lockKey(host, projectPath)

	c.mu.Lock()
	sem, ok := c.repoLocks[key]
	if !ok {
		sem = semaphore.NewWeighted(1)
		c.repoLocks[key] = sem
	}
	c.mu.Unlock()

	if err := sem.Acquire(ctx, 1); err != nil {
		return nil, fmt.Errorf("wait for the %s mirror: %w", projectPath, err)
	}
	if c.locker == nil {
		return func() { sem.Release(1) }, nil
	}

	release, err := c.locker.Lock(ctx, key)
	if err != nil {
		sem.Release(1)
		return nil, fmt.Errorf("lock the %s mirror: %w", projectPath, err)
	}
	return func() {
		release()
		sem.Release(1)
	}, nil
}

// mirrorsDir / worktreesDir partition the cache.
func (c *Cache) mirrorsDir() string   { return filepath.Join(c.root, "mirrors") }
func (c *Cache) worktreesDir() string { return filepath.Join(c.root, "worktrees") }

// BareDir returns the bare mirror path for a project.
func (c *Cache) BareDir(host, projectPath string) string {
	return filepath.Join(c.mirrorsDir(), sanitizeHost(host), filepath.FromSlash(projectPath)+".git")
}

// EnsureMirror clones (if missing) or fetches the bare mirror for a project and
// returns its path. cloneURL should be the http(s) URL; token, if non-empty, is
// injected for auth and registered for redaction.
func (c *Cache) EnsureMirror(ctx context.Context, cloneURL, host, projectPath, token string) (string, error) {
	bare := c.BareDir(host, projectPath)
	if token != "" {
		security.RegisterSecret(token)
	}
	auth := gitAuthEnv(token)

	// Held across the Stat too, not just the git call: the cold path is a TOCTOU
	// otherwise, and the loser of a `git clone --mirror` race sees
	// "destination path already exists and is not an empty directory" and then
	// proceeds against a mirror that is still being populated.
	unlock, err := c.lockRepo(ctx, host, projectPath)
	if err != nil {
		return "", err
	}
	defer unlock()

	if _, err := os.Stat(bare); err == nil {
		// Existing mirror: fetch updates.
		if err := c.run(ctx, "", auth, "git", "-C", bare, "fetch", "--prune", "--quiet"); err != nil {
			return "", fmt.Errorf("fetch mirror: %w", err)
		}
		return bare, nil
	}
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		return "", err
	}
	// Clean URL (no token) is what git persists to the mirror config; auth is
	// injected only for this invocation via the environment header.
	if err := c.run(ctx, "", auth, "git", "clone", "--mirror", "--quiet", cloneURL, bare); err != nil {
		return "", fmt.Errorf("clone mirror: %w", err)
	}
	return bare, nil
}

// AddWorktree creates a detached worktree at headSHA and returns its path plus a
// cleanup func. The worktree lives under the cache root; cleanup removes it.
func (c *Cache) AddWorktree(ctx context.Context, host, projectPath, headSHA string) (string, func(), error) {
	bare := c.BareDir(host, projectPath)
	wt := filepath.Join(c.worktreesDir(), sanitizeHost(host), filepath.FromSlash(projectPath), headSHA)

	// Worktrees of different head SHAs live in different directories, but they
	// are all registered in one shared administrative area inside the mirror, so
	// adding and removing them races the same way fetching does.
	unlock, err := c.lockRepo(ctx, host, projectPath)
	if err != nil {
		return "", nil, err
	}
	defer unlock()

	cleanup := func() { c.removeWorktree(host, projectPath, bare, wt) }

	// Reuse an existing worktree if present.
	if _, err := os.Stat(wt); err == nil {
		return wt, cleanup, nil
	}
	if err := os.MkdirAll(filepath.Dir(wt), 0o755); err != nil {
		return "", nil, err
	}
	if err := c.run(ctx, "", nil, "git", "-C", bare, "worktree", "add", "--detach", "--force", wt, headSHA); err != nil {
		return "", nil, fmt.Errorf("add worktree: %w", err)
	}
	return wt, cleanup, nil
}

// DiffRange returns `git diff fromSHA..toSHA` from the project's bare mirror,
// truncated to maxBytes (0 = unlimited). It returns "" without error when
// either commit is absent from the mirror, so callers can degrade silently.
func (c *Cache) DiffRange(ctx context.Context, host, projectPath, fromSHA, toSHA string, maxBytes int) (string, error) {
	bare := c.BareDir(host, projectPath)
	if _, err := os.Stat(bare); err != nil {
		return "", fmt.Errorf("mirror missing: %w", err)
	}
	for _, sha := range []string{fromSHA, toSHA} {
		if !c.hasCommit(ctx, bare, sha) {
			return "", nil
		}
	}
	out, err := c.runOut(ctx, "git", "-C", bare, "diff", "--unified=3", fromSHA+".."+toSHA)
	if err != nil {
		return "", err
	}
	if maxBytes > 0 && len(out) > maxBytes {
		out = out[:maxBytes] + "\n… (interdiff truncated)"
	}
	return out, nil
}

// CommitTouch is one commit's subject plus the paths it touched.
type CommitTouch struct {
	Subject string
	Paths   []string
}

// RecentHistory returns up to maxCommits recent commits (subject + touched
// paths) from the bare mirror. One batched `git log --name-only` process,
// regardless of how many files the caller cares about. Best-effort: a missing
// mirror or git failure returns an error and callers degrade.
func (c *Cache) RecentHistory(ctx context.Context, host, projectPath string, maxCommits int) ([]CommitTouch, error) {
	bare := c.BareDir(host, projectPath)
	if _, err := os.Stat(bare); err != nil {
		return nil, fmt.Errorf("mirror missing: %w", err)
	}
	if maxCommits <= 0 {
		maxCommits = 500
	}
	// NUL-prefixed subject line starts each record; following non-empty lines
	// are the commit's paths.
	out, err := c.runOut(ctx, "git", "-C", bare, "log", "--name-only",
		"--format=%x00%s", "-n", fmt.Sprint(maxCommits), "HEAD")
	if err != nil {
		return nil, err
	}
	var commits []CommitTouch
	for line := range strings.SplitSeq(out, "\n") {
		if rest, ok := strings.CutPrefix(line, "\x00"); ok {
			commits = append(commits, CommitTouch{Subject: rest})
			continue
		}
		line = strings.TrimSpace(line)
		if line == "" || len(commits) == 0 {
			continue
		}
		last := &commits[len(commits)-1]
		last.Paths = append(last.Paths, line)
	}
	return commits, nil
}

// hasCommit reports whether the bare repo contains sha as a commit.
func (c *Cache) hasCommit(ctx context.Context, bare, sha string) bool {
	if sha == "" {
		return false
	}
	return c.run(ctx, "", nil, "git", "-C", bare, "cat-file", "-e", sha+"^{commit}") == nil
}

// runOut executes a git command and returns its stdout, masking secrets in
// error output.
func (c *Cache) runOut(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	if err != nil {
		detail := ""
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			detail = ": " + security.Mask(strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("%w%s", err, detail)
	}
	return string(out), nil
}

// removeWorktreeTimeout bounds the wait for the repository lock during cleanup.
// Cleanup runs on a deferred path with no caller context, and leaving a stale
// worktree behind is far cheaper than blocking a review's return.
const removeWorktreeTimeout = 2 * time.Minute

// removeWorktree detaches and deletes a worktree, refusing paths outside root.
func (c *Cache) removeWorktree(host, projectPath, bare, wt string) {
	if !c.withinRoot(wt) {
		c.log.Errorw("refusing to remove worktree outside cache root", "path", wt)
		return
	}

	// `worktree remove` writes the same administrative area `worktree add` does,
	// so it takes the repository lock like every other mirror mutation.
	ctx, cancel := context.WithTimeout(context.Background(), removeWorktreeTimeout)
	defer cancel()
	unlock, err := c.lockRepo(ctx, host, projectPath)
	if err != nil {
		// The directory is still removed: a leaked worktree would be swept by the
		// cleanup job, but it also holds disk in a 20Gi emptyDir.
		c.log.Warnw("removing a worktree without the repository lock", "path", wt, "err", err)
	} else {
		defer unlock()
	}

	// Best-effort git bookkeeping, then force-remove the directory.
	_ = c.run(context.Background(), "", nil, "git", "-C", bare, "worktree", "remove", "--force", wt)
	if _, err := os.Stat(wt); err == nil {
		_ = os.RemoveAll(wt)
	}
}

// withinRoot reports whether path is inside the cache root (defense against
// path traversal before any deletion).
func (c *Cache) withinRoot(path string) bool {
	root, err1 := filepath.Abs(c.root)
	p, err2 := filepath.Abs(path)
	if err1 != nil || err2 != nil {
		return false
	}
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

// run executes a git command with GIT_TERMINAL_PROMPT disabled plus any extra
// environment (e.g. the auth header), masking secrets in captured output.
func (c *Cache) run(ctx context.Context, dir string, extraEnv []string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	cmd.Env = append(cmd.Env, extraEnv...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// %w, not %s: Signalled has to reach the *exec.ExitError underneath, and
		// "the child was killed" is a different fact from "git refused the work".
		return fmt.Errorf("%w: %s", err, security.Mask(strings.TrimSpace(string(out))))
	}
	return nil
}

// Signalled reports whether a git invocation ended because a *shutdown* signal
// killed the process, rather than because git rejected the work or because
// something else killed the child.
//
// It exists for one case, and that case is not hypothetical: a terminal delivers
// Ctrl-C to the whole process group, so our git children die of SIGINT the instant
// the operator asks the service to stop — before, by microseconds, our own
// cancellation reaches the code that reads the error. Read as an ordinary git
// failure it made review degrade to diff-only and then start a full LLM pass while
// the process was already shutting down.
//
// Only the three signals a shutdown actually delivers count. They mirror
// launcher.ShutdownSiganl()'s set, spelled out here rather than imported so this
// package keeps no dependency on mx — keep the two in step. "Killed by any signal"
// was too wide in the direction that costs money: the OOM killer SIGKILLing a
// fetch of a large mirror is infrastructure trouble, and the caller turns a true
// answer into a discarded attempt row — no backoff entry, a `canceled` metric and
// the same doomed head SHA re-reviewed at full price on every scan. Narrowing
// loses no real shutdown: when the stop arrives through our own context,
// exec.CommandContext kills the child with SIGKILL, but ctx.Err() is then
// context.Canceled and interruptedByShutdown's other half already covers it.
//
// Reading the signal number needs syscall.WaitStatus, because ExitCode() flattens
// every signal — and a never-started child — to -1. syscall is acceptable here:
// darwin and linux are the only targets (the image is Alpine) and both spell
// WaitStatus the same way.
func Signalled(err error) bool {
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return false
	}
	// This guard belongs to the ee.Sys() call below and must not be separated from
	// it: Sys() is promoted from ExitError's embedded *os.ProcessState and
	// dereferences that pointer, so an ExitError built by hand — no process ever
	// ran, no state attached — panics there, and nothing in production code may
	// panic. Wait always attaches a state, which is why the real code path never
	// takes this branch.
	if ee.ProcessState == nil {
		return false
	}
	// ee.Sys() reads the embedded ProcessState the guard above just checked. The
	// comma-ok form cannot fail on darwin or linux; it is here so a future GOOS
	// whose Sys() returns something other than a WaitStatus degrades to "not a
	// shutdown" instead of panicking.
	ws, ok := ee.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		return false
	}
	switch ws.Signal() {
	case syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT:
		return true
	default:
		return false
	}
}

// sanitizeHost turns a host URL into a safe single directory name.
func sanitizeHost(host string) string {
	host = strings.TrimPrefix(host, "https://")
	host = strings.TrimPrefix(host, "http://")
	host = strings.TrimSuffix(host, "/")
	if host == "" {
		host = "unknown-host"
	}
	return host
}

// gitAuthEnv returns the environment entries that inject an HTTP Basic auth
// header for GitLab (username "oauth2", password = token) via git's
// http.extraHeader config, without persisting anything to disk or exposing the
// token in the process argv. Returns nil for an empty token.
func gitAuthEnv(token string) []string {
	if token == "" {
		return nil
	}
	cred := base64.StdEncoding.EncodeToString([]byte("oauth2:" + token))
	return []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.extraHeader",
		"GIT_CONFIG_VALUE_0=Authorization: Basic " + cred,
	}
}
