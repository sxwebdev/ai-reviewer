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
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sxwebdev/ai-reviewer/internal/security"
)

// Cache manages bare mirrors and worktrees under a root directory.
type Cache struct {
	root string
	log  *slog.Logger
}

// NewCache builds a Cache rooted at dir.
func NewCache(dir string, log *slog.Logger) *Cache {
	return &Cache{root: dir, log: log}
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

	// Reuse an existing worktree if present.
	if _, err := os.Stat(wt); err == nil {
		return wt, func() { c.removeWorktree(bare, wt) }, nil
	}
	if err := os.MkdirAll(filepath.Dir(wt), 0o755); err != nil {
		return "", nil, err
	}
	if err := c.run(ctx, "", nil, "git", "-C", bare, "worktree", "add", "--detach", "--force", wt, headSHA); err != nil {
		return "", nil, fmt.Errorf("add worktree: %w", err)
	}
	return wt, func() { c.removeWorktree(bare, wt) }, nil
}

// removeWorktree detaches and deletes a worktree, refusing paths outside root.
func (c *Cache) removeWorktree(bare, wt string) {
	if !c.withinRoot(wt) {
		c.log.Error("refusing to remove worktree outside cache root", "path", wt)
		return
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
		return fmt.Errorf("%s: %s", err, security.Mask(strings.TrimSpace(string(out))))
	}
	return nil
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
