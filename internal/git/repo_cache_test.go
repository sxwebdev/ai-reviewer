package git

import (
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func initSourceRepo(t *testing.T) (dir, sha string) {
	t.Helper()
	dir = t.TempDir()
	gitRun(t, dir, "init", "-q", "-b", "main")
	gitRun(t, dir, "config", "user.email", "t@example.com")
	gitRun(t, dir, "config", "user.name", "Tester")
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-q", "-m", "init")
	return dir, gitRun(t, dir, "rev-parse", "HEAD")
}

func testCache(t *testing.T) *Cache {
	return NewCache(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestEnsureMirrorAndWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	src, sha := initSourceRepo(t)
	c := testCache(t)
	ctx := t.Context()

	bare, err := c.EnsureMirror(ctx, src, "https://gitlab.test", "group/repo", "")
	if err != nil {
		t.Fatalf("mirror: %v", err)
	}
	if _, err := os.Stat(bare); err != nil {
		t.Fatalf("bare dir missing: %v", err)
	}
	// Fetch path (mirror already exists) must also succeed.
	if _, err := c.EnsureMirror(ctx, src, "https://gitlab.test", "group/repo", ""); err != nil {
		t.Fatalf("re-mirror (fetch): %v", err)
	}

	wt, cleanup, err := c.AddWorktree(ctx, "https://gitlab.test", "group/repo", sha)
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt, "main.go")); err != nil {
		t.Errorf("worktree missing checked-out file: %v", err)
	}
	if !c.withinRoot(wt) {
		t.Error("worktree should be within cache root")
	}

	cleanup()
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Errorf("worktree should be removed after cleanup, stat err=%v", err)
	}
}

func TestDiffRange(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	src, sha1 := initSourceRepo(t)
	if err := os.WriteFile(filepath.Join(src, "main.go"), []byte("package main\n\nfunc main() { println(1) }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, src, "commit", "-aqm", "change main")
	sha2 := gitRun(t, src, "rev-parse", "HEAD")

	c := testCache(t)
	ctx := t.Context()
	if _, err := c.EnsureMirror(ctx, src, "https://gitlab.test", "group/repo", ""); err != nil {
		t.Fatal(err)
	}

	diff, err := c.DiffRange(ctx, "https://gitlab.test", "group/repo", sha1, sha2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff, "println(1)") || !strings.Contains(diff, "main.go") {
		t.Errorf("interdiff content wrong:\n%s", diff)
	}

	// Truncation appends a marker.
	small, err := c.DiffRange(ctx, "https://gitlab.test", "group/repo", sha1, sha2, 20)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(small, "interdiff truncated") {
		t.Errorf("truncated interdiff missing marker: %q", small)
	}

	// Unknown SHA degrades to empty without error.
	empty, err := c.DiffRange(ctx, "https://gitlab.test", "group/repo", strings.Repeat("0", 40), sha2, 0)
	if err != nil || empty != "" {
		t.Errorf("unknown sha must degrade silently, got %q err %v", empty, err)
	}
}

func TestRecentHistory(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	src, _ := initSourceRepo(t)
	// Second commit: fix touching two paths.
	if err := os.WriteFile(filepath.Join(src, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "main.go"), []byte("package main\n\nfunc main() { _ = 1 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, src, "add", ".")
	gitRun(t, src, "commit", "-qm", "fix: broken main and add a")

	c := testCache(t)
	ctx := t.Context()
	if _, err := c.EnsureMirror(ctx, src, "https://gitlab.test", "group/repo", ""); err != nil {
		t.Fatal(err)
	}

	history, err := c.RecentHistory(ctx, "https://gitlab.test", "group/repo", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 {
		t.Fatalf("want 2 commits, got %d: %+v", len(history), history)
	}
	newest := history[0]
	if newest.Subject != "fix: broken main and add a" {
		t.Errorf("subject wrong: %q", newest.Subject)
	}
	if len(newest.Paths) != 2 || newest.Paths[0] != "a.go" || newest.Paths[1] != "main.go" {
		t.Errorf("paths wrong: %v", newest.Paths)
	}
	if history[1].Subject != "init" || len(history[1].Paths) != 1 {
		t.Errorf("oldest commit wrong: %+v", history[1])
	}

	// Missing mirror degrades with an error.
	if _, err := c.RecentHistory(ctx, "https://gitlab.test", "group/missing", 10); err == nil {
		t.Error("missing mirror must error")
	}
}

func TestWithinRoot(t *testing.T) {
	c := NewCache("/tmp/cacheroot", nil)
	if !c.withinRoot("/tmp/cacheroot/worktrees/x") {
		t.Error("path inside root should pass")
	}
	if c.withinRoot("/etc/passwd") {
		t.Error("path outside root must be rejected")
	}
	if c.withinRoot("/tmp/cacheroot/../evil") {
		t.Error("traversal outside root must be rejected")
	}
}

func TestGitAuthEnv(t *testing.T) {
	if gitAuthEnv("") != nil {
		t.Error("empty token should yield no auth env")
	}
	env := gitAuthEnv("sekret-token")
	joined := strings.Join(env, "\n")
	// The raw token must NOT appear (it is base64'd inside a Basic header) and
	// must never be part of a URL that git would persist.
	if strings.Contains(joined, "sekret-token") {
		t.Errorf("raw token leaked into env: %q", joined)
	}
	if !strings.Contains(joined, "http.extraHeader") || !strings.Contains(joined, "Authorization: Basic ") {
		t.Errorf("auth header env missing: %q", joined)
	}
	// base64("oauth2:sekret-token") must be present.
	want := "b2F1dGgyOnNla3JldC10b2tlbg=="
	if !strings.Contains(joined, want) {
		t.Errorf("expected base64 credential %q in %q", want, joined)
	}
}

func TestSanitizeHost(t *testing.T) {
	if got := sanitizeHost("https://gitlab.example.com/"); got != "gitlab.example.com" {
		t.Errorf("sanitizeHost = %q", got)
	}
}
