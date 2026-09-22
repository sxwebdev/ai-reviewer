package app

import (
	"fmt"
	"os"
	"path/filepath"
)

// ensureWorkdirWritable creates review.workdir if it is missing and proves it is
// actually writable by creating and removing a probe file. MkdirAll alone is not
// enough: a read-only mount or a directory owned by another uid fails at the
// first clone rather than at startup.
//
// Shared by `doctor`, which reports the verdict, and `start`, which refuses to
// run without one — so the two can never disagree about what "writable" means.
func ensureWorkdirWritable(dir string) error {
	if dir == "" {
		return fmt.Errorf("review.workdir is empty")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	probe, err := os.CreateTemp(dir, ".doctor-*")
	if err != nil {
		return fmt.Errorf("%s is not writable: %w", filepath.Clean(dir), err)
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	return nil
}

// checkAgentWorkdir is the startup gate. It answers "may this process review
// anything usefully?" and is deliberately not the same question as "is the
// directory writable".
//
// The whole point is that an unusable workdir used to be survivable: EnsureMirror
// failed, prepareWorktree returned no worktree, agent mode silently switched
// itself off, the skeptic pass downgraded to self-reflection, and coverage became
// impossible — every step a WARN or an INFO, none a metric. The service then paid
// full price per review for a diff-only reading of the code, indefinitely. A
// review costs real money; degrading that quietly is worse than not starting.
//
// It is scoped to agent mode because that is the only consumer: with
// llm.claude.agent_mode off no mirror is cloned and no worktree is created, so
// the directory is genuinely unused and demanding it would be theatre.
func checkAgentWorkdir(agentMode bool, dir string) error {
	if !agentMode {
		return nil
	}
	if err := ensureWorkdirWritable(dir); err != nil {
		return fmt.Errorf("review.workdir is unusable and llm.claude.agent_mode is on, so every review "+
			"would silently fall back to a diff-only reading at full price: %w\n"+
			"  set review.workdir (AI_REVIEWER_REVIEW_WORKDIR) to a writable path, "+
			"or turn llm.claude.agent_mode off to accept diff-only reviews", err)
	}
	return nil
}
