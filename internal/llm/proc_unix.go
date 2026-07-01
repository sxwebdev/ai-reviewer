//go:build unix

package llm

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the child in its own process group and, on context
// cancellation/timeout, kills the whole group so tool subprocesses (git/bash in
// agent mode) are terminated with claude rather than orphaned.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
