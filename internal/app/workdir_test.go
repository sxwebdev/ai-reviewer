package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureWorkdirWritable(t *testing.T) {
	t.Parallel()

	t.Run("creates a missing directory", func(t *testing.T) {
		t.Parallel()
		dir := filepath.Join(t.TempDir(), "nested", "work")
		if err := ensureWorkdirWritable(dir); err != nil {
			t.Fatalf("ensureWorkdirWritable: %v", err)
		}
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("directory was not created: %v", err)
		}
	})

	t.Run("empty path", func(t *testing.T) {
		t.Parallel()
		if err := ensureWorkdirWritable(""); err == nil {
			t.Fatal("an empty review.workdir must be an error, not a silent cwd")
		}
	})

	// The production failure was `mkdir /work: read-only file system`, which
	// MkdirAll reports. A directory that exists but rejects writes is the other
	// half — a mount owned by another uid — and only the probe file catches it.
	t.Run("existing but not writable", func(t *testing.T) {
		t.Parallel()
		if os.Geteuid() == 0 {
			t.Skip("root ignores the permission bits this case relies on")
		}
		dir := filepath.Join(t.TempDir(), "ro")
		if err := os.Mkdir(dir, 0o555); err != nil {
			t.Fatal(err)
		}
		err := ensureWorkdirWritable(dir)
		if err == nil {
			t.Fatal("a read-only directory must be reported, not accepted")
		}
		if !strings.Contains(err.Error(), "not writable") {
			t.Errorf("error = %q, want it to say the directory is not writable", err)
		}
	})
}

func TestCheckAgentWorkdirGate(t *testing.T) {
	t.Parallel()

	t.Run("agent mode off skips the check entirely", func(t *testing.T) {
		t.Parallel()
		// A path that cannot possibly be created. With agent mode off no mirror is
		// cloned and no worktree is made, so demanding a workdir would stop a
		// perfectly serviceable deployment.
		if err := checkAgentWorkdir(false, "/proc/nonexistent/ai-reviewer"); err != nil {
			t.Errorf("gate fired with agent mode off: %v", err)
		}
	})

	t.Run("agent mode on refuses an unusable workdir", func(t *testing.T) {
		t.Parallel()
		err := checkAgentWorkdir(true, "/proc/nonexistent/ai-reviewer")
		if err == nil {
			t.Fatal("agent mode with an unusable workdir must not start")
		}
		// The message has to name both ways out, or the operator is left guessing
		// which knob to turn.
		for _, want := range []string{"review.workdir", "agent_mode", "diff-only"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %q, want it to mention %q", err, want)
			}
		}
	})

	t.Run("agent mode on accepts a writable workdir", func(t *testing.T) {
		t.Parallel()
		if err := checkAgentWorkdir(true, filepath.Join(t.TempDir(), "work")); err != nil {
			t.Errorf("gate rejected a writable workdir: %v", err)
		}
	})
}
